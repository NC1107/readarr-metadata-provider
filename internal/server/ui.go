package server

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"time"
)

//go:embed ui.html
var uiPage []byte

// defaultOfficialBase is the public rreading-glasses Hardcover instance,
// the service users would otherwise point Readarr at. Comparing against it
// side by side is the only way to answer "is this safe to switch to".
const defaultOfficialBase = "https://hardcover.bookinfo.pro"

func (s *Server) mountUI(mux *http.ServeMux, api http.Handler) {
	ui := &uiServer{server: s, api: api, officialBase: s.officialBase}
	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(uiPage)
	})
	mux.HandleFunc("GET /ui/query", ui.handleQuery)
}

type uiServer struct {
	server       *Server
	api          http.Handler
	officialBase string
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
	WorkID   int64   `json:"workId,omitempty"`
	BookID   int64   `json:"bookId,omitempty"`
	AuthorID int64   `json:"authorId,omitempty"`
	Rating   float64 `json:"rating,omitempty"`
	Ratings  int64   `json:"ratings,omitempty"`
	Image    string  `json:"image,omitempty"`
}

type uiResponse struct {
	Mode     string     `json:"mode"`
	Query    string     `json:"query"`
	Local    sideResult `json:"local"`
	Official sideResult `json:"official"`
	Verdict  string     `json:"verdict"`
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
	done := make(chan struct{})
	go func() {
		resp.Official = u.fetch("official", u.officialGET, mode, path)
		close(done)
	}()
	resp.Local = u.fetch("this server", u.localGET, mode, path)
	<-done

	resp.Verdict = verdict(resp.Local, resp.Official)
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
			it.Title = w.Title
			if len(w.Authors) > 0 {
				it.Subtitle = w.Authors[0].Name
			}
			if year := releaseYear(w.ReleaseDate); year != "" {
				it.Subtitle += " · " + year
			}
			it.Rating = w.AverageRating
			it.Ratings = w.RatingCount
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
	it := uiItem{
		Title:   w.Title,
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

func verdict(local, official sideResult) string {
	switch {
	case local.Error != "" && official.Error != "":
		return "both sides failed"
	case local.Error != "":
		return "this server failed where the official service answered"
	case official.Error != "":
		return "official service unavailable; showing this server only"
	}
	agree := "top results differ"
	if len(local.Items) > 0 && len(official.Items) > 0 {
		if local.Items[0].WorkID == official.Items[0].WorkID {
			agree = "top result agrees"
		}
	} else if len(local.Items) == len(official.Items) {
		agree = "both empty"
	}
	return fmt.Sprintf("%s · %dms vs %dms", agree, local.Took, official.Took)
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
