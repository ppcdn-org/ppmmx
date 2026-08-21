//go:build !windows

package recordcleaner

import "syscall"

// freeBytes returns the number of free bytes on the filesystem containing
// path, as reported to a non-root user (Bavail, not Bfree - consistent
// with `df`'s "Avail" column).
func freeBytes(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}
