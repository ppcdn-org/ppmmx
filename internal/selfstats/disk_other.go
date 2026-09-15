//go:build !linux

package selfstats

// diskUsagePercent is unsupported outside Linux (fine for local
// Windows/macOS dev - production always runs Linux).
func diskUsagePercent(path string) (float64, bool) {
	return 0, false
}
