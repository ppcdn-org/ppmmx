//go:build linux

package selfstats

import "syscall"

// diskUsagePercent returns the percentage (0-100) of space used on the
// filesystem containing path. Computed as (Blocks-Bfree)/Blocks, i.e. true
// physical usage - slightly different from `df`'s own "Use%" column, which
// excludes the root-reserved block margin from its denominator, but close
// enough for a monitoring signal.
func diskUsagePercent(path string) (float64, bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil || stat.Blocks == 0 {
		return 0, false
	}
	used := stat.Blocks - stat.Bfree
	return float64(used) / float64(stat.Blocks) * 100, true
}
