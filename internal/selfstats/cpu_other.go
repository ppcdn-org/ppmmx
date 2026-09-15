//go:build !linux

package selfstats

// readProcessCPUTime is unsupported outside Linux (fine for local
// Windows/macOS dev - production always runs Linux); returns ok=false so
// callers report "n/a" instead of a bogus rate.
func readProcessCPUTime() cpuTime {
	return cpuTime{}
}
