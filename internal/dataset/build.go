// Package dataset transforms the raw JSONL layer into the served SQLite
// database: normalized entity tables keyed by Hardcover ids, link tables,
// edition lookup indexes, and an FTS5 search index.
package dataset

import (
	"bufio"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);

CREATE TABLE works (
	id INTEGER PRIMARY KEY,
	title TEXT NOT NULL,
	users_count INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT,
	json TEXT NOT NULL
);

CREATE TABLE authors (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	updated_at TEXT,
	json TEXT NOT NULL
);

CREATE TABLE series (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL,
	json TEXT NOT NULL
);

CREATE TABLE work_authors (
	work_id INTEGER NOT NULL,
	author_id INTEGER NOT NULL,
	role TEXT,
	PRIMARY KEY (work_id, author_id)
) WITHOUT ROWID;

CREATE TABLE work_series (
	work_id INTEGER NOT NULL,
	series_id INTEGER NOT NULL,
	position REAL,
	PRIMARY KEY (work_id, series_id)
) WITHOUT ROWID;

CREATE TABLE editions (
	id INTEGER PRIMARY KEY,
	work_id INTEGER NOT NULL,
	isbn13 TEXT,
	isbn10 TEXT,
	asin TEXT
);

-- Contentless FTS: rowid is the work id, payload lives in works.json.
CREATE VIRTUAL TABLE search USING fts5(title, authors, series, content='');
`

const postIndexes = `
CREATE INDEX idx_work_authors_author ON work_authors (author_id, work_id);
CREATE INDEX idx_work_series_series ON work_series (series_id, work_id);
CREATE INDEX idx_editions_work ON editions (work_id);
CREATE INDEX idx_editions_isbn13 ON editions (isbn13) WHERE isbn13 IS NOT NULL;
CREATE INDEX idx_editions_isbn10 ON editions (isbn10) WHERE isbn10 IS NOT NULL;
CREATE INDEX idx_editions_asin ON editions (asin) WHERE asin IS NOT NULL;
`

type Builder struct {
	DataDir string
	Out     string
}

func (b *Builder) Build() error {
	start := time.Now()
	tmp := b.Out + ".tmp"
	os.Remove(tmp)
	if err := os.MkdirAll(filepath.Dir(b.Out), 0o755); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", tmp)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, pragma := range []string{
		"PRAGMA journal_mode = OFF",
		"PRAGMA synchronous = OFF",
		"PRAGMA cache_size = -262144", // 256MB
		"PRAGMA page_size = 8192",
	} {
		if _, err := db.Exec(pragma); err != nil {
			return err
		}
	}
	if _, err := db.Exec(schema); err != nil {
		return err
	}

	if err := b.loadWorks(db); err != nil {
		return fmt.Errorf("works: %w", err)
	}
	if err := b.loadAuthorsAndSeries(db); err != nil {
		return fmt.Errorf("authors/series: %w", err)
	}
	if err := b.buildSearch(db); err != nil {
		return fmt.Errorf("search: %w", err)
	}

	log.Print("creating indexes")
	if _, err := db.Exec(postIndexes); err != nil {
		return err
	}
	if err := b.writeMeta(db); err != nil {
		return err
	}
	log.Print("analyzing")
	if _, err := db.Exec("ANALYZE"); err != nil {
		return err
	}
	if err := db.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, b.Out); err != nil {
		return err
	}
	log.Printf("dataset built in %s: %s", time.Since(start).Round(time.Second), b.Out)
	return nil
}

// workRow is the subset of a raw book row the relational layer needs; the
// full row is stored verbatim in works.json.
type workRow struct {
	ID            int64   `json:"id"`
	Title         string  `json:"title"`
	UsersCount    int64   `json:"users_count"`
	UpdatedAt     string  `json:"updated_at"`
	Contributions []struct {
		AuthorID     *int64  `json:"author_id"`
		Contribution *string `json:"contribution"`
	} `json:"contributions"`
	BookSeries []struct {
		SeriesID *int64   `json:"series_id"`
		Position *float64 `json:"position"`
	} `json:"book_series"`
	Editions []struct {
		ID     int64   `json:"id"`
		ISBN13 *string `json:"isbn_13"`
		ISBN10 *string `json:"isbn_10"`
		ASIN   *string `json:"asin"`
	} `json:"editions"`
}

func (b *Builder) loadWorks(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Later files (deltas) overwrite earlier rows for the same id.
	insWork, _ := tx.Prepare(`INSERT INTO works (id, title, users_count, updated_at, json) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET title=excluded.title, users_count=excluded.users_count, updated_at=excluded.updated_at, json=excluded.json`)
	insWA, _ := tx.Prepare(`INSERT OR REPLACE INTO work_authors (work_id, author_id, role) VALUES (?, ?, ?)`)
	insWS, _ := tx.Prepare(`INSERT OR REPLACE INTO work_series (work_id, series_id, position) VALUES (?, ?, ?)`)
	insEd, _ := tx.Prepare(`INSERT OR REPLACE INTO editions (id, work_id, isbn13, isbn10, asin) VALUES (?, ?, ?, ?, ?)`)

	n := 0
	err = b.walkJSONL([]string{"books", "books-delta"}, func(line []byte) error {
		var w workRow
		if err := json.Unmarshal(line, &w); err != nil {
			return err
		}
		if _, err := insWork.Exec(w.ID, w.Title, w.UsersCount, w.UpdatedAt, string(line)); err != nil {
			return err
		}
		for _, c := range w.Contributions {
			if c.AuthorID == nil {
				continue
			}
			if _, err := insWA.Exec(w.ID, *c.AuthorID, c.Contribution); err != nil {
				return err
			}
		}
		for _, s := range w.BookSeries {
			if s.SeriesID == nil {
				continue
			}
			if _, err := insWS.Exec(w.ID, *s.SeriesID, s.Position); err != nil {
				return err
			}
		}
		for _, e := range w.Editions {
			if _, err := insEd.Exec(e.ID, w.ID, e.ISBN13, e.ISBN10, e.ASIN); err != nil {
				return err
			}
		}
		n++
		if n%500000 == 0 {
			log.Printf("works: %d rows", n)
		}
		return nil
	})
	if err != nil {
		return err
	}
	log.Printf("works: %d rows loaded", n)
	return tx.Commit()
}

type linkRow struct {
	Contributions []struct {
		Author json.RawMessage `json:"author"`
	} `json:"contributions"`
	BookSeries []struct {
		Series json.RawMessage `json:"series"`
	} `json:"book_series"`
}

type authorKey struct {
	ID        *int64 `json:"id"`
	Name      string `json:"name"`
	UpdatedAt string `json:"updated_at"`
}

type seriesKey struct {
	ID   *int64 `json:"id"`
	Name string `json:"name"`
}

// loadAuthorsAndSeries merges author and series snapshots from the
// book_links sweep with the (partial) direct sweeps. For authors the row
// with the newest updated_at wins; series snapshots carry no timestamp, so
// the last one seen wins.
func (b *Builder) loadAuthorsAndSeries(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	insAuthor, _ := tx.Prepare(`INSERT INTO authors (id, name, updated_at, json) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, updated_at=excluded.updated_at, json=excluded.json
		WHERE excluded.updated_at >= authors.updated_at`)
	insSeries, _ := tx.Prepare(`INSERT OR REPLACE INTO series (id, name, json) VALUES (?, ?, ?)`)

	putAuthor := func(raw json.RawMessage) error {
		var k authorKey
		if err := json.Unmarshal(raw, &k); err != nil || k.ID == nil {
			return err
		}
		_, err := insAuthor.Exec(*k.ID, k.Name, k.UpdatedAt, string(raw))
		return err
	}
	putSeries := func(raw json.RawMessage) error {
		var k seriesKey
		if err := json.Unmarshal(raw, &k); err != nil || k.ID == nil {
			return err
		}
		_, err := insSeries.Exec(*k.ID, k.Name, string(raw))
		return err
	}

	n := 0
	err = b.walkJSONL([]string{"book_links", "book_links-delta"}, func(line []byte) error {
		var l linkRow
		if err := json.Unmarshal(line, &l); err != nil {
			return err
		}
		for _, c := range l.Contributions {
			if len(c.Author) > 0 && string(c.Author) != "null" {
				if err := putAuthor(c.Author); err != nil {
					return err
				}
			}
		}
		for _, s := range l.BookSeries {
			if len(s.Series) > 0 && string(s.Series) != "null" {
				if err := putSeries(s.Series); err != nil {
					return err
				}
			}
		}
		n++
		if n%500000 == 0 {
			log.Printf("book_links: %d rows", n)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := b.walkJSONL([]string{"authors", "authors-delta"}, func(line []byte) error {
		return putAuthor(line)
	}); err != nil {
		return err
	}
	if err := b.walkJSONL([]string{"series"}, func(line []byte) error {
		return putSeries(line)
	}); err != nil {
		return err
	}
	log.Printf("book_links: %d rows merged", n)
	return tx.Commit()
}

// buildSearch fills the FTS index: one document per work, carrying its
// title plus the names of its authors and series so a single query matches
// any of them.
func (b *Builder) buildSearch(db *sql.DB) error {
	log.Print("building search index")
	_, err := db.Exec(`
		INSERT INTO search (rowid, title, authors, series)
		SELECT w.id,
			w.title,
			COALESCE((SELECT group_concat(a.name, ', ') FROM work_authors wa JOIN authors a ON a.id = wa.author_id WHERE wa.work_id = w.id), ''),
			COALESCE((SELECT group_concat(s.name, ', ') FROM work_series ws JOIN series s ON s.id = ws.series_id WHERE ws.work_id = w.id), '')
		FROM works w`)
	return err
}

func (b *Builder) writeMeta(db *sql.DB) error {
	_, err := db.Exec(`INSERT INTO meta (key, value) VALUES
		('generated_at', ?),
		('source', 'hardcover'),
		('schema_version', '1')`,
		time.Now().UTC().Format(time.RFC3339))
	return err
}

// walkJSONL streams every line of every .jsonl.gz file under the given
// raw subdirectories, in filename order. Missing directories are skipped.
func (b *Builder) walkJSONL(subdirs []string, fn func(line []byte) error) error {
	for _, sub := range subdirs {
		dir := filepath.Join(b.DataDir, "raw", sub)
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		var names []string
		for _, e := range entries {
			if filepath.Ext(e.Name()) == ".gz" {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			if err := b.walkFile(filepath.Join(dir, name), fn); err != nil {
				return fmt.Errorf("%s/%s: %w", sub, name, err)
			}
		}
	}
	return nil
}

func (b *Builder) walkFile(path string, fn func(line []byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		if err := fn(sc.Bytes()); err != nil {
			return err
		}
	}
	return sc.Err()
}
