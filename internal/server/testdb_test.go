package server

import (
	"compress/gzip"
	"encoding/json"
	"flag"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/NC1107/readarr-metadata-provider/internal/dataset"
)

// Fixture ids, shared by every test in the package.
const (
	fxAuthorRothfuss  int64 = 1001
	fxAuthorSanderson int64 = 1002

	fxSeriesKingkiller int64 = 501

	fxWorkNameOfWind  int64 = 2001 // series #1, most popular
	fxWorkWiseMan     int64 = 2002 // series #2, has a translator credit first
	fxWorkNOTWDupe    int64 = 2003 // canonical_id -> fxWorkNameOfWind
	fxWorkElantris    int64 = 2004 // Sanderson
	fxWorkExtra       int64 = 2005 // only present in the "B" fixture
	fxWorkNoEditions  int64 = 2006 // never inserted; used as an unknown id
	fxEditionNOTWHC   int64 = 30001
	fxEditionNOTWKind int64 = 30002
	fxEditionWiseMan  int64 = 30003
	fxEditionDupe     int64 = 30004
	fxEditionElantris int64 = 30005
	fxEditionExtra    int64 = 30006

	fxISBN13NOTW   = "9780756404741"
	fxISBN10NOTW   = "0756404746"
	fxASINNOTW     = "B0010SKUYM"
	fxISBN13Kindle = "9780756405892"
	fxASINWiseMan  = "B004ZZZ111"
)

// TestMain silences the builder's and instrumenter's log lines unless -v
// asks for them, and removes the cached fixture builds at exit.
func TestMain(m *testing.M) {
	flag.Parse()
	if !testing.Verbose() {
		log.SetOutput(io.Discard)
	}
	code := m.Run()
	if fixtureCacheDir != "" {
		os.RemoveAll(fixtureCacheDir)
	}
	os.Exit(code)
}

// The builder is run once per distinct fixture and the resulting file is
// copied for each test, since building (and ANALYZE) dominates test time
// under -race.
var (
	fixtureCacheMu  sync.Mutex
	fixtureCacheDir string
	fixtureCache    = map[string]string{}
)

func cachedFixture(t *testing.T, key string, build func() string) string {
	t.Helper()
	fixtureCacheMu.Lock()
	defer fixtureCacheMu.Unlock()
	if p, ok := fixtureCache[key]; ok {
		return p
	}
	if fixtureCacheDir == "" {
		dir, err := os.MkdirTemp("", "rmp-fixtures-")
		if err != nil {
			t.Fatal(err)
		}
		fixtureCacheDir = dir
	}
	src := build()
	dst := filepath.Join(fixtureCacheDir, key+".db")
	copyFile(t, src, dst)
	fixtureCache[key] = dst
	return dst
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func ptr[T any](v T) *T { return &v }

// fxEdition returns one edition in the JSON shape rawEdition decodes.
func fxEdition(id int64, title, isbn13, isbn10, asin string, formatID int64, editionFormat string, users int64) map[string]any {
	e := map[string]any{
		"id":                  id,
		"title":               title,
		"subtitle":            "",
		"isbn_13":             isbn13,
		"isbn_10":             isbn10,
		"asin":                asin,
		"users_count":         users,
		"release_date":        "2007-03-27",
		"pages":               662,
		"physical_format":     "Hardcover",
		"edition_format":      editionFormat,
		"edition_information": "First edition",
		"reading_format_id":   formatID,
		"publisher":           map[string]any{"name": "DAW Books"},
		"language":            map[string]any{"code2": "en", "code3": "eng", "language": "English"},
		"cached_image":        map[string]any{"url": "https://img.example/" + title + ".jpg"},
	}
	// The builder stores NULL for missing identifiers; mirror that so the
	// partial indexes behave as in production.
	for _, k := range []string{"isbn_13", "isbn_10", "asin"} {
		if e[k] == "" {
			e[k] = nil
		}
	}
	return e
}

// fxBook returns one work in the JSON shape rawBook decodes; the builder
// also reads id/title/ratings_count/contributions/book_series/editions
// from it for the relational tables.
func fxBook(id int64, canonical *int64, title, subtitle, slug string, ratings int64, rating float64,
	contributions []map[string]any, series []map[string]any, editions []map[string]any) map[string]any {
	return map[string]any{
		"id":            id,
		"canonical_id":  canonical,
		"title":         title,
		"subtitle":      subtitle,
		"description":   "About " + title,
		"release_date":  "2007-03-27",
		"slug":          slug,
		"rating":        rating,
		"ratings_count": ratings,
		"users_count":   ratings * 2,
		"updated_at":    "2024-01-01T00:00:00",
		"cached_tags": map[string]any{
			"Genre": []map[string]any{{"tag": "Fantasy", "count": 10}, {"tag": "Fiction", "count": 5}},
		},
		"cached_image":  map[string]any{"url": "https://img.example/" + slug + ".jpg"},
		"contributions": contributions,
		"book_series":   series,
		"editions":      editions,
	}
}

func fxAuthorRow(id int64, name, slug, bio string) map[string]any {
	return map[string]any{
		"id":           id,
		"name":         name,
		"slug":         slug,
		"bio":          bio,
		"updated_at":   "2024-01-01T00:00:00",
		"cached_image": map[string]any{"url": "https://img.example/" + slug + ".jpg"},
	}
}

func author(id int64, role string) map[string]any {
	return map[string]any{"author_id": id, "contribution": role}
}

// fixtureBooks is the standard dataset: two authors, one series, four
// works (one a canonical duplicate), six editions.
func fixtureBooks() []map[string]any {
	return []map[string]any{
		fxBook(fxWorkNameOfWind, nil, "The Name of the Wind", "", "the-name-of-the-wind", 5000, 4.5,
			[]map[string]any{author(fxAuthorRothfuss, "Author")},
			[]map[string]any{{"series_id": fxSeriesKingkiller, "position": 1}},
			[]map[string]any{
				fxEdition(fxEditionNOTWHC, "The Name of the Wind", fxISBN13NOTW, fxISBN10NOTW, fxASINNOTW, formatPhysical, "Hardcover", 900),
				fxEdition(fxEditionNOTWKind, "The Name of the Wind", fxISBN13Kindle, "", "", formatEbook, "Kindle Edition", 300),
			}),
		fxBook(fxWorkWiseMan, nil, "The Wise Man's Fear: The Kingkiller Chronicle Day Two", "The Kingkiller Chronicle Day Two", "the-wise-mans-fear", 3000, 4.4,
			// A translator credited first must not become the primary author.
			[]map[string]any{author(fxAuthorSanderson, "Translator"), author(fxAuthorRothfuss, "Author")},
			[]map[string]any{{"series_id": fxSeriesKingkiller, "position": 2}},
			[]map[string]any{
				fxEdition(fxEditionWiseMan, "The Wise Man's Fear", "9780756407919", "", fxASINWiseMan, formatPhysical, "Paperback", 500),
			}),
		fxBook(fxWorkNOTWDupe, ptr(fxWorkNameOfWind), "The Name of the Wind", "", "the-name-of-the-wind-2", 0, 0,
			[]map[string]any{author(fxAuthorRothfuss, "")},
			nil,
			[]map[string]any{
				fxEdition(fxEditionDupe, "The Name of the Wind", "", "", "", formatPhysical, "", 1),
			}),
		fxBook(fxWorkElantris, nil, "Elantris", "", "elantris", 2000, 4.1,
			[]map[string]any{author(fxAuthorSanderson, "Author")},
			nil,
			[]map[string]any{
				fxEdition(fxEditionElantris, "Elantris", "9780765350374", "", "", formatAudio, "", 400),
			}),
	}
}

func fixtureAuthors() []map[string]any {
	return []map[string]any{
		fxAuthorRow(fxAuthorRothfuss, "Patrick Rothfuss", "patrick-rothfuss", "Author of the Kingkiller Chronicle."),
		fxAuthorRow(fxAuthorSanderson, "Brandon Sanderson", "brandon-sanderson", ""),
	}
}

func fixtureSeries() []map[string]any {
	return []map[string]any{
		{"id": fxSeriesKingkiller, "name": "The Kingkiller Chronicle", "description": "Kvothe's story."},
	}
}

// newFixtureDB builds the standard dataset through the real
// dataset.Builder, so the schema under test is exactly what production
// serves, and returns the path of a private copy the test may rename or
// overwrite.
func newFixtureDB(t *testing.T) string {
	t.Helper()
	src := cachedFixture(t, "standard", func() string {
		return buildFixtureDB(t, fixtureBooks(), fixtureAuthors(), fixtureSeries())
	})
	dst := filepath.Join(t.TempDir(), "metadata.db")
	copyFile(t, src, dst)
	return dst
}

// newFixtureDBExtra is the "B" dataset for swap tests: the standard rows
// plus one extra Rothfuss work.
func newFixtureDBExtra(t *testing.T) string {
	t.Helper()
	src := cachedFixture(t, "extra", func() string { return buildFixtureDBExtra(t) })
	dst := filepath.Join(t.TempDir(), "metadata-b.db")
	copyFile(t, src, dst)
	return dst
}

func buildFixtureDBExtra(t *testing.T) string {
	t.Helper()
	books := append(fixtureBooks(),
		fxBook(fxWorkExtra, nil, "The Slow Regard of Silent Things", "", "the-slow-regard-of-silent-things", 100, 4.0,
			[]map[string]any{author(fxAuthorRothfuss, "Author")},
			[]map[string]any{{"series_id": fxSeriesKingkiller, "position": 2.5}},
			[]map[string]any{
				fxEdition(fxEditionExtra, "The Slow Regard of Silent Things", "9780756410438", "", "", formatPhysical, "Hardcover", 50),
			}))
	return buildFixtureDB(t, books, fixtureAuthors(), fixtureSeries())
}

// buildFixtureDB writes the rows as gzipped JSONL under a temp data dir and
// runs the builder over it.
func buildFixtureDB(t *testing.T, books, authors, series []map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	writeJSONL(t, filepath.Join(dir, "raw", "books", "000.jsonl.gz"), books)
	writeJSONL(t, filepath.Join(dir, "raw", "authors", "000.jsonl.gz"), authors)
	writeJSONL(t, filepath.Join(dir, "raw", "series", "000.jsonl.gz"), series)
	out := filepath.Join(dir, "metadata.db")
	b := &dataset.Builder{DataDir: dir, Out: out}
	if err := b.Build(); err != nil {
		t.Fatalf("building fixture dataset: %v", err)
	}
	return out
}

func writeJSONL(t *testing.T, path string, rows []map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	enc := json.NewEncoder(gz)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

// openFixture opens the standard fixture as a store and closes it with the
// test.
func openFixture(t *testing.T) *store {
	t.Helper()
	st, err := openStore(newFixtureDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.db.Close() })
	return st
}
