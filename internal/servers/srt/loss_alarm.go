package srt

import "context"

// srtLossAlarmReporter is the subset of mmxcontrol.SRTLossAlarmClient that
// runReceiveStatsSummary needs, narrowed to an interface so this package
// doesn't import mmxcontrol (matching how recording's SplitRecFileReporter
// decouples internal/servers packages from internal/mmxcontrol) - internal/
// core bridges the two.
//
// The pct argument is the UNRECOVERABLE loss rate (packets lost and never
// retransmitted), not raw SRT loss - see runReceiveStatsSummary's call site.
type srtLossAlarmReporter interface {
	ReportSRTLoss(ctx context.Context, pathName string, unrecoveredPct, bitrateBps float64, sustainedSec int) error
}
