package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// fixturesDir is the reference service's captured responses, relative to
// this package.
const fixturesDir = "../../fixtures/rg"

// jsonKeys returns the JSON member names a struct type can produce,
// including omitempty fields (which a re-marshalled zero value would hide).
func jsonKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		keys[name] = true
	}
	return keys
}

// unknownKeys reports members of obj that typ cannot represent, sorted.
func unknownKeys(obj map[string]json.RawMessage, typ reflect.Type) []string {
	known := jsonKeys(typ)
	var missing []string
	for k := range obj {
		if !known[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	return missing
}

// checkShape asserts that every key in raw (an object) is known to typ,
// then recurses into the named array members with their element types.
func checkShape(t *testing.T, label string, raw json.RawMessage, typ reflect.Type, nested map[string]reflect.Type) {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("%s: not a JSON object: %v", label, err)
	}
	if missing := unknownKeys(obj, typ); len(missing) > 0 {
		t.Errorf("%s: reference emits keys unknown to %s: %v", label, typ.Name(), missing)
	}
	for member, elemType := range nested {
		arr, ok := obj[member]
		if !ok {
			continue
		}
		var items []json.RawMessage
		if err := json.Unmarshal(arr, &items); err != nil {
			t.Errorf("%s.%s: not an array: %v", label, member, err)
			continue
		}
		if len(items) == 0 {
			continue
		}
		checkShape(t, label+"."+member+"[0]", items[0], elemType, nil)
	}
}

// roundTrip proves the fixture decodes into v and that re-encoding keeps
// every non-omitempty key, i.e. the struct really models the payload.
func roundTrip(t *testing.T, label string, raw []byte, v any) json.RawMessage {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s: decoding into %T: %v", label, v, err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: re-marshal: %v", label, err)
	}
	return out
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixturesDir, name))
	if err != nil {
		t.Skipf("fixture unavailable: %v", err)
	}
	trimmed := bytes.TrimSpace(raw)
	// Some captures record the reference service's failure at capture time
	// (e.g. "searching: HTTP 429: Too Many Requests") rather than a
	// response body. They carry no shape to compare against; skip them
	// loudly so a later re-capture starts being checked automatically.
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		t.Skipf("%s is not a JSON capture: %q", name, string(trimmed))
	}
	return raw
}

var (
	workType    = reflect.TypeOf(workResource{})
	authorType  = reflect.TypeOf(authorResource{})
	bookType    = reflect.TypeOf(bookResource{})
	seriesType  = reflect.TypeOf(seriesResource{})
	linkType    = reflect.TypeOf(seriesWorkLinkResource{})
	contribType = reflect.TypeOf(contributorResource{})
	searchType  = reflect.TypeOf(searchResource{})
)

func TestFixtureShapes(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(fixturesDir)
	if err != nil {
		t.Skipf("fixtures unavailable: %v", err)
	}
	seen := 0
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) != ".json" {
			continue
		}
		seen++
		t.Run(name, func(t *testing.T) {
			raw := readFixture(t, name)
			switch {
			case strings.HasPrefix(name, "work_"):
				var w workResource
				roundTrip(t, name, raw, &w)
				if w.ForeignID == 0 || len(w.Books) == 0 || len(w.Authors) == 0 {
					t.Errorf("%s decoded hollow: id %d, %d books, %d authors", name, w.ForeignID, len(w.Books), len(w.Authors))
				}
				checkShape(t, name, raw, workType, map[string]reflect.Type{
					"Books": bookType, "Authors": authorType, "Series": seriesType,
				})
				checkNested(t, name, raw)
			case strings.HasPrefix(name, "author_"):
				var a authorResource
				roundTrip(t, name, raw, &a)
				if a.ForeignID == 0 || len(a.Works) == 0 {
					t.Errorf("%s decoded hollow: id %d, %d works", name, a.ForeignID, len(a.Works))
				}
				checkShape(t, name, raw, authorType, map[string]reflect.Type{
					"Works": workType, "Series": seriesType,
				})
				checkNested(t, name, raw)
			case strings.HasPrefix(name, "series_"):
				var s seriesResource
				roundTrip(t, name, raw, &s)
				checkShape(t, name, raw, seriesType, map[string]reflect.Type{"LinkItems": linkType})
			case strings.HasPrefix(name, "search_"):
				var rs []searchResource
				roundTrip(t, name, raw, &rs)
				var items []json.RawMessage
				if err := json.Unmarshal(raw, &items); err != nil {
					t.Fatalf("%s: not a list: %v", name, err)
				}
				if len(items) > 0 {
					checkShape(t, name+"[0]", items[0], searchType, map[string]reflect.Type{})
					var first map[string]json.RawMessage
					_ = json.Unmarshal(items[0], &first)
					if a, ok := first["author"]; ok {
						checkShape(t, name+"[0].author", a, reflect.TypeOf(searchResourceAuthor{}), nil)
					}
				}
			default:
				t.Skipf("no resource type mapped for %s", name)
			}
		})
	}
	if seen == 0 {
		t.Skip("no fixtures present")
	}
}

// checkNested walks deeper than the top-level pass: the first book's
// Contributors, the first series' LinkItems, and the works nested under the
// first author (or the first work's books under an author payload).
func checkNested(t *testing.T, name string, raw json.RawMessage) {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	firstOf := func(parent map[string]json.RawMessage, member string) map[string]json.RawMessage {
		var items []json.RawMessage
		if err := json.Unmarshal(parent[member], &items); err != nil || len(items) == 0 {
			return nil
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(items[0], &m) != nil {
			return nil
		}
		return m
	}
	if book := firstOf(obj, "Books"); book != nil {
		if c := firstOf(book, "Contributors"); c != nil {
			if missing := unknownKeys(c, contribType); len(missing) > 0 {
				t.Errorf("%s.Books[0].Contributors[0]: unknown keys %v", name, missing)
			}
		}
	}
	if sr := firstOf(obj, "Series"); sr != nil {
		if l := firstOf(sr, "LinkItems"); l != nil {
			if missing := unknownKeys(l, linkType); len(missing) > 0 {
				t.Errorf("%s.Series[0].LinkItems[0]: unknown keys %v", name, missing)
			}
		}
	}
	if w := firstOf(obj, "Works"); w != nil {
		if missing := unknownKeys(w, workType); len(missing) > 0 {
			t.Errorf("%s.Works[0]: unknown keys %v", name, missing)
		}
		if b := firstOf(w, "Books"); b != nil {
			if missing := unknownKeys(b, bookType); len(missing) > 0 {
				t.Errorf("%s.Works[0].Books[0]: unknown keys %v", name, missing)
			}
		}
	}
	if a := firstOf(obj, "Authors"); a != nil {
		if w := firstOf(a, "Works"); w != nil {
			if missing := unknownKeys(w, workType); len(missing) > 0 {
				t.Errorf("%s.Authors[0].Works[0]: unknown keys %v", name, missing)
			}
		}
	}
}
