package textnorm

import "testing"

func TestName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"Sarah J. Maas", "sarah j maas"},
		{"sarah j maas", "sarah j maas"},
		{"  Brandon   SANDERSON ", "brandon sanderson"},
		{"N.K. Jemisin", "n k jemisin"},
		{"Émile Zola", "émile zola"},
		{"Война и мир", "война и мир"},
		{"!!!", ""},
		{"--Lead-Trail--", "lead trail"},
		{"Catch-22", "catch 22"},
		{"O'Brien", "o brien"},
	}
	for _, c := range cases {
		if got := Name(c.in); got != c.want {
			t.Errorf("Name(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
