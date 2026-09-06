package server

import "testing"

func TestModePath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode, query string
		want        string
		wantErr     bool
	}{
		{"search", "name of the wind", "/search?q=name+of+the+wind", false},
		{"", "a b&c=d/e?", "/search?q=a+b%26c%3Dd%2Fe%3F", false},
		{"search", "<script>", "/search?q=%3Cscript%3E", false},
		{"work", "42", "/work/42", false},
		{"work", " 42 ", "/work/42", false},
		{"author", "7", "/author/7", false},
		{"book", "30001", "/book/30001", false},
		{"work", "abc", "", true},
		{"work", "42abc", "", true},
		{"author", "0", "", true},
		{"book", "-5", "", true},
		{"work", "../etc", "", true},
		{"work", "", "", true},
		{"bogus", "1", "", true},
	}
	for _, c := range cases {
		got, err := modePath(c.mode, c.query)
		if (err != nil) != c.wantErr {
			t.Errorf("modePath(%q, %q) err = %v, wantErr %v", c.mode, c.query, err, c.wantErr)
			continue
		}
		if got != c.want {
			t.Errorf("modePath(%q, %q) = %q, want %q", c.mode, c.query, got, c.want)
		}
	}
}

func TestReleaseYear(t *testing.T) {
	t.Parallel()
	if releaseYear("2007-03-27") != "2007" || releaseYear("200") != "" || releaseYear("") != "" {
		t.Error("releaseYear")
	}
}
