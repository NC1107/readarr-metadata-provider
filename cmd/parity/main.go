// Command parity measures agreement with the public rreading-glasses
// Hardcover instance, the service users would otherwise point Readarr at.
//
// Correctness of a single lookup is provable against a fixture; search
// quality is not, because it is a ranking. The only workable definition is
// agreement with the service users are switching away from: if someone
// searches for a book and we put a different one first, they will notice
// and they will blame us.
//
// The official instance is community-funded and throttles aggressively, so
// queries are paced far apart and a 429 waits rather than retries hot.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const officialBase = "https://hardcover.bookinfo.pro"

type searchResult struct {
	BookID int64 `json:"bookId"`
	WorkID int64 `json:"workId"`
	Author struct {
		ID int64 `json:"id"`
	} `json:"author"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "parity:", err)
		os.Exit(1)
	}
}

func run() error {
	base := flag.String("base", "http://localhost:8816", "the server under test")
	queryFile := flag.String("queries", "fixtures/search-queries.txt", "file of queries, one per line")
	pace := flag.Duration("pace", 5*time.Second, "delay between reference queries")
	deep := flag.Bool("deep", false, "field-by-field work comparison instead of search parity")
	source := flag.String("source", "official", `reference: "official" (the public rreading-glasses instance) or "hardcover" (Hardcover's own search API, needs HARDCOVER_TOKEN; this is the same backend the official instance defers to, minus its HTTP cache and throttle)`)
	flag.Parse()

	if *deep {
		return deepCompare(*base)
	}

	reference := func(q string) ([]searchResult, error) { return search(officialBase, q) }
	referenceName := officialBase
	if *source == "hardcover" {
		token := os.Getenv("HARDCOVER_TOKEN")
		if token == "" {
			return fmt.Errorf("-source hardcover needs HARDCOVER_TOKEN set")
		}
		reference = func(q string) ([]searchResult, error) { return searchHardcover(token, q) }
		referenceName = "Hardcover's search API"
		if *pace == 5*time.Second {
			*pace = 1500 * time.Millisecond // Their limit is 60/min; stay well under.
		}
	}

	queries, err := readQueries(*queryFile)
	if err != nil {
		return err
	}
	fmt.Printf("Comparing %s against %s over %d queries (paced %s apart)\n\n",
		*base, referenceName, len(queries), *pace)

	var top1, top5, missing, answered int
	for i, q := range queries {
		if i > 0 {
			time.Sleep(*pace)
		}
		ours, ourErr := search(*base, q)
		theirs, theirErr := reference(q)
		if ourErr != nil || theirErr != nil {
			fmt.Printf("  ERROR  %-40q ours=%v theirs=%v\n", q, ourErr, theirErr)
			continue
		}
		if len(theirs) == 0 {
			continue // A query the official service cannot answer says nothing about us.
		}
		answered++
		want := theirs[0].WorkID

		switch {
		case len(ours) == 0:
			missing++
			fmt.Printf("  EMPTY  %-40q official put work %d first\n", q, want)
		case ours[0].WorkID == want:
			top1++
			top5++
		default:
			inTop5 := false
			for _, r := range ours[:min(len(ours), 5)] {
				if r.WorkID == want {
					inTop5 = true
					break
				}
			}
			if inTop5 {
				top5++
				fmt.Printf("  RANK   %-40q official's top pick is in our top 5, not first\n", q)
			} else {
				fmt.Printf("  DIFF   %-40q official put work %d first; not in our top 5\n", q, want)
			}
		}
	}

	if answered == 0 {
		return fmt.Errorf("the official service answered no queries; try again later")
	}
	fmt.Printf("\nTop-1 agreement: %d/%d (%.0f%%)\n", top1, answered, 100*float64(top1)/float64(answered))
	fmt.Printf("Top-5 containment: %d/%d (%.0f%%)\n", top5, answered, 100*float64(top5)/float64(answered))
	fmt.Printf("Empty where official answered: %d\n", missing)
	return nil
}

// deepWorks are popular works across genres and formats, compared field by
// field. IDs are Hardcover ids, valid on both sides by construction.
var deepWorks = []int64{
	379217, // The Name of the Wind (fantasy, series)
	373525, // It (horror)
	383587, // 11/22/63 (fiction)
	280359, // The Shining
	376341, // The Stand
}

func deepCompare(base string) error {
	fmt.Printf("Deep-comparing %d works between %s and %s\n\n", len(deepWorks), base, officialBase)
	clean := true
	for i, id := range deepWorks {
		if i > 0 {
			time.Sleep(3 * time.Second)
		}
		ours, err := getJSON(fmt.Sprintf("%s/work/%d", base, id))
		if err != nil {
			return fmt.Errorf("ours work %d: %w", id, err)
		}
		theirs, err := getJSON(fmt.Sprintf("%s/work/%d", officialBase, id))
		if err != nil {
			fmt.Printf("  SKIP work %d: official: %v\n", id, err)
			continue
		}
		issues := diffShape(theirs, ours, "")
		for _, key := range []string{"ForeignId", "Title"} {
			if fmt.Sprint(theirs[key]) != fmt.Sprint(ours[key]) {
				issues = append(issues, fmt.Sprintf("%s: %v vs %v", key, theirs[key], ours[key]))
			}
		}
		if len(issues) == 0 {
			fmt.Printf("  OK   work %d (%v)\n", id, ours["Title"])
			continue
		}
		clean = false
		fmt.Printf("  DIFF work %d (%v)\n", id, ours["Title"])
		for _, is := range issues {
			fmt.Printf("       %s\n", is)
		}
	}
	if !clean {
		return fmt.Errorf("structural differences found")
	}
	return nil
}

// diffShape reports keys or types the official response has that ours lacks
// (and vice versa), recursively, comparing the first element of arrays.
func diffShape(theirs, ours map[string]any, path string) []string {
	var issues []string
	for k, tv := range theirs {
		ov, ok := ours[k]
		if !ok {
			issues = append(issues, path+k+": missing in ours")
			continue
		}
		issues = append(issues, diffValue(tv, ov, path+k)...)
	}
	for k := range ours {
		if _, ok := theirs[k]; !ok {
			issues = append(issues, path+k+": extra in ours")
		}
	}
	return issues
}

func diffValue(tv, ov any, path string) []string {
	switch t := tv.(type) {
	case map[string]any:
		o, ok := ov.(map[string]any)
		if !ok {
			return []string{path + ": type mismatch"}
		}
		return diffShape(t, o, path+".")
	case []any:
		o, ok := ov.([]any)
		if !ok {
			return []string{path + ": type mismatch"}
		}
		if len(t) > 0 && len(o) > 0 {
			return diffValue(t[0], o[0], path+"[0]")
		}
	}
	return nil
}

func search(base, q string) ([]searchResult, error) {
	body, err := get(base + "/search?q=" + url.QueryEscape(q))
	if err != nil {
		return nil, err
	}
	var out []searchResult
	return out, json.Unmarshal(body, &out)
}

// searchHardcover asks Hardcover's search API directly, with the exact
// query configuration rreading-glasses uses, so the ranking it returns is
// what the official instance would serve uncached.
func searchHardcover(token, q string) ([]searchResult, error) {
	payload, _ := json.Marshal(map[string]any{
		"query": `query ($q: String!) {
			search(query: $q, per_page: 15, query_type: "book",
				fields: "title,isbns,series_names,author_names,alternative_titles",
				weights: "5,1,3,5,1",
				sort: "ratings_count:desc,_text_match:desc") { ids }
		}`,
		"variables": map[string]string{"q": q},
	})
	req, err := http.NewRequest(http.MethodPost, "https://api.hardcover.app/v1/graphql", strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var out struct {
		Data struct {
			Search struct {
				IDs []json.Number `json:"ids"`
			} `json:"search"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("bad response: %s", truncateStr(string(body), 120))
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("graphql: %s", out.Errors[0].Message)
	}
	results := make([]searchResult, 0, len(out.Data.Search.IDs))
	for _, id := range out.Data.Search.IDs {
		n, err := id.Int64()
		if err != nil {
			continue
		}
		results = append(results, searchResult{WorkID: n})
	}
	return results, nil
}

func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func getJSON(u string) (map[string]any, error) {
	body, err := get(u)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(body, &out)
}

var client = &http.Client{Timeout: 60 * time.Second}

func get(u string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		resp, err := client.Get(u)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt == 0 {
			time.Sleep(30 * time.Second) // Let the official instance breathe, once.
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return body, nil
	}
}

func readQueries(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if q := strings.TrimSpace(sc.Text()); q != "" && !strings.HasPrefix(q, "#") {
			out = append(out, q)
		}
	}
	return out, sc.Err()
}
