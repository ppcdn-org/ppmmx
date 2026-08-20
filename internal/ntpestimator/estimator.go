// Package ntpestimator contains a NTP estimator.
package ntpestimator

import (
	"time"
)

var timeNow = time.Now

// Estimator is a NTP estimator.
type Estimator struct {
	ClockRate int

	refNTP time.Time
	refPTS int64
}

// Estimate returns estimated NTP based on PTS (encoding time),
// not on frame arrival time. This ensures the viewer plays at
// the correct speed even when frames arrive irregularly.
func (e *Estimator) Estimate(pts int64) time.Time {
	now := timeNow()
	now = now.Round(0)

	if e.refNTP.IsZero() {
		e.refNTP = now
		e.refPTS = pts
		return now
	}

	// Compute NTP from PTS timeline (encoding clock)
	// Never re-anchor a running stream to wall clock: doing so makes RTP/NTP
	// mappings jump and causes WebRTC playout to move backwards or forwards.
	return e.refNTP.Add(time.Duration(pts-e.refPTS) * time.Second / time.Duration(e.ClockRate))
}

// DebugInfo returns internal state for diagnostics.
func (e *Estimator) DebugInfo() (refNTP time.Time, refPTS int64, clockRate int) {
	return e.refNTP, e.refPTS, e.ClockRate
}
