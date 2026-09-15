// Package selfstats periodically logs mmx's own process-level resource
// usage - CPU%, memory, goroutine count, and disk usage of its working
// directory's filesystem - so operators can spot resource exhaustion from
// the same log stream as everything else (journalctl/log file), without
// needing to SSH in and run top/df/ps by hand. Unlike systemd's own
// "Consumed X CPU time, Y memory peak" line (a one-shot summary printed
// only when the service stops), this is a live, repeating time series.
package selfstats

import (
	"fmt"
	"runtime"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"
)

// Interval is the default cadence a Reporter logs at.
const Interval = 90 * time.Second

// Reporter periodically logs a single [selfstats] line. Create one with
// NewReporter, Start it once at process startup, and Stop it on shutdown -
// it owns exactly one goroutine for its lifetime.
type Reporter struct {
	log      logger.Writer
	diskPath string
	interval time.Duration

	stop chan struct{}
	done chan struct{}
}

// NewReporter creates a Reporter. diskPath is the filesystem path whose
// disk usage gets reported - typically mmx's own working directory ("."),
// since that's where its data/recordings/logs actually accumulate.
func NewReporter(log logger.Writer, diskPath string, interval time.Duration) *Reporter {
	return &Reporter{
		log:      log,
		diskPath: diskPath,
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start begins the periodic logging loop in a new goroutine.
func (r *Reporter) Start() {
	go r.run()
}

// Stop ends the logging loop and waits for its goroutine to exit.
func (r *Reporter) Stop() {
	close(r.stop)
	<-r.done
}

func (r *Reporter) run() {
	defer close(r.done)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	lastCPU := readProcessCPUTime()
	lastAt := time.Now()

	for {
		select {
		case <-r.stop:
			return
		case now := <-ticker.C:
			cpu := readProcessCPUTime()
			cpuPct := "n/a"
			if cpu.ok && lastCPU.ok {
				if elapsed := now.Sub(lastAt).Seconds(); elapsed > 0 {
					cpuPct = fmt.Sprintf("%.1f%%", (cpu.seconds-lastCPU.seconds)/elapsed*100)
				}
			}
			lastCPU, lastAt = cpu, now

			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)

			diskPct := "n/a"
			if used, ok := diskUsagePercent(r.diskPath); ok {
				diskPct = fmt.Sprintf("%.1f%%", used)
			}

			r.log.Log(logger.Info, "[selfstats] cpu=%s mem_alloc=%s mem_sys=%s goroutines=%d disk=%s",
				cpuPct, formatBytes(mem.Alloc), formatBytes(mem.Sys), runtime.NumGoroutine(), diskPct)
		}
	}
}

func formatBytes(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2fGB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// cpuTime is this process's cumulative CPU time consumed so far (user+
// system), platform-sourced - see cpu_linux.go/cpu_other.go. ok is false
// when the platform doesn't support reading it (e.g. local Windows dev);
// callers must not compute a rate from an !ok sample.
type cpuTime struct {
	seconds float64
	ok      bool
}
