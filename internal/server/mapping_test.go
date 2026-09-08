package server

import (
	"testing"
)

func TestBestAuthorID(t *testing.T) {
	t.Parallel()
	contrib := func(id *int64, role *string) struct {
		AuthorID     *int64  `json:"author_id"`
		Contribution *string `json:"contribution"`
	} {
		return struct {
			AuthorID     *int64  `json:"author_id"`
			Contribution *string `json:"contribution"`
		}{id, role}
	}
	cases := []struct {
		name   string
		book   rawBook
		wantID int64
	}{
		{"no contributions", rawBook{}, 0},
		{"nil author id skipped", rawBook{Contributions: append(rawBook{}.Contributions, contrib(nil, nil))}, 0},
		{"empty role is author", rawBook{Contributions: append(rawBook{}.Contributions, contrib(ptr[int64](5), nil))}, 5},
		{"explicit author", rawBook{Contributions: append(rawBook{}.Contributions, contrib(ptr[int64](6), ptr("Author")))}, 6},
		{"case and spaces", rawBook{Contributions: append(rawBook{}.Contributions, contrib(ptr[int64](7), ptr("  AUTHOR ")))}, 7},
		{"author/narrator", rawBook{Contributions: append(rawBook{}.Contributions, contrib(ptr[int64](8), ptr("Author/Narrator")))}, 8},
		{"translator first is skipped", rawBook{Contributions: append(rawBook{}.Contributions,
			contrib(ptr[int64](9), ptr("Translator")), contrib(ptr[int64](10), ptr("Author")))}, 10},
		{"only non-author roles", rawBook{Contributions: append(rawBook{}.Contributions,
			contrib(ptr[int64](11), ptr("Illustrator")), contrib(ptr[int64](12), ptr("Narrator")))}, 0},
	}
	for _, c := range cases {
		id, role := bestAuthorID(&c.book)
		if id != c.wantID {
			t.Errorf("%s: id = %d, want %d", c.name, id, c.wantID)
		}
		if wantRole := ""; c.wantID != 0 {
			wantRole = "Author"
			if role != wantRole {
				t.Errorf("%s: role = %q, want %q", c.name, role, wantRole)
			}
		} else if role != "" {
			t.Errorf("%s: role = %q, want empty", c.name, role)
		}
	}
}

func TestSplitTitle(t *testing.T) {
	t.Parallel()
	cases := []struct{ title, subtitle, short, full string }{
		{"Dune", "", "Dune", "Dune"},
		{"The Wise Man's Fear: Day Two", "Day Two", "The Wise Man's Fear", "The Wise Man's Fear: Day Two"},
		{"Plain", "Sub", "Plain", "Plain: Sub"},
		// Subtitle that is not embedded with ": " is simply appended.
		{"Plain - Sub", "Sub", "Plain - Sub", "Plain - Sub: Sub"},
		{"", "", "", ""},
	}
	for _, c := range cases {
		short, full := splitTitle(c.title, c.subtitle)
		if short != c.short || full != c.full {
			t.Errorf("splitTitle(%q, %q) = (%q, %q), want (%q, %q)", c.title, c.subtitle, short, full, c.short, c.full)
		}
	}
}

func TestReleaseDate(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"2007-03-27", "2007-03-27"},
		{"0001-01-01", "0001-01-01"},
		{"9999-12-31", "9999-12-31"},
		{"", ""},
		{"2007", ""},
		{"2007-03", ""},
		{"2007-02-30", ""}, // no such day
		{"2007-13-01", ""},
		{"March 27, 2007", ""},
		{"2007-03-27T00:00:00", ""},
		{"10000-01-01", ""},
		{"-0500-01-01", ""},
		{"800 BC", "0001-01-01"},
		{"0300-01-01 BC", "0001-01-01"},
	}
	for _, c := range cases {
		if got := releaseDate(c.in); got != c.want {
			t.Errorf("releaseDate(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEditionFormat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		e       rawEdition
		format  string
		isEbook bool
	}{
		{"edition_format wins", rawEdition{EditionFormat: "Hardcover", FormatID: formatEbook, Physical: "x"}, "Hardcover", true},
		{"ebook string", rawEdition{EditionFormat: "ebook"}, "ebook", true},
		{"kindle string", rawEdition{EditionFormat: "Kindle Edition", FormatID: formatPhysical}, "Kindle Edition", true},
		{"paperback string", rawEdition{EditionFormat: "Paperback", FormatID: formatPhysical}, "Paperback", false},
		{"fallback ebook id", rawEdition{FormatID: formatEbook}, "ebook", true},
		{"fallback audio id", rawEdition{FormatID: formatAudio, Physical: "CD"}, "Audiobook", false},
		{"fallback physical", rawEdition{FormatID: formatPhysical, Physical: "Mass Market Paperback"}, "Mass Market Paperback", false},
		{"nothing known", rawEdition{}, "", false},
	}
	for _, c := range cases {
		f, e := editionFormat(&c.e)
		if f != c.format || e != c.isEbook {
			t.Errorf("%s: editionFormat = (%q, %v), want (%q, %v)", c.name, f, e, c.format, c.isEbook)
		}
	}
}

func TestPickEdition(t *testing.T) {
	t.Parallel()
	if pickEdition(&rawBook{}, 0) != nil {
		t.Error("no editions should give nil")
	}
	b := &rawBook{Editions: []rawEdition{{ID: 1}, {ID: 2}, {ID: 3}}}
	if e := pickEdition(b, 0); e.ID != 1 {
		t.Errorf("default = %d, want first", e.ID)
	}
	if e := pickEdition(b, 3); e.ID != 3 {
		t.Errorf("explicit = %d, want 3", e.ID)
	}
	if e := pickEdition(b, 99); e.ID != 1 {
		t.Errorf("unknown = %d, want fallback to first", e.ID)
	}
	if e := pickEdition(b, 2); e != &b.Editions[1] {
		t.Error("should return a pointer into the slice, not a copy")
	}
}

func TestISO639_3(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"en": "eng", "de": "ger", "fr": "fre", "zh": "chi", "ja": "jpn",
		"": "", "xx": "xx", "eng": "eng",
	}
	for in, want := range cases {
		if got := iso639_3(in); got != want {
			t.Errorf("iso639_3(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOrNA(t *testing.T) {
	t.Parallel()
	if orNA("") != "N/A" || orNA("x") != "x" {
		t.Error("orNA")
	}
}

func TestCachedTagsUnmarshal(t *testing.T) {
	t.Parallel()
	var b rawBook
	if err := b.CachedTags.UnmarshalJSON([]byte(`{"Genre":[{"tag":"Fantasy","count":3},{"tag":"","count":1}],"Mood":[{"tag":"dark"}]}`)); err != nil {
		t.Fatal(err)
	}
	if len(b.CachedTags.Genres) != 1 || b.CachedTags.Genres[0] != "Fantasy" {
		t.Errorf("genres = %v", b.CachedTags.Genres)
	}
	// Other shapes are tolerated silently.
	for _, raw := range []string{`[]`, `"str"`, `null`, `{"Genre":"oops"}`, `{"Genre":[1,2]}`} {
		var c cachedTags
		if err := c.UnmarshalJSON([]byte(raw)); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

func TestWorkResource(t *testing.T) {
	t.Parallel()
	st := openFixture(t)
	a := &app{store: st}

	b, err := st.work(fxWorkWiseMan)
	if err != nil {
		t.Fatal(err)
	}
	w, err := a.workResource(b, 0, map[int64]*rawSeries{})
	if err != nil {
		t.Fatal(err)
	}
	if w.ForeignID != fxWorkWiseMan || w.Title != "The Wise Man's Fear" || w.FullTitle != "The Wise Man's Fear: The Kingkiller Chronicle Day Two" {
		t.Errorf("titles: %q / %q", w.Title, w.FullTitle)
	}
	if len(w.Authors) != 1 || w.Authors[0].ForeignID != fxAuthorRothfuss {
		t.Errorf("translator credited first must not win: %+v", w.Authors)
	}
	if len(w.Authors[0].Works) != 1 || len(w.Authors[0].Works[0].Books) != 0 {
		t.Error("nested author work should be a bare copy without relations")
	}
	if len(w.Books) != 1 || w.Books[0].ForeignID != fxEditionWiseMan || w.Books[0].Asin != fxASINWiseMan || w.Books[0].Language != "eng" {
		t.Errorf("book: %+v", w.Books)
	}
	if w.Books[0].Format != "Paperback" || w.Books[0].IsEbook || w.Books[0].Publisher != "DAW Books" || w.Books[0].NumPages != 662 {
		t.Errorf("book format fields: %+v", w.Books[0])
	}
	if len(w.Books[0].Contributors) != 1 || w.Books[0].Contributors[0].Role != "Author" {
		t.Errorf("contributors: %+v", w.Books[0].Contributors)
	}
	if len(w.Series) != 1 || w.Series[0].ForeignID != fxSeriesKingkiller || w.Series[0].LinkItems[0].PositionInSeries != "2" || w.Series[0].LinkItems[0].SeriesPosition != 2 {
		t.Errorf("series: %+v", w.Series)
	}
	if w.RatingCount != 3000 || w.AverageRating != 4.4 || w.RatingSum != int64(3000*4.4) {
		t.Errorf("ratings: %d %v %d", w.RatingCount, w.AverageRating, w.RatingSum)
	}
	if w.Genres[0] != "Fantasy" || w.RelatedWorks == nil || w.URL != "https://hardcover.app/books/the-wise-mans-fear" {
		t.Errorf("misc: %+v", w)
	}

	// Explicit edition selection, and a work with no genres gets "none".
	b, _ = st.work(fxWorkNameOfWind)
	w, err = a.workResource(b, fxEditionNOTWKind, map[int64]*rawSeries{})
	if err != nil {
		t.Fatal(err)
	}
	if w.Books[0].ForeignID != fxEditionNOTWKind || !w.Books[0].IsEbook || w.Books[0].Format != "Kindle Edition" {
		t.Errorf("kindle edition: %+v", w.Books[0])
	}
	if w.BestBookID != fxEditionNOTWHC {
		t.Errorf("BestBookId should stay the most-shelved edition, got %d", w.BestBookID)
	}
	b.CachedTags.Genres = nil
	w, _ = a.workResource(b, 0, map[int64]*rawSeries{})
	if len(w.Genres) != 1 || w.Genres[0] != "none" {
		t.Errorf("genres fallback = %v", w.Genres)
	}

	// Failure modes.
	if _, err := a.workResource(&rawBook{ID: 1}, 0, nil); err == nil {
		t.Error("no primary author should fail")
	}
	noEd := *b
	noEd.Editions = nil
	if _, err := a.workResource(&noEd, 0, map[int64]*rawSeries{}); err == nil {
		t.Error("no editions should fail")
	}
}

func TestAuthorResource(t *testing.T) {
	t.Parallel()
	st := openFixture(t)
	a := &app{store: st}

	ar, err := a.authorResource(fxAuthorRothfuss, 50)
	if err != nil {
		t.Fatal(err)
	}
	if ar.Name != "Patrick Rothfuss" || ar.Description != "Author of the Kingkiller Chronicle." {
		t.Errorf("author: %+v", ar)
	}
	if len(ar.Works) != 3 || ar.Works[0].ForeignID != fxWorkNameOfWind {
		t.Errorf("works = %d, first %d", len(ar.Works), ar.Works[0].ForeignID)
	}
	if len(ar.Series) != 1 || len(ar.Series[0].LinkItems) != 2 {
		t.Errorf("series links should be aggregated across works: %+v", ar.Series)
	}
	if ar.RatingCount != 8000 {
		t.Errorf("RatingCount = %d, want 8000", ar.RatingCount)
	}
	if ar.AverageRating < 4.46 || ar.AverageRating > 4.47 {
		t.Errorf("AverageRating = %v", ar.AverageRating)
	}

	// Sanderson: Elantris only; the translator credit is not a primary work.
	ar, err = a.authorResource(fxAuthorSanderson, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(ar.Works) != 1 || ar.Works[0].ForeignID != fxWorkElantris || ar.Description != "N/A" {
		t.Errorf("sanderson: %+v", ar)
	}
	if ar.Series == nil || len(ar.Series) != 0 {
		t.Errorf("series should be an empty, non-nil list: %#v", ar.Series)
	}

	if _, err := a.authorResource(4242, 50); err == nil {
		t.Error("unknown author should fail")
	}
	if ids, _ := a.authorResource(fxAuthorRothfuss, 1); len(ids.Works) != 1 {
		t.Error("maxWorks not honoured")
	}
}
