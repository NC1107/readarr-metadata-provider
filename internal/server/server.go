package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/NC1107/readarr-metadata-provider/internal/hcapi"
)

type Server struct {
	app          *app
	maxWorks     int
	officialBase string
	hc           *hcapi.Client
	metrics      *metrics
	dbPath       string
	searchLangs  map[string]bool

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
	for _, r := range query {
		if r > 127 {
			return true
		}
	}
	if len(b.Editions) > 0 && b.Editions[0].Language != nil && b.Editions[0].Language.Code2 != "" {
		return s.searchLangs[b.Editions[0].Language.Code2]
	}
	// No language recorded (common for Hardcover's translated imports):
	// fall back to the title's script. A mostly non-Latin title is not what
	// a Latin-script query is after.
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

func (s *Server) loadStats() {
	var st datasetStats
	if fi, err := os.Stat(s.dbPath); err == nil {
		st.SizeBytes = fi.Size()
	}
	_ = s.app.store.db.QueryRow(`SELECT value FROM meta WHERE key = 'generated_at'`).Scan(&st.GeneratedAt)
	for _, c := range []struct {
		table string
		dst   *int64
	}{
		{"works", &st.Works}, {"authors", &st.Authors},
		{"series", &st.Series}, {"editions", &st.Editions},
	} {
		_ = s.app.store.db.QueryRow(`SELECT count(*) FROM ` + c.table).Scan(c.dst)
	}
	st.Ready = true
	s.statsMu.Lock()
	s.stats = st
	s.statsMu.Unlock()
}

func (s *Server) datasetStats() datasetStats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.stats
}

// New opens the dataset. hcToken is optional; with it, the /ui console adds
// a live Hardcover comparison column. searchLangs is a comma-separated list
// of edition language codes search results may have (empty disables the
// filter); works with no language recorded always pass, and a query typed
// in non-Latin script skips the filter so native-language searches work.
func New(dbPath string, maxWorks int, officialBase, hcToken, searchLangs string) (*Server, error) {
	st, err := openStore(dbPath)
	if err != nil {
		return nil, err
	}
	if officialBase == "" {
		officialBase = defaultOfficialBase
	}
	s := &Server{
		app:          &app{store: st},
		maxWorks:     maxWorks,
		officialBase: officialBase,
		metrics:      newMetrics(),
		dbPath:       dbPath,
		searchLangs:  map[string]bool{},
	}
	for _, l := range strings.Split(searchLangs, ",") {
		if l = strings.TrimSpace(l); l != "" {
			s.searchLangs[l] = true
		}
	}
	if hcToken != "" {
		s.hc = hcapi.NewClient(hcToken)
	}
	go s.loadStats()
	return s, nil
}

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
	// Browsers visiting the console request this; keep it out of the API
	// history and error counters.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"><text y="13" font-size="13">&#128214;</text></svg>`))
	})
	s.mountUI(mux, api)
	return mux
}

func (s *Server) error(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, errNotFound) {
		status = http.StatusNotFound
	}
	http.Error(w, err.Error(), status)
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

	var workIDs []int64
	normalized := strings.ReplaceAll(query, "-", "")
	switch {
	case asinPattern.MatchString(query):
		if editionID, err := s.app.store.editionByASIN(query); err == nil {
			if workID, err := s.app.store.workIDForEdition(editionID); err == nil {
				workIDs = []int64{workID}
			}
		}
	case isbnPattern.MatchString(normalized):
		if editionID, err := s.app.store.editionByISBN(strings.ToUpper(normalized)); err == nil {
			if workID, err := s.app.store.workIDForEdition(editionID); err == nil {
				workIDs = []int64{workID}
			}
		}
	default:
		var err error
		workIDs, err = s.app.store.searchWorks(query, 30)
		if err != nil {
			s.error(w, err)
			return
		}
	}

	results := []searchResource{}
	for _, workID := range workIDs {
		b, err := s.app.store.work(workID)
		if err != nil {
			continue
		}
		authorID, _ := bestAuthorID(b)
		if authorID == 0 || len(b.Editions) == 0 || !s.langAllowed(b, query) {
			continue
		}
		results = append(results, searchResource{
			BookID: b.Editions[0].ID,
			WorkID: b.ID,
			Author: searchResourceAuthor{ID: authorID},
		})
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
	b, err := s.app.store.work(id)
	if err != nil {
		s.error(w, err)
		return
	}
	// Duplicate works carry a canonical_id; serve the canonical work like
	// upstream does.
	if b.CanonicalID != nil && *b.CanonicalID != 0 {
		if canonical, err := s.app.store.work(*b.CanonicalID); err == nil {
			b = canonical
		}
	}
	work, err := s.app.workResource(b, 0, map[int64]*rawSeries{})
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
	workID, err := s.app.store.workIDForEdition(editionID)
	if err != nil {
		s.error(w, err)
		return
	}
	b, err := s.app.store.work(workID)
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
	asin := strings.TrimSpace(r.PathValue("asin"))
	editionID, err := s.app.store.editionByASIN(asin)
	if err != nil {
		s.error(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/book/%d", editionID), http.StatusSeeOther)
}

func (s *Server) getISBN(w http.ResponseWriter, r *http.Request) {
	isbn := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(r.PathValue("isbn")), "-", ""))
	if !isbnPattern.MatchString(isbn) {
		s.error(w, fmt.Errorf("%w: bad isbn", errNotFound))
		return
	}
	editionID, err := s.app.store.editionByISBN(isbn)
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

	// A /book/{id} redirect lands here: return the author with only the
	// requested edition's work attached.
	if edition := r.URL.Query().Get("edition"); edition != "" {
		editionID, err := strconv.ParseInt(edition, 10, 64)
		if err != nil {
			s.error(w, fmt.Errorf("%w: bad edition %q", errNotFound, edition))
			return
		}
		workID, err := s.app.store.workIDForEdition(editionID)
		if err != nil {
			s.error(w, err)
			return
		}
		b, err := s.app.store.work(workID)
		if err != nil {
			s.error(w, err)
			return
		}
		work, err := s.app.workResource(b, editionID, map[int64]*rawSeries{})
		if err != nil {
			s.error(w, err)
			return
		}
		author := work.Authors[0]
		author.Works = []workResource{*work}
		author.RatingCount, author.AverageRating = s.app.store.authorAggregates(author.ForeignID)
		writeJSON(w, author)
		return
	}

	author, err := s.app.authorResource(id, s.maxWorks)
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
	sr, err := s.app.store.series(id)
	if err != nil {
		s.error(w, err)
		return
	}
	links, err := s.app.store.seriesWorks(id, s.maxWorks)
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
		if err := json.NewDecoder(r.Body).Decode(&ids); err != nil || len(ids) == 0 {
			s.error(w, fmt.Errorf("%w: missing ids", errNotFound))
			return
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

	for _, idStr := range r.URL.Query()["id"] {
		editionID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			continue
		}
		workID, err := s.app.store.workIDForEdition(editionID)
		if err != nil {
			continue
		}
		b, err := s.app.store.work(workID)
		if err != nil {
			continue
		}
		work, err := s.app.workResource(b, editionID, seriesCache)
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
	ids, err := s.app.store.topWorkIDs(100, int(100*(page-1)))
	if err != nil {
		s.error(w, err)
		return
	}
	writeJSON(w, recommendationsResource{WorkIDs: ids})
}
