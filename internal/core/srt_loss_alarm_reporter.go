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
type srtLossAlarmReporterAdapter struct {
	client *mmxcontrol.SRTLossAlarmClient
}

func (a srtLossAlarmReporterAdapter) ReportSRTLoss(ctx context.Context, pathName string, lossPct, bitrateBps float64, sustainedSec int) error {
	return a.client.Report(ctx, mmxcontrol.SRTLossAlarmReport{
		PathName:     pathName,
		LossPct:      lossPct,
		BitrateBps:   bitrateBps,
		SustainedSec: sustainedSec,
	})
}
