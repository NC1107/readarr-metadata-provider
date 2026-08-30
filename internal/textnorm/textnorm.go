// Package textnorm normalizes names for matching: the dataset builder and
// the server must agree on this exactly, or lookups silently miss.
package textnorm

import (
	"strings"
	"unicode"
)

// Name lowercases, drops punctuation, and collapses whitespace, so
// "Sarah J. Maas" and "sarah j maas" compare equal.
func Name(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		default:
			space = true
		}
	}
	return b.String()
}
