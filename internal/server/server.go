package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/NC1107/readarr-metadata-provider/internal/hcapi"
)

type Server struct {
	// app is replaced wholesale when a newer dataset is installed; readers
	// take it once per request and keep using it to the end.
	app          atomic.Pointer[app]
	version      string
	maxWorks     int
	officialBase string
	hc           *hcapi.Client
	metrics      *metrics
	webUI        bool
	searchLangs  map[string]bool
	minRatings   int64

	statsMu sync.Mutex
	stats   datasetStats
}

// langAllowed applies the search language preference: a work is kept when
// the filter is off, its language is unrecorded or allowed, or the query
// itself is written in non-Latin script.
func (s *Server) langAllowed(b *rawBook, query string) bool {
	if len(s.searchLangs) == 0 {
		return true
	}
	if !latinScript(query) {
		return true
	}
	// Any edition in an allowed language keeps the work: a book whose
	// most-shelved edition happens to be a translation is still the book
	// being searched for.
	anyKnown := false
	for _, e := range b.Editions {
		if e.Language == nil || e.Language.Code2 == "" {
			continue
		}
		anyKnown = true
		if s.searchLangs[e.Language.Code2] {
			return true
		}
	}
	if anyKnown {
		return false
	}
	// No language recorded anywhere (common for Hardcover's translated
	// imports): fall back to the title's script. A mostly non-Latin title is
	// not what a Latin-script query is after.
	var latin, letters int
	for _, r := range b.Title {
		if unicode.IsLetter(r) {
			letters++
			if r < 128 || unicode.Is(unicode.Latin, r) {
				latin++
			}
		}
	}
	return letters == 0 || latin*2 >= letters
}

// latinScript reports whether every letter in s is Latin. Accented Latin
// ("Misérables") counts as Latin; a query in Cyrillic, Han or Arabic does
// not, and is what the language filter must step aside for.
func latinScript(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) && !unicode.Is(unicode.Latin, r) {
			return false
		}
	}
	return true
}

// datasetStats is filled in the background at startup; count queries over
// millions of rows are too slow for the request path.
type datasetStats struct {
	Ready       bool   `json:"ready"`
	Works       int64  `json:"works"`
	Authors     int64  `json:"authors"`
	Series      int64  `json:"series"`
	Editions    int64  `json:"editions"`
	GeneratedAt string `json:"generatedAt"`
	SizeBytes   int64  `json:"sizeBytes"`
}

// loadStats counts the dataset behind a; it is given the app rather than
// reading the current one so a swap mid-count cannot mix two datasets, and
// a count that finishes after a later swap is discarded.
func (s *Server) loadStats(a *app) {
	var st datasetStats
	if fi, err := os.Stat(a.path); err == nil {
		st.SizeBytes = fi.Size()
	}
	_ = a.store.db.QueryRow(`SELECT value FROM meta WHERE key = 'generated_at'`).Scan(&st.GeneratedAt)
	for _, c := range []struct {
		table string
		dst   *int64
	}{
		{"works", &st.Works}, {"authors", &st.Authors},
		{"series", &st.Series}, {"editions", &st.Editions},
	} {
		_ = a.store.db.QueryRow(`SELECT count(*) FROM ` + c.table).Scan(c.dst)
	}
	st.Ready = true
	if s.current() != a {
		return
	}
	s.statsMu.Lock()
	s.stats = st
	s.statsMu.Unlock()
}

func (s *Server) datasetStats() datasetStats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.stats
}

// Config holds the server's tunables.
type Config struct {
	DBPath string
	// Version is reported on the info route.
	Version  string
	MaxWorks int
	// EnableWebUI mounts the comparison console at /ui. It is off by default
	// because the console is unauthenticated and makes the server query
	// third-party services on a visitor's behalf.
	EnableWebUI bool
	// OfficialBase is the reference service for the /ui console; empty
	// selects the public rreading-glasses Hardcover instance.
	OfficialBase string
	// HCToken is optional; with it, the /ui console adds a live Hardcover
	// comparison column.
	HCToken string
	// SearchLangs is a comma-separated list of edition language codes
	// search results may have (empty disables the filter); works with no
	// language recorded pass a title-script check instead, and a query
	// typed in non-Latin script skips the filter entirely.
	SearchLangs string
	// MinRatings drops search results with fewer ratings than this, unless
	// that would leave no results at all. Junk imports and box-set stubs
	// rarely clear even a low bar.
	MinRatings int64
}

func New(cfg Config) (*Server, error) {
	st, err := openStore(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	if cfg.OfficialBase == "" {
		cfg.OfficialBase = defaultOfficialBase
	}
	s := &Server{
		version:      cfg.Version,
		maxWorks:     cfg.MaxWorks,
		officialBase: cfg.OfficialBase,
		metrics:      newMetrics(),
		webUI:        cfg.EnableWebUI,
		minRatings:   cfg.MinRatings,
		searchLangs:  map[string]bool{},
	}
	if s.version == "" {
		s.version = "0.0.0-dev"
	}
	for _, l := range strings.Split(cfg.SearchLangs, ",") {
		if l = strings.TrimSpace(l); l != "" {
			s.searchLangs[l] = true
		}
	}
	a := &app{store: st, path: cfg.DBPath}
	s.app.Store(a)
	if cfg.HCToken != "" {
		s.hc = hcapi.NewClient(cfg.HCToken)
	}
	go s.loadStats(a)
	return s, nil
}

// Validate opens the dataset at path, proves it can serve a lookup, and
// closes it again. The updater runs it on a download before installing it.
func Validate(path string) error {
	st, err := openStore(path)
	if err != nil {
		return err
	}
	defer st.db.Close()
	return st.probe(context.Background())
}

// current returns the app serving requests right now.
func (s *Server) current() *app { return s.app.Load() }

// Probe runs a real lookup against the dataset currently serving, for the
// health route.
func (s *Server) Probe(ctx context.Context) error { return s.current().store.probe(ctx) }

// Swap installs a freshly opened dataset and closes the previous one after a
// grace window, which is far longer than any request takes, so nothing
// in flight touches a closed database. The old store's connections are
// pinned to the file they opened, so it keeps serving the old dataset until
// then even though the path now names the new file.
func (s *Server) Swap(dbPath string) error {
	st, err := openStore(dbPath)
	if err != nil {
		return err
	}
	if err := st.probe(context.Background()); err != nil {
		st.db.Close()
		return err
	}
	a := &app{store: st, path: dbPath}
	old := s.app.Swap(a)
	go s.loadStats(a)
	if old != nil && old.store != nil && old.store != st {
		time.AfterFunc(30*time.Second, func() { _ = old.store.db.Close() })
	}
	return nil
}

// Close releases the dataset. It is for shutdown, not for a swap.
func (s *Server) Close() error {
	if a := s.current(); a != nil && a.store != nil {
		return a.store.db.Close()
	}
	return nil
}

// bulkLimit caps how many editions one /book/bulk request may resolve. Each
// id costs several queries and a full resource assembly, and Readarr never
// asks for more than a page at a time.
const bulkLimit = 100

// maxBulkBody bounds the POST body of /book/bulk, which carries a JSON list
// of ids and nothing else.
const maxBulkBody = 1 << 20

func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("/search", s.search)
	api.HandleFunc("/recommended", s.recommended)
	api.HandleFunc("/work/{id}", s.getWork)
	api.HandleFunc("/book/{id}", s.getBook)
	api.HandleFunc("/book/asin/{asin}", s.getASIN)
	api.HandleFunc("/book/isbn/{isbn}", s.getISBN)
	api.HandleFunc("/book/bulk", s.bulkBook)
	api.HandleFunc("/author/{id}", s.getAuthor)
	api.HandleFunc("/author/changed", s.authorChanged)
	api.HandleFunc("/series/{id}", s.getSeries)

	mux := http.NewServeMux()
	// The console's own probes use the raw api handler, so only real client
	// traffic is instrumented into the history and counters.
	mux.Handle("/", s.instrument(api))
	// The root answers so that anything probing "is a server here" (the
	// switch script does, before it repoints Readarr) gets a yes, with
	// enough about the dataset to tell one instance from another.
	mux.HandleFunc("GET /{$}", s.handleInfo)
	// The healthcheck runs a real lookup, and stays out of the request
	// history so a 30-second poll does not crowd out real traffic.
	mux.HandleFunc("GET /healthz", s.handleHealth)
	// Browsers visiting the console request this; keep it out of the API
	// history and error counters.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><text y="13" font-size="13">&#128214;</text></svg>`))
	})
	if s.webUI {
		s.mountUI(mux, api)
	}
	return mux
}

// handleInfo describes the running instance: its version and what dataset
// it is serving.
func (s *Server) handleInfo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"service": "readarr-metadata-provider",
		"version": s.version,
		"dataset": s.datasetStats(),
	})
}

// handleHealth answers 200 only when the served dataset can actually answer
// a lookup, so an orchestrator sees "cannot serve" rather than "process
// exists".
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.Probe(ctx); err != nil {
		log.Printf("health probe failed: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "unhealthy", "error": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// error answers a handler failure. Not-found is reported as such; anything
// else is logged with its detail and answered with a generic 500, since a
// database error message is for the operator, not the client.
func (s *Server) error(w http.ResponseWriter, err error) {
	if errors.Is(err, errNotFound) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	log.Printf("request failed: %v", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

// noopDelete handles the cache-bust DELETEs Readarr sends; there is no cache
// to bust, so acknowledge and move on.
func noopDelete(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusOK)
		return true
	}
	return false
}

func pathID(r *http.Request, key string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(key), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: bad id %q", errNotFound, r.PathValue(key))
	}
	return id, nil
}

var (
	asinPattern = regexp.MustCompile(`^B[0-9A-Z]{9}$`)
	isbnPattern = regexp.MustCompile(`^(?:[0-9]{9}[0-9Xx]|[0-9]{13})$`)
)

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	if noopDelete(w, r) {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	a := s.current()

	var workIDs []int64
	normalized := strings.ReplaceAll(query, "-", "")
	switch {
	case asinPattern.MatchString(query):
		if editionID, err := a.store.editionByASIN(query); err == nil {
			if workID, err := a.store.workIDForEdition(editionID); err == nil {
				workIDs = []int64{workID}
			}
		}
	case isbnPattern.MatchString(normalized):
		if editionID, err := a.store.editionByISBN(strings.ToUpper(normalized)); err == nil {
			if workID, err := a.store.workIDForEdition(editionID); err == nil {
				workIDs = []int64{workID}
			}
		}
	default:
		var err error
		workIDs, err = a.store.searchWorks(query, 30)
		if err != nil {
			s.error(w, err)
			return
		}
	}

	results := []searchResource{}
	var belowCutoff []searchResource
	for _, workID := range workIDs {
		b, err := a.store.work(workID)
		if err != nil {
			continue
		}
		authorID, _ := bestAuthorID(b)
		if authorID == 0 || len(b.Editions) == 0 || !s.langAllowed(b, query) {
			continue
		}
		r := searchResource{
			BookID: b.Editions[0].ID,
			WorkID: b.ID,
			Author: searchResourceAuthor{ID: authorID},
		}
		if b.RatingsCount < s.minRatings {
			belowCutoff = append(belowCutoff, r)
			continue
		}
		results = append(results, r)
	}
	// A cutoff that filters everything filters nothing: an obscure book
	// with two ratings must still be findable when it is the only match.
	if len(results) == 0 {
		results = belowCutoff
	}
	if results == nil {
		results = []searchResource{}
	}
	writeJSON(w, results)
}

func (s *Server) getWork(w http.ResponseWriter, r *http.Request) {
	if noopDelete(w, r) {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		s.error(w, err)
		return
	}
	a := s.current()
	b, err := a.store.work(id)
	if err != nil {
		s.error(w, err)
		return
	}
	// Duplicate works carry a canonical_id; serve the canonical work like
	// upstream does.
	if b.CanonicalID != nil && *b.CanonicalID != 0 {
		if canonical, err := a.store.work(*b.CanonicalID); err == nil {
			b = canonical
		}
	}
	work, err := a.workResource(b, 0, map[int64]*rawSeries{})
	if err != nil {
		s.error(w, err)
		return
	}
	writeJSON(w, work)
}

// getBook mirrors upstream behavior: the client expects a redirect to an
// author payload scoped to the requested edition.
func (s *Server) getBook(w http.ResponseWriter, r *http.Request) {
	if noopDelete(w, r) {
		return
	}
	editionID, err := pathID(r, "id")
	if err != nil {
		s.error(w, err)
		return
	}
	a := s.current()
	workID, err := a.store.workIDForEdition(editionID)
	if err != nil {
		s.error(w, err)
		return
	}
	b, err := a.store.work(workID)
	if err != nil {
		s.error(w, err)
		return
	}
	authorID, _ := bestAuthorID(b)
	if authorID == 0 {
		s.error(w, fmt.Errorf("%w: work %d has no primary author", errNotFound, workID))
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/author/%d?edition=%d", authorID, editionID), http.StatusSeeOther)
}

func (s *Server) getASIN(w http.ResponseWriter, r *http.Request) {
	if noopDelete(w, r) {
		return
	}
	asin := strings.ToUpper(strings.TrimSpace(r.PathValue("asin")))
	if !asinPattern.MatchString(asin) {
		s.error(w, fmt.Errorf("%w: bad asin", errNotFound))
		return
	}
	editionID, err := s.current().store.editionByASIN(asin)
	if err != nil {
		s.error(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/book/%d", editionID), http.StatusSeeOther)
}

func (s *Server) getISBN(w http.ResponseWriter, r *http.Request) {
	if noopDelete(w, r) {
		return
	}
	isbn := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(r.PathValue("isbn")), "-", ""))
	if !isbnPattern.MatchString(isbn) {
		s.error(w, fmt.Errorf("%w: bad isbn", errNotFound))
		return
	}
	editionID, err := s.current().store.editionByISBN(isbn)
	if err != nil {
		s.error(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/book/%d", editionID), http.StatusSeeOther)
}

func (s *Server) getAuthor(w http.ResponseWriter, r *http.Request) {
	if noopDelete(w, r) {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		s.error(w, err)
		return
	}

	a := s.current()
	// A /book/{id} redirect lands here: return the author with only the
	// requested edition's work attached.
	if edition := r.URL.Query().Get("edition"); edition != "" {
		editionID, err := strconv.ParseInt(edition, 10, 64)
		if err != nil {
			s.error(w, fmt.Errorf("%w: bad edition %q", errNotFound, edition))
			return
		}
		workID, err := a.store.workIDForEdition(editionID)
		if err != nil {
			s.error(w, err)
			return
		}
		b, err := a.store.work(workID)
		if err != nil {
			s.error(w, err)
			return
		}
		work, err := a.workResource(b, editionID, map[int64]*rawSeries{})
		if err != nil {
			s.error(w, err)
			return
		}
		author := work.Authors[0]
		author.Works = []workResource{*work}
		author.RatingCount, author.AverageRating = a.store.authorAggregates(author.ForeignID)
		writeJSON(w, author)
		return
	}

	author, err := a.authorResource(id, s.maxWorks)
	if err != nil {
		s.error(w, err)
		return
	}
	writeJSON(w, author)
}

func (s *Server) authorChanged(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"Limited": true, "Ids": []}`))
}

func (s *Server) getSeries(w http.ResponseWriter, r *http.Request) {
	if noopDelete(w, r) {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		s.error(w, err)
		return
	}
	a := s.current()
	sr, err := a.store.series(id)
	if err != nil {
		s.error(w, err)
		return
	}
	links, err := a.store.seriesWorks(id, s.maxWorks)
	if err != nil {
		s.error(w, err)
		return
	}
	items := []seriesWorkLinkResource{}
	for _, l := range links {
		position := 0.0
		if l.Position != nil {
			position = *l.Position
		}
		items = append(items, seriesWorkLinkResource{
			ForeignWorkID:    l.WorkID,
			PositionInSeries: strconv.FormatFloat(position, 'f', -1, 64),
			SeriesPosition:   int(position),
			Primary:          false,
		})
	}
	writeJSON(w, seriesResource{
		ForeignID:   sr.ID,
		Title:       sr.Name,
		Description: sr.Description,
		LinkItems:   items,
	})
}

func (s *Server) bulkBook(w http.ResponseWriter, r *http.Request) {
	// POSTs redirect to a cacheable GET, matching upstream.
	if r.Method == http.MethodPost {
		var ids []int64
		body := http.MaxBytesReader(w, r.Body, maxBulkBody)
		if err := json.NewDecoder(body).Decode(&ids); err != nil || len(ids) == 0 {
			s.error(w, fmt.Errorf("%w: missing ids", errNotFound))
			return
		}
		if len(ids) > bulkLimit {
			ids = ids[:bulkLimit]
		}
		query := url.Values{}
		for _, id := range ids {
			query.Add("id", strconv.FormatInt(id, 10))
		}
		http.Redirect(w, r, r.URL.Path+"?"+query.Encode(), http.StatusSeeOther)
		return
	}

	result := bulkBookResource{
		Works:   []workResource{},
		Series:  []seriesResource{},
		Authors: []authorResource{},
	}
	seriesCache := map[int64]*rawSeries{}
	seenAuthors := map[int64]bool{}
	seenSeries := map[int64]bool{}
	a := s.current()

	ids := r.URL.Query()["id"]
	if len(ids) > bulkLimit {
		ids = ids[:bulkLimit]
	}
	for _, idStr := range ids {
		editionID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		workID, err := a.store.workIDForEdition(editionID)
		if err != nil {
			continue
		}
		b, err := a.store.work(workID)
		if err != nil {
			continue
		}
		work, err := a.workResource(b, editionID, seriesCache)
		if err != nil {
			continue
		}
		// Upstream promotes full titles for display in bulk results.
		if work.FullTitle != "" {
			work.Title = work.FullTitle
		}
		if len(work.Books) > 0 && work.Books[0].FullTitle != "" {
			work.Books[0].Title = work.Books[0].FullTitle
		}
		result.Works = append(result.Works, *work)

		author := work.Authors[0]
		if !seenAuthors[author.ForeignID] {
			seenAuthors[author.ForeignID] = true
			result.Authors = append(result.Authors, author)
		}
		for _, sr := range work.Series {
			if !seenSeries[sr.ForeignID] {
				seenSeries[sr.ForeignID] = true
				result.Series = append(result.Series, sr)
			}
		}
	}

	sort.SliceStable(result.Works, func(i, j int) bool {
		return result.Works[i].RatingCount > result.Works[j].RatingCount
	})
	writeJSON(w, result)
}

func (s *Server) recommended(w http.ResponseWriter, r *http.Request) {
	if noopDelete(w, r) {
		return
	}
	page := int64(1)
	if p := r.URL.Query().Get("page"); p != "" {
		page, _ = strconv.ParseInt(p, 10, 64)
	}
	if page < 1 {
		s.error(w, fmt.Errorf("%w: bad page", errNotFound))
		return
	}
	ids, err := s.current().store.topWorkIDs(100, int(100*(page-1)))
	if err != nil {
		s.error(w, err)
		return
	}
	writeJSON(w, recommendationsResource{WorkIDs: ids})
}
