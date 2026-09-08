package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// newTestServer serves the standard fixture from a path the test owns, so
// swap tests can rename other files over it.
func newTestServer(t *testing.T, cfg Config) (*Server, http.Handler, string) {
	t.Helper()
	if cfg.DBPath == "" {
		src := newFixtureDB(t)
		cfg.DBPath = filepath.Join(t.TempDir(), "served.db")
		if err := os.Rename(src, cfg.DBPath); err != nil {
			t.Fatal(err)
		}
	}
	if cfg.MaxWorks == 0 {
		cfg.MaxWorks = 100
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, s.Handler(), cfg.DBPath
}

func do(t *testing.T, h http.Handler, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, h, http.MethodGet, target, nil)
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, code int) {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, code, rec.Body.String())
	}
}

func TestInfoRoute(t *testing.T) {
	t.Parallel()
	_, h, _ := newTestServer(t, Config{Version: "1.2.3"})
	rec := get(t, h, "/")
	wantStatus(t, rec, 200)
	var info map[string]any
	decode(t, rec, &info)
	if info["service"] != "readarr-metadata-provider" || info["version"] != "1.2.3" {
		t.Errorf("info = %v", info)
	}
	if _, ok := info["dataset"]; !ok {
		t.Error("info lacks dataset")
	}
	// Anything else at the root is not the info route.
	if rec := get(t, h, "/nope"); rec.Code != 404 {
		t.Errorf("GET /nope = %d, want 404", rec.Code)
	}
}

func TestDefaultVersion(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestServer(t, Config{})
	if s.version != "0.0.0-dev" {
		t.Errorf("version = %q", s.version)
	}
}

func TestHealthzAndFailedSwapKeepsOldDataset(t *testing.T) {
	t.Parallel()
	s, h, path := newTestServer(t, Config{})
	rec := get(t, h, "/healthz")
	wantStatus(t, rec, 200)
	var st map[string]string
	decode(t, rec, &st)
	if st["status"] != "ok" {
		t.Errorf("health = %v", st)
	}

	// The updater installs by rename; do the same with an empty file.
	empty := filepath.Join(filepath.Dir(path), "empty.db")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(empty, path); err != nil {
		t.Fatal(err)
	}
	if err := s.Swap(path); err == nil {
		t.Fatal("Swap accepted an empty file")
	}
	if rec := get(t, h, "/healthz"); rec.Code != 200 {
		t.Errorf("after failed swap /healthz = %d, want 200 (old dataset must keep serving); body %s", rec.Code, rec.Body.String())
	}
	if rec := get(t, h, fmt.Sprintf("/work/%d", fxWorkNameOfWind)); rec.Code != 200 {
		t.Errorf("after failed swap /work = %d", rec.Code)
	}

	// A dataset whose probe fails (tables but no works) is refused too.
	noWorks := buildFixtureDB(t, nil, fixtureAuthors(), fixtureSeries())
	if err := s.Swap(noWorks); err == nil {
		t.Error("Swap accepted a dataset with no works")
	}
	if rec := get(t, h, "/healthz"); rec.Code != 200 {
		t.Errorf("/healthz = %d after refused swap", rec.Code)
	}
}

func TestHealthzUnhealthy(t *testing.T) {
	t.Parallel()
	// Point the live app at a store whose only table set is present but
	// empty: the probe must report 503 with a JSON body.
	s, h, _ := newTestServer(t, Config{})
	st, err := openStore(buildFixtureDB(t, nil, fixtureAuthors(), fixtureSeries()))
	if err != nil {
		t.Fatal(err)
	}
	defer st.db.Close()
	s.app.Store(&app{store: st})
	rec := get(t, h, "/healthz")
	wantStatus(t, rec, http.StatusServiceUnavailable)
	var body map[string]string
	decode(t, rec, &body)
	if body["status"] != "unhealthy" || body["error"] == "" {
		t.Errorf("body = %v", body)
	}
}

func TestSearch(t *testing.T) {
	t.Parallel()
	_, h, _ := newTestServer(t, Config{})

	rec := get(t, h, "/search?q="+url.QueryEscape("the name of the wind"))
	wantStatus(t, rec, 200)
	var raw []map[string]any
	decode(t, rec, &raw)
	if len(raw) == 0 {
		t.Fatal("no results")
	}
	// Field casing is part of the contract (rreading-glasses search shape).
	first := raw[0]
	for _, k := range []string{"bookId", "workId", "author"} {
		if _, ok := first[k]; !ok {
			t.Errorf("result lacks %q: %v", k, first)
		}
	}
	if a, _ := first["author"].(map[string]any); a["id"] == nil {
		t.Errorf("author lacks id: %v", first["author"])
	}
	var results []searchResource
	decode(t, rec, &results)
	if results[0].WorkID != fxWorkNameOfWind || results[0].BookID != fxEditionNOTWHC || results[0].Author.ID != fxAuthorRothfuss {
		t.Errorf("first result = %+v", results[0])
	}

	// Identifier queries resolve directly.
	for _, q := range []string{fxISBN13NOTW, "978-0-7564-0474-1", fxISBN10NOTW, fxASINNOTW} {
		rec := get(t, h, "/search?q="+url.QueryEscape(q))
		wantStatus(t, rec, 200)
		var rs []searchResource
		decode(t, rec, &rs)
		if len(rs) != 1 || rs[0].WorkID != fxWorkNameOfWind {
			t.Errorf("search %q = %+v", q, rs)
		}
	}

	// No match is an empty list, never null.
	for _, q := range []string{"zzzzqqqq", "", "%00", "%00%00", "9999999999999", "B000000000"} {
		rec := get(t, h, "/search?q="+q)
		if rec.Code != 200 {
			t.Errorf("search %q = %d: %s", q, rec.Code, rec.Body.String())
			continue
		}
		if body := strings.TrimSpace(rec.Body.String()); body != "[]" {
			t.Errorf("search %q body = %s, want []", q, body)
		}
	}

	if rec := do(t, h, http.MethodDelete, "/search?q=x", nil); rec.Code != 200 {
		t.Errorf("DELETE /search = %d", rec.Code)
	}
}

func TestSearchMinRatings(t *testing.T) {
	t.Parallel()
	_, h, _ := newTestServer(t, Config{MinRatings: 10})
	// Rothfuss' works include the zero-rating duplicate; it must be dropped
	// when better results exist.
	rec := get(t, h, "/search?q=patrick+rothfuss")
	var rs []searchResource
	decode(t, rec, &rs)
	for _, r := range rs {
		if r.WorkID == fxWorkNOTWDupe {
			t.Errorf("duplicate with 0 ratings survived the cutoff: %+v", rs)
		}
	}
	if len(rs) != 2 {
		t.Errorf("got %d results, want 2", len(rs))
	}
}

func TestSearchLanguageFilter(t *testing.T) {
	t.Parallel()
	_, h, _ := newTestServer(t, Config{SearchLangs: "de, fr"})
	// Every fixture edition is English, so a Latin query finds nothing...
	rec := get(t, h, "/search?q=elantris")
	var rs []searchResource
	decode(t, rec, &rs)
	if len(rs) != 0 {
		t.Errorf("english-only works should be filtered: %+v", rs)
	}
	// ...but a non-Latin query skips the filter.
	s, _, _ := newTestServer(t, Config{SearchLangs: "de"})
	b := &rawBook{Title: "Elantris", Editions: []rawEdition{{}}}
	if !s.langAllowed(b, "Война") {
		t.Error("non-Latin query must bypass the filter")
	}
	if !s.langAllowed(b, "elantris") {
		t.Error("work with no recorded language and a Latin title passes")
	}
	b.Title = "Война и мир"
	if s.langAllowed(b, "war and peace") {
		t.Error("non-Latin title with no language should be dropped for a Latin query")
	}
}

func TestGetWork(t *testing.T) {
	t.Parallel()
	_, h, _ := newTestServer(t, Config{})

	rec := get(t, h, fmt.Sprintf("/work/%d", fxWorkNameOfWind))
	wantStatus(t, rec, 200)
	var raw map[string]any
	decode(t, rec, &raw)
	if raw["ForeignId"] != float64(fxWorkNameOfWind) {
		t.Errorf("ForeignId = %v", raw["ForeignId"])
	}
	var w workResource
	decode(t, rec, &w)
	if len(w.Books) != 1 || w.Books[0].ForeignID != fxEditionNOTWHC || len(w.Authors) != 1 || len(w.Series) != 1 {
		t.Errorf("work = %+v", w)
	}

	// Duplicate: the canonical work is served in its place.
	rec = get(t, h, fmt.Sprintf("/work/%d", fxWorkNOTWDupe))
	wantStatus(t, rec, 200)
	decode(t, rec, &w)
	if w.ForeignID != fxWorkNameOfWind {
		t.Errorf("canonical redirect served %d, want %d", w.ForeignID, fxWorkNameOfWind)
	}

	for _, p := range []string{"/work/999999", "/work/0", "/work/-1", "/work/abc", "/work/1.5"} {
		if rec := get(t, h, p); rec.Code != 404 {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
	if rec := do(t, h, http.MethodDelete, "/work/1", nil); rec.Code != 200 {
		t.Errorf("DELETE /work = %d", rec.Code)
	}
}

func TestGetBookRedirectsToScopedAuthor(t *testing.T) {
	t.Parallel()
	_, h, _ := newTestServer(t, Config{})

	rec := get(t, h, fmt.Sprintf("/book/%d", fxEditionNOTWKind))
	wantStatus(t, rec, http.StatusSeeOther)
	loc := rec.Header().Get("Location")
	if want := fmt.Sprintf("/author/%d?edition=%d", fxAuthorRothfuss, fxEditionNOTWKind); loc != want {
		t.Fatalf("Location = %q, want %q", loc, want)
	}

	rec = get(t, h, loc)
	wantStatus(t, rec, 200)
	var a authorResource
	decode(t, rec, &a)
	if a.ForeignID != fxAuthorRothfuss || len(a.Works) != 1 {
		t.Fatalf("scoped author = id %d, %d works", a.ForeignID, len(a.Works))
	}
	if a.Works[0].ForeignID != fxWorkNameOfWind || len(a.Works[0].Books) != 1 || a.Works[0].Books[0].ForeignID != fxEditionNOTWKind {
		t.Errorf("scoped work = %+v", a.Works[0])
	}
	if a.RatingCount == 0 || a.AverageRating == 0 {
		t.Errorf("aggregates not filled: %d %v", a.RatingCount, a.AverageRating)
	}

	for _, p := range []string{"/book/999999", "/book/abc", "/book/0", "/author/1001?edition=abc", "/author/1001?edition=999999"} {
		if rec := get(t, h, p); rec.Code != 404 {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
	if rec := do(t, h, http.MethodDelete, "/book/1", nil); rec.Code != 200 {
		t.Errorf("DELETE /book = %d", rec.Code)
	}
}

func TestISBNAndASINRoutes(t *testing.T) {
	t.Parallel()
	_, h, _ := newTestServer(t, Config{})

	for _, p := range []string{"/book/isbn/" + fxISBN13NOTW, "/book/isbn/978-0-7564-0474-1", "/book/isbn/" + fxISBN10NOTW} {
		rec := get(t, h, p)
		if rec.Code != http.StatusSeeOther {
			t.Errorf("GET %s = %d, want 303", p, rec.Code)
			continue
		}
		if loc := rec.Header().Get("Location"); loc != fmt.Sprintf("/book/%d", fxEditionNOTWHC) {
			t.Errorf("GET %s Location = %q", p, loc)
		}
	}
	for _, p := range []string{"/book/isbn/12345", "/book/isbn/abcdefghijklm", "/book/isbn/9999999999999", "/book/isbn/"} {
		if rec := get(t, h, p); rec.Code != 404 {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}

	rec := get(t, h, "/book/asin/"+strings.ToLower(fxASINWiseMan))
	wantStatus(t, rec, http.StatusSeeOther)
	if loc := rec.Header().Get("Location"); loc != fmt.Sprintf("/book/%d", fxEditionWiseMan) {
		t.Errorf("asin Location = %q", loc)
	}
	for _, p := range []string{"/book/asin/notanasin", "/book/asin/B000000000", "/book/asin/" + fxISBN10NOTW} {
		if rec := get(t, h, p); rec.Code != 404 {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}

	for _, p := range []string{"/book/isbn/" + fxISBN13NOTW, "/book/asin/" + fxASINNOTW, "/book/isbn/garbage"} {
		if rec := do(t, h, http.MethodDelete, p, nil); rec.Code != 200 {
			t.Errorf("DELETE %s = %d, want 200", p, rec.Code)
		}
	}
}

func TestBulkBook(t *testing.T) {
	t.Parallel()
	_, h, _ := newTestServer(t, Config{})

	body := fmt.Sprintf("[%d, %d, %d]", fxEditionNOTWHC, fxEditionWiseMan, fxEditionElantris)
	rec := do(t, h, http.MethodPost, "/book/bulk", strings.NewReader(body))
	wantStatus(t, rec, http.StatusSeeOther)
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/book/bulk?") || !strings.Contains(loc, "id="+strconv.FormatInt(fxEditionNOTWHC, 10)) {
		t.Fatalf("Location = %q", loc)
	}

	rec = get(t, h, loc)
	wantStatus(t, rec, 200)
	var bulk bulkBookResource
	decode(t, rec, &bulk)
	if len(bulk.Works) != 3 || len(bulk.Authors) != 2 || len(bulk.Series) != 1 {
		t.Errorf("bulk = %d works, %d authors, %d series", len(bulk.Works), len(bulk.Authors), len(bulk.Series))
	}
	// Sorted by rating count, and titles promoted to full titles.
	if bulk.Works[0].ForeignID != fxWorkNameOfWind || bulk.Works[1].ForeignID != fxWorkWiseMan {
		t.Errorf("order: %d %d %d", bulk.Works[0].ForeignID, bulk.Works[1].ForeignID, bulk.Works[2].ForeignID)
	}
	if bulk.Works[1].Title != bulk.Works[1].FullTitle {
		t.Errorf("bulk title not promoted: %q vs %q", bulk.Works[1].Title, bulk.Works[1].FullTitle)
	}

	// Oversized POST lists are truncated to bulkLimit.
	ids := make([]int64, 200)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	big, _ := json.Marshal(ids)
	rec = do(t, h, http.MethodPost, "/book/bulk", bytes.NewReader(big))
	wantStatus(t, rec, http.StatusSeeOther)
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(u.Query()["id"]); n != bulkLimit {
		t.Errorf("redirect carries %d ids, want %d", n, bulkLimit)
	}

	// Empty, invalid, and unknown-only inputs.
	for _, b := range []string{"[]", "", "{}", "null", "[\"x\"]"} {
		if rec := do(t, h, http.MethodPost, "/book/bulk", strings.NewReader(b)); rec.Code != 404 {
			t.Errorf("POST %q = %d, want 404", b, rec.Code)
		}
	}
	for _, q := range []string{"/book/bulk", "/book/bulk?id=abc&id=999999"} {
		rec := get(t, h, q)
		wantStatus(t, rec, 200)
		var raw map[string]json.RawMessage
		decode(t, rec, &raw)
		for _, k := range []string{"Works", "Series", "Authors"} {
			if string(raw[k]) != "[]" {
				t.Errorf("GET %s %s = %s, want []", q, k, raw[k])
			}
		}
	}

	// GET with more than bulkLimit ids is capped too.
	q := url.Values{}
	for i := 0; i < 150; i++ {
		q.Add("id", strconv.FormatInt(fxEditionNOTWHC, 10))
	}
	rec = get(t, h, "/book/bulk?"+q.Encode())
	wantStatus(t, rec, 200)
	decode(t, rec, &bulk)
	if len(bulk.Works) != bulkLimit {
		t.Errorf("GET bulk resolved %d works, want %d", len(bulk.Works), bulkLimit)
	}
}

func TestAuthorSeriesRecommendedChanged(t *testing.T) {
	t.Parallel()
	_, h, _ := newTestServer(t, Config{})

	rec := get(t, h, fmt.Sprintf("/author/%d", fxAuthorRothfuss))
	wantStatus(t, rec, 200)
	var a authorResource
	decode(t, rec, &a)
	if len(a.Works) != 3 || len(a.Series) != 1 {
		t.Errorf("author = %d works, %d series", len(a.Works), len(a.Series))
	}
	if rec := get(t, h, "/author/424242"); rec.Code != 404 {
		t.Errorf("unknown author = %d", rec.Code)
	}

	rec = get(t, h, fmt.Sprintf("/series/%d", fxSeriesKingkiller))
	wantStatus(t, rec, 200)
	var sr seriesResource
	decode(t, rec, &sr)
	if sr.ForeignID != fxSeriesKingkiller || sr.Title != "The Kingkiller Chronicle" || len(sr.LinkItems) != 2 {
		t.Errorf("series = %+v", sr)
	}
	if sr.LinkItems[0].ForeignWorkID != fxWorkNameOfWind || sr.LinkItems[0].PositionInSeries != "1" || sr.LinkItems[1].SeriesPosition != 2 {
		t.Errorf("links = %+v", sr.LinkItems)
	}
	if rec := get(t, h, "/series/9"); rec.Code != 404 {
		t.Errorf("unknown series = %d", rec.Code)
	}

	rec = get(t, h, "/recommended")
	wantStatus(t, rec, 200)
	var raw map[string]json.RawMessage
	decode(t, rec, &raw)
	if _, ok := raw["workIds"]; !ok {
		t.Errorf("recommended keys = %v", raw)
	}
	var rr recommendationsResource
	decode(t, rec, &rr)
	if len(rr.WorkIDs) != 4 || rr.WorkIDs[0] != fxWorkNameOfWind {
		t.Errorf("recommended = %v", rr.WorkIDs)
	}
	rec = get(t, h, "/recommended?page=2")
	wantStatus(t, rec, 200)
	decode(t, rec, &rr)
	if len(rr.WorkIDs) != 0 {
		t.Errorf("page 2 = %v", rr.WorkIDs)
	}
	if rec := get(t, h, "/recommended?page=0"); rec.Code != 404 {
		t.Errorf("page 0 = %d", rec.Code)
	}

	rec = get(t, h, "/author/changed")
	wantStatus(t, rec, 200)
	var changed struct {
		Limited bool
		Ids     []int64
	}
	decode(t, rec, &changed)
	if !changed.Limited || changed.Ids == nil || len(changed.Ids) != 0 {
		t.Errorf("changed = %s", rec.Body.String())
	}
}

func TestWebUIToggle(t *testing.T) {
	t.Parallel()
	_, off, _ := newTestServer(t, Config{})
	if rec := get(t, off, "/ui"); rec.Code != 404 {
		t.Errorf("/ui with UI disabled = %d, want 404", rec.Code)
	}
	if rec := get(t, off, "/ui/status"); rec.Code != 404 {
		t.Errorf("/ui/status with UI disabled = %d, want 404", rec.Code)
	}

	_, on, _ := newTestServer(t, Config{EnableWebUI: true})
	rec := get(t, on, "/ui")
	wantStatus(t, rec, 200)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q", ct)
	}
	if rec.Body.Len() == 0 {
		t.Error("empty console page")
	}
	rec = get(t, on, "/ui/status")
	wantStatus(t, rec, 200)
	var st map[string]any
	decode(t, rec, &st)
	if st["dataset"] == nil || st["metrics"] == nil {
		t.Errorf("status = %v", st)
	}
	if rec := get(t, on, "/ui/query"); rec.Code != 400 {
		t.Errorf("/ui/query without q = %d, want 400", rec.Code)
	}
	if rec := get(t, on, "/ui/query?mode=work&q=abc"); rec.Code != 400 {
		t.Errorf("/ui/query bad id = %d, want 400", rec.Code)
	}
}

func TestErrorHidesInternalDetail(t *testing.T) {
	t.Parallel()
	s, _, _ := newTestServer(t, Config{})

	rec := httptest.NewRecorder()
	s.error(rec, errors.New("sqlite: disk I/O error at page 42"))
	if rec.Code != 500 {
		t.Errorf("status = %d", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "internal error" {
		t.Errorf("body = %q, want generic message", body)
	}

	rec = httptest.NewRecorder()
	s.error(rec, fmt.Errorf("%w: work 7", errNotFound))
	if rec.Code != 404 || !strings.Contains(rec.Body.String(), "work 7") {
		t.Errorf("not found: %d %q", rec.Code, rec.Body.String())
	}
}

func TestMetrics(t *testing.T) {
	t.Parallel()
	if routeOf("/nope/x") != "other" || routeOf("/") != "other" || routeOf("") != "other" {
		t.Error("unknown routes must bucket as other")
	}
	if routeOf("/search?q=x") != "search" || routeOf("/work/1") != "work" || routeOf("/book/bulk?id=1") != "book" {
		t.Error("known routes")
	}
	if routeOf("/searchx") != "other" {
		t.Error("prefix must not match")
	}

	m := newMetrics()
	m.record("GET", "/work/1", 404, 1)
	m.record("GET", "/work/1", 404, 2)
	m.record("GET", "/work/2", 200, 3)
	m.record("GET", "/zzz", 500, 4)
	snap := m.snapshot()
	if snap["errors"].(int64) != 1 {
		t.Errorf("errors = %v, want 1 (404 is not an error)", snap["errors"])
	}
	if snap["total"].(int64) != 4 {
		t.Errorf("total = %v", snap["total"])
	}
	recent := snap["recent"].([]reqRecord)
	if len(recent) != 3 || recent[0].Path != "/zzz" || recent[2].Count != 2 {
		t.Errorf("recent = %+v", recent)
	}
	if by := snap["byRoute"].(map[string]int64); by["work"] != 3 || by["other"] != 1 {
		t.Errorf("byRoute = %v", by)
	}
	for i := 0; i < historySize+10; i++ {
		m.record("GET", fmt.Sprintf("/work/%d", i), 200, 0)
	}
	if n := len(m.snapshot()["recent"].([]reqRecord)); n != historySize {
		t.Errorf("history grew to %d", n)
	}

	// Through the handler: a 404 on the API counts as traffic, not error,
	// and the health/info routes are not instrumented at all.
	s, h, _ := newTestServer(t, Config{})
	get(t, h, "/work/999999")
	get(t, h, "/healthz")
	get(t, h, "/")
	snap = s.metrics.snapshot()
	if snap["total"].(int64) != 1 || snap["errors"].(int64) != 0 {
		t.Errorf("handler metrics = total %v errors %v", snap["total"], snap["errors"])
	}
}

func TestSwapUnderLoad(t *testing.T) {
	t.Parallel()
	s, h, path := newTestServer(t, Config{})
	fixtureB := newFixtureDBExtra(t)

	// Before: the extra work is unknown.
	if rec := get(t, h, fmt.Sprintf("/work/%d", fxWorkExtra)); rec.Code != 404 {
		t.Fatalf("extra work already present: %d", rec.Code)
	}

	const workers = 16
	const perWorker = 40
	var (
		wg       sync.WaitGroup
		stop     atomic.Bool
		bad      atomic.Int64
		badBody  atomic.Pointer[string]
		requests atomic.Int64
	)
	paths := []string{
		fmt.Sprintf("/work/%d", fxWorkNameOfWind),
		fmt.Sprintf("/work/%d", fxWorkNOTWDupe),
		"/search?q=name+of+the+wind",
		"/search?q=patrick+rothfuss",
		fmt.Sprintf("/author/%d", fxAuthorRothfuss),
		fmt.Sprintf("/book/bulk?id=%d&id=%d", fxEditionNOTWHC, fxEditionWiseMan),
		"/healthz",
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker && !stop.Load(); i++ {
				p := paths[(w+i)%len(paths)]
				req := httptest.NewRequest(http.MethodGet, p, nil)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				requests.Add(1)
				if rec.Code >= 500 {
					bad.Add(1)
					msg := fmt.Sprintf("%s -> %d: %s", p, rec.Code, rec.Body.String())
					badBody.Store(&msg)
				}
			}
		}(w)
	}

	// Mid-flight: install B over the served path, then swap.
	if err := os.Rename(fixtureB, path); err != nil {
		t.Fatal(err)
	}
	// The old pool is pinned to the old inode, so the old content must
	// still be served until Swap.
	if rec := get(t, h, fmt.Sprintf("/work/%d", fxWorkExtra)); rec.Code != 404 {
		t.Errorf("before Swap, renamed file leaked through: %d", rec.Code)
	}
	if err := s.Swap(path); err != nil {
		t.Fatalf("Swap: %v", err)
	}
	rec := get(t, h, fmt.Sprintf("/work/%d", fxWorkExtra))
	if rec.Code != 200 {
		t.Errorf("after Swap, extra work = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := get(t, h, fmt.Sprintf("/author/%d", fxAuthorRothfuss)); rec.Code == 200 {
		var a authorResource
		decode(t, rec, &a)
		if len(a.Works) != 4 {
			t.Errorf("after Swap author has %d works, want 4", len(a.Works))
		}
	} else {
		t.Errorf("author after swap = %d", rec.Code)
	}

	wg.Wait()
	if n := bad.Load(); n != 0 {
		t.Fatalf("%d of %d requests returned 5xx; first: %s", n, requests.Load(), *badBody.Load())
	}
	if requests.Load() < workers {
		t.Errorf("only %d requests ran", requests.Load())
	}

	// Stats reflect the new dataset once loaded.
	if st := s.datasetStats(); st.Ready && st.Works != 0 && st.Works != 5 {
		t.Errorf("stats works = %d", st.Works)
	}
}

func TestValidateAndClose(t *testing.T) {
	t.Parallel()
	path := newFixtureDB(t)
	if err := Validate(path); err != nil {
		t.Fatal(err)
	}
	if err := Validate(filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Error("Validate accepted a missing file")
	}
	s, err := New(Config{DBPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := New(Config{DBPath: filepath.Join(t.TempDir(), "missing.db")}); err == nil {
		t.Error("New accepted a missing file")
	}
}
