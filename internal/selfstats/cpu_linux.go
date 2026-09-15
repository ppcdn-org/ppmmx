//go:build linux

package selfstats

import "syscall"

// readProcessCPUTime returns this process's total CPU time (user+system)
// consumed so far via getrusage(RUSAGE_SELF) - the same underlying source
// systemd itself uses for its own "Consumed X CPU time" accounting.
func readProcessCPUTime() cpuTime {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return cpuTime{}
	}
	toSeconds := func(tv syscall.Timeval) float64 {
		return float64(tv.Sec) + float64(tv.Usec)/1e6
	}
	return cpuTime{seconds: toSeconds(ru.Utime) + toSeconds(ru.Stime), ok: true}
}
