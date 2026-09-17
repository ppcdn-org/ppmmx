package core

import (
	"context"

	"github.com/bluenviron/mediamtx/internal/mmxcontrol"
)

// srtLossAlarmReporterAdapter satisfies srt.srtLossAlarmReporter by
// forwarding to a *mmxcontrol.SRTLossAlarmClient. The srt and mmxcontrol
// packages don't import each other (same reasoning as
// splitRecFileReporterAdapter above), so core - which already depends on
// both - is where the conversion happens.
//
// The pct parameter carries the UNRECOVERABLE loss rate, not raw SRT loss -
// see mmxcontrol.SRTLossAlarmReport and conn.go's call site.
type srtLossAlarmReporterAdapter struct {
	client *mmxcontrol.SRTLossAlarmClient
}

func (a srtLossAlarmReporterAdapter) ReportSRTLoss(ctx context.Context, pathName string, unrecoveredPct, bitrateBps float64, sustainedSec int) error {
	return a.client.Report(ctx, mmxcontrol.SRTLossAlarmReport{
		PathName:       pathName,
		UnrecoveredPct: unrecoveredPct,
		BitrateBps:     bitrateBps,
		SustainedSec:   sustainedSec,
	})
}
