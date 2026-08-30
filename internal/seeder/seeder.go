// Package seeder bulk-downloads Hardcover entities into raw JSONL files.
//
// The raw layer is deliberately dumb storage: one gzipped JSONL file per
// request, one entity row per line, exactly as returned by the API. Schema
// or transform changes in later phases never require refetching.
package seeder

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/NC1107/readarr-metadata-provider/internal/hcapi"
)

type State struct {
	MaxID     int64        `json:"max_id"`
	NextStart int64        `json:"next_start"`
	SeededAt  string       `json:"seeded_at,omitempty"`
	Delta     *DeltaCursor `json:"delta_cursor,omitempty"`
}

type DeltaCursor struct {
	UpdatedAt string `json:"updated_at"`
	ID        int64  `json:"id"`
}

type Seeder struct {
	Client  *hcapi.Client
	DataDir string
}

// Seed runs (or resumes) a full id-range download of one entity.
func (s *Seeder) Seed(ctx context.Context, spec EntitySpec) error {
	state, err := s.loadState(spec.Name)
	if err != nil {
		return err
	}
	if state.MaxID == 0 {
		state.MaxID, err = s.discoverMaxID(ctx, spec)
		if err != nil {
			return err
		}
		state.NextStart = 0
		log.Printf("%s: max id %d", spec.Name, state.MaxID)
	}
	if state.NextStart > state.MaxID {
		log.Printf("%s: already seeded", spec.Name)
		return nil
	}

	rawDir := filepath.Join(s.DataDir, "raw", spec.Name)
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		return err
	}

	span := spec.RangeWidth * aliasesPerRequest
	requests := 0
	rows := 0
	for state.NextStart <= state.MaxID {
		start := state.NextStart
		end := start + span
		batch, err := s.fetchRange(ctx, spec, start, end)
		if err != nil {
			s.saveState(spec.Name, state)
			return fmt.Errorf("%s range [%d,%d): %w", spec.Name, start, end, err)
		}
		if len(batch) > 0 {
			file := filepath.Join(rawDir, fmt.Sprintf("%09d-%09d.jsonl.gz", start, end))
			if err := writeJSONL(file, batch); err != nil {
				s.saveState(spec.Name, state)
				return err
			}
		}
		state.NextStart = end
		if err := s.saveState(spec.Name, state); err != nil {
			return err
		}
		requests++
		rows += len(batch)
		if requests%25 == 0 {
			pct := float64(state.NextStart) / float64(state.MaxID) * 100
			log.Printf("%s: %.1f%% (%d rows, daily remaining %d)",
				spec.Name, min(pct, 100), rows, s.Client.DailyRemaining())
		}
	}

	state.SeededAt = time.Now().UTC().Format(time.RFC3339)
	if err := s.saveState(spec.Name, state); err != nil {
		return err
	}
	log.Printf("%s: seed complete (%d rows this run)", spec.Name, rows)
	return nil
}

// Delta downloads rows changed since the stored cursor, using a compound
// (updated_at, id) cursor so equal timestamps cannot skip rows.
func (s *Seeder) Delta(ctx context.Context, spec EntitySpec) error {
	if !spec.HasUpdated {
		return fmt.Errorf("%s has no updated_at; re-seed instead", spec.Name)
	}
	state, err := s.loadState(spec.Name)
	if err != nil {
		return err
	}
	cursor := state.Delta
	if cursor == nil {
		if state.SeededAt == "" {
			return fmt.Errorf("%s: no completed seed to delta from", spec.Name)
		}
		cursor = &DeltaCursor{UpdatedAt: state.SeededAt}
	}

	rawDir := filepath.Join(s.DataDir, "raw", spec.Name+"-delta")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		return err
	}

	query := fmt.Sprintf(`query ($ts: timestamptz!, $id: Int!) {
		rows: %s(
			where: {_or: [
				{updated_at: {_gt: $ts}},
				{_and: [{updated_at: {_eq: $ts}}, {id: {_gt: $id}}]}
			]},
			order_by: [{updated_at: asc}, {id: asc}],
			limit: %d
		) { %s }
	}`, spec.root(), spec.RowCap, spec.Fields)

	total := 0
	for {
		data, err := s.Client.Query(ctx, query, map[string]any{
			"ts": cursor.UpdatedAt,
			"id": cursor.ID,
		})
		if err != nil {
			return err
		}
		var out struct {
			Rows []json.RawMessage `json:"rows"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return err
		}
		if len(out.Rows) == 0 {
			break
		}

		file := filepath.Join(rawDir, time.Now().UTC().Format("20060102T150405.000000")+".jsonl.gz")
		if err := writeJSONL(file, out.Rows); err != nil {
			return err
		}
		last := out.Rows[len(out.Rows)-1]
		var key struct {
			ID        int64  `json:"id"`
			UpdatedAt string `json:"updated_at"`
		}
		if err := json.Unmarshal(last, &key); err != nil {
			return err
		}
		cursor = &DeltaCursor{UpdatedAt: key.UpdatedAt, ID: key.ID}
		state.Delta = cursor
		if err := s.saveState(spec.Name, state); err != nil {
			return err
		}
		total += len(out.Rows)
		if int64(len(out.Rows)) < spec.RowCap {
			break
		}
	}
	log.Printf("%s: delta complete (%d changed rows)", spec.Name, total)
	return nil
}

func (s *Seeder) fetchRange(ctx context.Context, spec EntitySpec, start, end int64) ([]json.RawMessage, error) {
	var q string
	for i := int64(0); i < aliasesPerRequest; i++ {
		lo := start + i*spec.RangeWidth
		hi := lo + spec.RangeWidth
		if lo > end {
			break
		}
		q += fmt.Sprintf("r%d: %s(where: {id: {_gte: %d, _lt: %d}}, limit: %d) { %s }\n",
			i, spec.root(), lo, hi, spec.RangeWidth, spec.Fields)
	}
	data, err := s.Client.Query(ctx, "{\n"+q+"}", nil)
	if err != nil {
		return nil, err
	}
	var aliases map[string][]json.RawMessage
	if err := json.Unmarshal(data, &aliases); err != nil {
		return nil, err
	}
	var rows []json.RawMessage
	for i := int64(0); i < aliasesPerRequest; i++ {
		rows = append(rows, aliases[fmt.Sprintf("r%d", i)]...)
	}
	return rows, nil
}

func (s *Seeder) discoverMaxID(ctx context.Context, spec EntitySpec) (int64, error) {
	q := fmt.Sprintf("{ rows: %s(limit: 1, order_by: {id: desc}) { id } }", spec.root())
	data, err := s.Client.Query(ctx, q, nil)
	if err != nil {
		return 0, err
	}
	var out struct {
		Rows []struct {
			ID int64 `json:"id"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, err
	}
	if len(out.Rows) == 0 {
		return 0, fmt.Errorf("%s: no rows found", spec.Name)
	}
	return out.Rows[0].ID, nil
}

func (s *Seeder) statePath(entity string) string {
	return filepath.Join(s.DataDir, "state", entity+".json")
}

func (s *Seeder) loadState(entity string) (*State, error) {
	b, err := os.ReadFile(s.statePath(entity))
	if errors.Is(err, os.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("corrupt state file %s: %w", s.statePath(entity), err)
	}
	return &st, nil
}

func (s *Seeder) saveState(entity string, st *State) error {
	path := s.statePath(entity)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func writeJSONL(path string, rows []json.RawMessage) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	gz, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	for _, row := range rows {
		if _, err := gz.Write(append(row, '\n')); err != nil {
			f.Close()
			os.Remove(tmp)
			return err
		}
	}
	if err := gz.Close(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
