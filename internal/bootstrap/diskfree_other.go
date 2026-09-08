//go:build !(linux || darwin || freebsd)

package bootstrap

// freeBytes is unavailable here; the download proceeds without a space check.
func freeBytes(string) (int64, bool) { return 0, false }
