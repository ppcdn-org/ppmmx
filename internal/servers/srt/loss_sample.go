package srt

import "context"

// srtLossSampleReporter is the subset of mmxcontrol.SRTLossSampleClient that
// runReceiveStatsSummary needs, narrowed to an interface so this package
// doesn't import mmxcontrol (same reasoning as srtLossAlarmReporter in
// loss_alarm.go) - internal/core bridges the two.
//
// Unlike srtLossAlarmReporter, which only fires when loss crosses a
// threshold, this is called every recvstats.Interval so the full trend is
// persisted, not just the moments that were bad enough to page someone.
//
// Both percentages are on the same basis (the interval's expected packet
// count, i.e. received+lost): recoverablePct is the loss ARQ retransmitted
// in time, unrecoverablePct the loss it did not. Their sum is the raw loss
// rate SRT reports.
type srtLossSampleReporter interface {
	ReportSRTLossSample(ctx context.Context, pathName string, recoverablePct, unrecoverablePct, bitrateBps float64, minuteUTC string) error
}
