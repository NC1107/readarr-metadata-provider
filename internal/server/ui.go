package server

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/NC1107/readarr-metadata-provider/internal/hcapi"
	"github.com/NC1107/readarr-metadata-provider/internal/seeder"
)

//go:embed ui.html
var uiPage []byte

// defaultOfficialBase is the public rreading-glasses Hardcover instance,
// the service users would otherwise point Readarr at. Comparing against it
// side by side is the only way to answer "is this safe to switch to".
const defaultOfficialBase = "https://hardcover.bookinfo.pro"

func (s *Server) mountUI(mux *http.ServeMux, api http.Handler) {
	ui := &uiServer{server: s, api: api, officialBase: s.officialBase, hc: s.hc}
	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(uiPage)
	})
	mux.HandleFunc("GET /ui/query", ui.handleQuery)
	mux.HandleFunc("GET /ui/status", ui.handleStatus)
}

func (u *uiServer) handleStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"dataset": u.server.datasetStats(),
		"metrics": u.server.metrics.snapshot(),
	})
}

type uiServer struct {
	server       *Server
	api          http.Handler
	officialBase string
	hc           *hcapi.Client
}

// sideResult is one half of a comparison: what one server returned and what
// it cost.
type sideResult struct {
	Label        string          `json:"label"`
	Took         int64           `json:"tookMs"`
	Bytes        int             `json:"bytes"`
	Count        int             `json:"count"`
	Error        string          `json:"error,omitempty"`
	FormatIssues []string        `json:"formatIssues"`
	Items        []uiItem        `json:"items"`
	Raw          json.RawMessage `json:"raw,omitempty"`
}

type uiItem struct {
	Title    string  `json:"title"`
	Subtitle string  `json:"subtitle"`
	URL      string  `json:"url,omitempty"`
	WorkID   int64   `json:"workId,omitempty"`
	BookID   int64   `json:"bookId,omitempty"`
	AuthorID int64   `json:"authorId,omitempty"`
	Rating   float64 `json:"rating,omitempty"`
	Ratings  int64   `json:"ratings,omitempty"`
	Image    string  `json:"image,omitempty"`
}

type uiResponse struct {
	Mode      string     `json:"mode"`
	Query     string     `json:"query"`
	Local     sideResult `json:"local"`
	Official  sideResult `json:"official"`
	Hardcover sideResult `json:"hardcover"`
}

func (u *uiServer) handleQuery(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		http.Error(w, "missing q", http.StatusBadRequest)
		return
	}
	path, err := modePath(mode, query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	resp := uiResponse{Mode: mode, Query: query}
	official := make(chan struct{})
	hardcover := make(chan struct{})
	go func() {
		resp.Official = u.fetch("readarr endpoint", u.officialGET, mode, path)
		close(official)
	}()
	go func() {
		resp.Hardcover = u.fetchHardcover(r.Context(), mode, query)
		close(hardcover)
	}()
	resp.Local = u.fetch("local server", u.localGET, mode, path)
	<-official
	<-hardcover

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func modePath(mode, query string) (string, error) {
	id := strings.TrimSpace(query)
	switch mode {
	case "search", "":
		return "/search?q=" + url.QueryEscape(query), nil
	case "work":
		return "/work/" + id, nil
	case "author":
		return "/author/" + id, nil
	case "book":
		return "/book/" + id, nil
	default:
		return "", fmt.Errorf("unknown mode %q", mode)
	}
}

type getFunc func(path string) ([]byte, int, error)

// fetch runs one side of the comparison, following the contract's redirect
// choreography and hydrating search results through that side's own bulk
// endpoint so both columns are built the same way.
func (u *uiServer) fetch(label string, get getFunc, mode, path string) sideResult {
	res := sideResult{Label: label, FormatIssues: []string{}, Items: []uiItem{}}
	start := time.Now()
	body, status, err := get(path)
	res.Took = time.Since(start).Milliseconds()
	res.Bytes = len(body)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	if status != http.StatusOK {
		res.Error = fmt.Sprintf("HTTP %d: %s", status, truncate(string(body), 200))
		return res
	}

	if mode == "search" || mode == "" {
		var results []searchResource
		if err := json.Unmarshal(body, &results); err != nil {
			res.Error = "bad search JSON: " + err.Error()
			return res
		}
		res.Count = len(results)
		res.FormatIssues = checkSearchFormat(body)
		res.Items = u.hydrate(get, results, &res)
		return res
	}

	res.Raw = body
	res.FormatIssues = checkResourceFormat(mode, body)
	res.Items, res.Count = resourceItems(mode, body)
	return res
}

// hydrate resolves search triples into displayable rows via /book/bulk.
func (u *uiServer) hydrate(get getFunc, results []searchResource, res *sideResult) []uiItem {
	items := []uiItem{}
	if len(results) == 0 {
		return items
	}
	limit := min(len(results), 10)
	params := url.Values{}
	for _, r := range results[:limit] {
		params.Add("id", fmt.Sprint(r.BookID))
	}
	body, status, err := get("/book/bulk?" + params.Encode())
	if err != nil || status != http.StatusOK {
		res.FormatIssues = append(res.FormatIssues, "bulk hydration failed")
		for _, r := range results[:limit] {
			items = append(items, uiItem{Title: fmt.Sprintf("work %d", r.WorkID), WorkID: r.WorkID, BookID: r.BookID, AuthorID: r.Author.ID})
		}
		return items
	}
	var bulk bulkBookResource
	if err := json.Unmarshal(body, &bulk); err != nil {
		res.FormatIssues = append(res.FormatIssues, "bad bulk JSON: "+err.Error())
		return items
	}
	byBook := map[int64]workResource{}
	for _, w := range bulk.Works {
		if len(w.Books) > 0 {
			byBook[w.Books[0].ForeignID] = w
		}
	}
	for _, r := range results[:limit] {
		it := uiItem{WorkID: r.WorkID, BookID: r.BookID, AuthorID: r.Author.ID}
		if w, ok := byBook[r.BookID]; ok {
			// Bulk promotes Title to FullTitle for the client; the console
			// shows the short title so columns align visually.
			it.Title = w.Title
			if w.ShortTitle != "" {
				it.Title = w.ShortTitle
			}
			if len(w.Authors) > 0 {
				it.Subtitle = w.Authors[0].Name
			}
			if year := releaseYear(w.ReleaseDate); year != "" {
				it.Subtitle += " · " + year
			}
			it.Rating = w.AverageRating
			it.Ratings = w.RatingCount
			it.URL = w.URL
			if len(w.Books) > 0 {
				it.Image = w.Books[0].ImageURL
			}
		} else {
			it.Title = fmt.Sprintf("work %d", r.WorkID)
		}
		items = append(items, it)
	}
	return items
}

func resourceItems(mode string, body []byte) ([]uiItem, int) {
	items := []uiItem{}
	switch mode {
	case "work":
		var w workResource
		if json.Unmarshal(body, &w) != nil {
			return items, 0
		}
		items = append(items, workItem(w))
		return items, 1
	case "author", "book":
		var a authorResource
		if json.Unmarshal(body, &a) != nil {
			return items, 0
		}
		for i, w := range a.Works {
			if i >= 50 {
				break
			}
			items = append(items, workItem(w))
		}
		return items, len(a.Works)
	}
	return items, 0
}

func workItem(w workResource) uiItem {
	title := w.Title
	if w.ShortTitle != "" {
		title = w.ShortTitle
	}
	it := uiItem{
		Title:   title,
		URL:     w.URL,
		WorkID:  w.ForeignID,
		BookID:  w.BestBookID,
		Rating:  w.AverageRating,
		Ratings: w.RatingCount,
	}
	if len(w.Authors) > 0 {
		it.Subtitle = w.Authors[0].Name
		it.AuthorID = w.Authors[0].ForeignID
	}
	if year := releaseYear(w.ReleaseDate); year != "" {
		it.Subtitle += " · " + year
	}
	if len(w.Books) > 0 {
		it.Image = w.Books[0].ImageURL
	}
	return it
}

func releaseYear(d string) string {
	if len(d) >= 4 {
		return d[:4]
	}
	return ""
}

// localGET serves a path through the real handler stack in process,
// following redirect hops the same way Readarr's client would.
func (u *uiServer) localGET(path string) ([]byte, int, error) {
	for hop := 0; hop < 4; hop++ {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		u.api.ServeHTTP(rec, req)
		if loc := rec.Header().Get("Location"); rec.Code >= 300 && rec.Code < 400 && loc != "" {
			path = loc
			continue
		}
		return rec.Body.Bytes(), rec.Code, nil
	}
	return nil, 0, fmt.Errorf("too many redirects")
}

var uiClient = &http.Client{Timeout: 30 * time.Second}

func (u *uiServer) officialGET(path string) ([]byte, int, error) {
	resp, err := uiClient.Get(u.officialBase + path)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	return body, resp.StatusCode, err
}

// fetchHardcover builds the third column straight from Hardcover's API:
// their native search ranking, or the live book rows for an id, mapped
// through the same resource assembly the local column uses.
func (u *uiServer) fetchHardcover(ctx context.Context, mode, query string) (res sideResult) {
	res = sideResult{Label: "hardcover", FormatIssues: []string{}, Items: []uiItem{}}
	if u.hc == nil {
		res.Error = "no HARDCOVER_TOKEN configured"
		return res
	}
	start := time.Now()
	defer func() { res.Took = time.Since(start).Milliseconds() }()

	var books []rawBook
	var editionID int64
	var n int
	var err error
	switch mode {
	case "search", "":
		books, n, err = u.hcSearchBooks(ctx, query)
	case "work":
		books, n, err = u.hcBooks(ctx, fmt.Sprintf(`{id: {_eq: %s}}`, query), 1, "")
	case "book":
		editionID, _ = strconv.ParseInt(query, 10, 64)
		books, n, err = u.hcBooks(ctx, fmt.Sprintf(`{editions: {id: {_eq: %s}}}`, query), 1, "")
	case "author":
		books, n, err = u.hcBooks(ctx, fmt.Sprintf(`{contributions: {author_id: {_eq: %s}}}`, query), 10,
			`order_by: {users_count: desc_nulls_last},`)
	}
	res.Bytes = n
	if err != nil {
		res.Error = err.Error()
		return res
	}

	seriesCache := map[int64]*rawSeries{}
	for _, b := range books {
		w, err := u.server.app.workResource(&b, editionID, seriesCache)
		if err != nil {
			continue
		}
		res.Items = append(res.Items, workItem(*w))
	}
	res.Count = len(res.Items)
	return res
}

func (u *uiServer) hcBooks(ctx context.Context, where string, limit int, extra string) ([]rawBook, int, error) {
	q := fmt.Sprintf(`{ books(where: %s, %s limit: %d) { %s } }`,
		where, extra, limit, seeder.Entities["books"].Fields)
	data, err := u.hc.Query(ctx, q, nil)
	if err != nil {
		return nil, 0, err
	}
	var out struct {
		Books []rawBook `json:"books"`
	}
	return out.Books, len(data), json.Unmarshal(data, &out)
}

// hcSearchBooks runs Hardcover's own search with the exact configuration
// rreading-glasses uses, then fetches the matched books in rank order.
func (u *uiServer) hcSearchBooks(ctx context.Context, query string) ([]rawBook, int, error) {
	data, err := u.hc.Query(ctx, `query ($q: String!) {
		search(query: $q, per_page: 10, query_type: "book",
			fields: "title,isbns,series_names,author_names,alternative_titles",
			weights: "5,1,3,5,1",
			sort: "ratings_count:desc,_text_match:desc") { ids }
	}`, map[string]any{"q": query})
	if err != nil {
		return nil, 0, err
	}
	var out struct {
		Search struct {
			IDs []json.Number `json:"ids"`
		} `json:"search"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, len(data), err
	}
	if len(out.Search.IDs) == 0 {
		return nil, len(data), nil
	}
	ids := make([]string, len(out.Search.IDs))
	for i, id := range out.Search.IDs {
		ids[i] = id.String()
	}
	books, n, err := u.hcBooks(ctx, fmt.Sprintf(`{id: {_in: [%s]}}`, strings.Join(ids, ",")), len(ids), "")
	if err != nil {
		return nil, len(data) + n, err
	}
	byID := map[string]rawBook{}
	for _, b := range books {
		byID[fmt.Sprint(b.ID)] = b
	}
	ordered := make([]rawBook, 0, len(books))
	for _, id := range ids {
		if b, ok := byID[id]; ok {
			ordered = append(ordered, b)
		}
	}
	return ordered, len(data) + n, nil
}

// checkSearchFormat verifies the exact lowercase keys Readarr's search
// deserializer expects.
func checkSearchFormat(body []byte) []string {
	issues := []string{}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil {
		return append(issues, "not a JSON array")
	}
	if len(rows) == 0 {
		return issues
	}
	for _, key := range []string{"bookId", "workId", "author"} {
		if _, ok := rows[0][key]; !ok {
			issues = append(issues, "missing key "+key)
		}
	}
	return issues
}

// checkResourceFormat verifies the PascalCase keys Readarr's resource
// deserializer expects on works and authors.
func checkResourceFormat(mode string, body []byte) []string {
	issues := []string{}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return append(issues, "not a JSON object")
	}
	required := []string{"ForeignId", "Title", "Url"}
	if mode == "author" || mode == "book" {
		required = []string{"ForeignId", "Name", "Description", "ImageUrl", "Url", "Works", "Series"}
	}
	for _, key := range required {
		if _, ok := m[key]; !ok {
			issues = append(issues, "missing key "+key)
		}
	}
	return issues
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
