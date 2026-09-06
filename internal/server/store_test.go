package server

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFTSQuery(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"wind", `"wind"*`},
		{"name of the wind", `"name" "of" "the" "wind"*`},
		{`say "hi"`, `"say" """hi"""*`},
		// Control characters become separators; a NUL must never reach FTS5.
		{"\x00", ""},
		{"a\x00b", `"a" "b"*`},
		{"tab\tsep\x7f", `"tab" "sep"*`},
		{"  lead trail  ", `"lead" "trail"*`},
		// FTS operators are neutralised by quoting.
		{"AND OR NOT", `"AND" "OR" "NOT"*`},
		{"foo* (bar)", `"foo*" "(bar)"*`},
	}
	for _, c := range cases {
		if got := ftsQuery(c.in); got != c.want {
			t.Errorf("ftsQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLatinScript(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"Misérables", true},
		{"the name of the wind", true},
		{"", true},
		{"123 !?", true},
		{"Война", false},
		{"三体", false},
		{"harry potter و", false},
	}
	for _, c := range cases {
		if got := latinScript(c.in); got != c.want {
			t.Errorf("latinScript(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestOpenStoreRejectsBadFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if st, err := openStore(empty); err == nil {
		st.db.Close()
		t.Error("openStore accepted a zero-byte file")
	}

	garbage := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(garbage, []byte("this is not a sqlite database at all, just text\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, err := openStore(garbage); err == nil {
		st.db.Close()
		t.Error("openStore accepted a non-SQLite file")
	}

	partial := filepath.Join(dir, "partial.db")
	db, err := sql.Open("sqlite", partial)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE works (id INTEGER PRIMARY KEY, json TEXT); CREATE TABLE authors (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st, err := openStore(partial)
	if err == nil {
		st.db.Close()
		t.Fatal("openStore accepted a SQLite file lacking the dataset tables")
	}
	if !strings.Contains(err.Error(), "missing table") {
		t.Errorf("error = %q, want it to name the missing table", err)
	}

	if _, err := openStore(filepath.Join(dir, "does-not-exist.db")); err == nil {
		t.Error("openStore accepted a missing file")
	}
}

func TestOpenStoreAndProbeFixture(t *testing.T) {
	t.Parallel()
	st := openFixture(t)
	if !st.hasNormName {
		t.Error("fixture built by dataset.Builder should carry norm_name")
	}
	if err := st.probe(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := Validate(newFixtureDB(t)); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestProbeFailsOnDatasetWithoutWorks(t *testing.T) {
	t.Parallel()
	path := buildFixtureDB(t, nil, fixtureAuthors(), fixtureSeries())
	st, err := openStore(path)
	if err != nil {
		t.Fatalf("openStore should accept an empty-but-complete schema: %v", err)
	}
	defer st.db.Close()
	if err := st.probe(context.Background()); err == nil || !strings.Contains(err.Error(), "no works") {
		t.Errorf("probe error = %v, want 'dataset has no works'", err)
	}
}

func TestStoreLookups(t *testing.T) {
	t.Parallel()
	st := openFixture(t)

	b, err := st.work(fxWorkNameOfWind)
	if err != nil {
		t.Fatal(err)
	}
	if b.Title != "The Name of the Wind" || len(b.Editions) != 2 || len(b.CachedTags.Genres) != 2 {
		t.Errorf("work decoded oddly: title=%q editions=%d genres=%v", b.Title, len(b.Editions), b.CachedTags.Genres)
	}
	if b.Editions[0].Language == nil || b.Editions[0].Language.Code3 != "eng" {
		t.Errorf("edition language not decoded: %+v", b.Editions[0].Language)
	}
	if _, err := st.work(fxWorkNoEditions); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown work error = %v, want errNotFound", err)
	}

	a, err := st.author(fxAuthorRothfuss)
	if err != nil || a.Name != "Patrick Rothfuss" {
		t.Errorf("author = %+v, %v", a, err)
	}
	sr, err := st.series(fxSeriesKingkiller)
	if err != nil || sr.Name != "The Kingkiller Chronicle" {
		t.Errorf("series = %+v, %v", sr, err)
	}

	if id, err := st.workIDForEdition(fxEditionNOTWKind); err != nil || id != fxWorkNameOfWind {
		t.Errorf("workIDForEdition = %d, %v", id, err)
	}
	if _, err := st.workIDForEdition(99); err == nil {
		t.Error("workIDForEdition(99) should fail")
	}
	if id, err := st.editionByISBN(fxISBN13NOTW); err != nil || id != fxEditionNOTWHC {
		t.Errorf("editionByISBN(13) = %d, %v", id, err)
	}
	if id, err := st.editionByISBN(fxISBN10NOTW); err != nil || id != fxEditionNOTWHC {
		t.Errorf("editionByISBN(10) = %d, %v", id, err)
	}
	if _, err := st.editionByISBN("9999999999999"); err == nil {
		t.Error("unknown isbn should fail")
	}
	if id, err := st.editionByASIN(fxASINWiseMan); err != nil || id != fxEditionWiseMan {
		t.Errorf("editionByASIN = %d, %v", id, err)
	}

	ids, err := st.authorWorkIDs(fxAuthorRothfuss, 10)
	if err != nil {
		t.Fatal(err)
	}
	// Most popular first; the translator-credited work is still linked here
	// (the role filter lives in authorResource).
	if want := []int64{fxWorkNameOfWind, fxWorkWiseMan, fxWorkNOTWDupe}; !equalIDs(ids, want) {
		t.Errorf("authorWorkIDs = %v, want %v", ids, want)
	}
	links, err := st.seriesWorks(fxSeriesKingkiller, 10)
	if err != nil || len(links) != 2 || links[0].WorkID != fxWorkNameOfWind || links[1].Position == nil || *links[1].Position != 2 {
		t.Errorf("seriesWorks = %+v, %v", links, err)
	}
	top, err := st.topWorkIDs(2, 0)
	if err != nil || !equalIDs(top, []int64{fxWorkNameOfWind, fxWorkWiseMan}) {
		t.Errorf("topWorkIDs = %v, %v", top, err)
	}
	count, avg := st.authorAggregates(fxAuthorSanderson)
	// Sanderson is linked to Elantris (2000 @ 4.1) and, as translator, to
	// Wise Man's Fear (3000 @ 4.4); the aggregate deliberately ignores role.
	if count != 5000 || avg < 4.27 || avg > 4.29 {
		t.Errorf("authorAggregates = %d, %v", count, avg)
	}
}

func TestSearchWorks(t *testing.T) {
	t.Parallel()
	st := openFixture(t)
	cases := []struct {
		query    string
		first    int64   // expected top hit
		all      []int64 // exact expected list
		contains int64   // must appear somewhere
	}{
		// Plain title: FTS, popularity-blended, canonical above the dupe.
		{query: "name of the wind", first: fxWorkNameOfWind},
		{query: "the name of the wi", first: fxWorkNameOfWind},
		// Exact author name (modulo case/punctuation): that author's works.
		{query: "PATRICK ROTHFUSS", all: []int64{fxWorkNameOfWind, fxWorkWiseMan, fxWorkNOTWDupe}},
		// Only authorship credits count for an author-name query: Sanderson's
		// translator credit on Wise Man's Fear (3000 ratings) must not put
		// Rothfuss's book above, or even beside, his own Elantris.
		{query: "brandon sanderson", all: []int64{fxWorkElantris}},
		// Series + position resolves through work_series.
		{query: "kingkiller chronicle 2", first: fxWorkWiseMan},
		{query: "Kingkiller Chronicle book 1", first: fxWorkNameOfWind},
		// Misspelled author falls back to fuzzy matching.
		{query: "patrik rothfus", first: fxWorkNameOfWind},
		// Nothing matches anything.
		{query: "zzzzqqqq", all: []int64{}},
	}
	for _, c := range cases {
		ids, err := st.searchWorks(c.query, 30)
		if err != nil {
			t.Errorf("searchWorks(%q): %v", c.query, err)
			continue
		}
		if c.all != nil {
			if !equalIDs(ids, c.all) {
				t.Errorf("searchWorks(%q) = %v, want %v", c.query, ids, c.all)
			}
			continue
		}
		if c.contains != 0 {
			found := false
			for _, id := range ids {
				found = found || id == c.contains
			}
			if !found {
				t.Errorf("searchWorks(%q) = %v, want it to contain %d", c.query, ids, c.contains)
			}
			continue
		}
		if len(ids) == 0 || ids[0] != c.first {
			t.Errorf("searchWorks(%q) = %v, want first %d", c.query, ids, c.first)
		}
	}
}

func TestSearchWorksSurvivesHostileInput(t *testing.T) {
	t.Parallel()
	st := openFixture(t)
	for _, q := range []string{"\x00", "a\x00b", `"`, `"""`, "*", "(", ")", "NEAR(", "col:x", "-", "\"unterminated", strings.Repeat("x ", 500)} {
		if _, err := st.searchWorks(q, 10); err != nil {
			t.Errorf("searchWorks(%q) errored: %v", q, err)
		}
	}
}

func TestSeriesPositionWorks(t *testing.T) {
	t.Parallel()
	st := openFixture(t)
	if ids := st.seriesPositionWorks("kingkiller chronicle 2"); !equalIDs(ids, []int64{fxWorkWiseMan}) {
		t.Errorf("got %v", ids)
	}
	if ids := st.seriesPositionWorks("kingkiller chronicle 9"); len(ids) != 0 {
		t.Errorf("position 9 should not exist: %v", ids)
	}
	// Short fragments and non-numeric tails are ignored.
	if ids := st.seriesPositionWorks("kin 2"); len(ids) != 0 {
		t.Errorf("short fragment matched: %v", ids)
	}
	if ids := st.seriesPositionWorks("kingkiller chronicle"); len(ids) != 0 {
		t.Errorf("no number matched: %v", ids)
	}
}

func TestFuzzyHelpers(t *testing.T) {
	t.Parallel()
	q := bigrams("rothfuss")
	if diceScore(q, "rothfuss") != 1 {
		t.Error("identical strings should score 1")
	}
	if s := diceScore(q, "rothfus"); s < 0.9 {
		t.Errorf("one-char deletion scored %v", s)
	}
	if s := diceScore(q, "sanderson"); s > 0.3 {
		t.Errorf("unrelated name scored %v", s)
	}
	if diceScore(q, "") != 0 || len(bigrams("a")) != 0 {
		t.Error("degenerate inputs")
	}
	if firstID(nil) != 0 || firstID([]int64{7, 8}) != 7 {
		t.Error("firstID")
	}
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
