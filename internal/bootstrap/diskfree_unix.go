//go:build linux || darwin || freebsd

package bootstrap

import "syscall"

// freeBytes reports the space available to this process in dir, or false
// when the filesystem cannot say.
func freeBytes(dir string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return int64(st.Bavail) * int64(st.Bsize), true
}
