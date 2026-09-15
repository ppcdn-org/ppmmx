package srt

import "context"

// srtLossAlarmReporter is the subset of mmxcontrol.SRTLossAlarmClient that
// runReceiveStatsSummary needs, narrowed to an interface so this package
// doesn't import mmxcontrol (matching how recording's SplitRecFileReporter
// decouples internal/servers packages from internal/mmxcontrol) - internal/
// core bridges the two.
type srtLossAlarmReporter interface {
	ReportSRTLoss(ctx context.Context, pathName string, lossPct, bitrateBps float64, sustainedSec int) error
}
