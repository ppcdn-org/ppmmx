package core

import (
	"context"

	"github.com/bluenviron/mediamtx/internal/mmxcontrol"
)

// rtpLossAlarmReporterAdapter satisfies webrtc's rtpLossAlarmReporter by
// forwarding to a *mmxcontrol.RTPLossAlarmClient. The webrtc and mmxcontrol
// packages don't import each other (same reasoning as
// srtLossAlarmReporterAdapter), so core - which already depends on both - is
// where the conversion happens.
type rtpLossAlarmReporterAdapter struct {
	client *mmxcontrol.RTPLossAlarmClient
}

func (a rtpLossAlarmReporterAdapter) ReportRTPLoss(ctx context.Context, pathName string, lossPct, bitrateBps float64, sustainedSec int) error {
	return a.client.Report(ctx, mmxcontrol.RTPLossAlarmReport{
		PathName:     pathName,
		LossPct:      lossPct,
		BitrateBps:   bitrateBps,
		SustainedSec: sustainedSec,
	})
}
