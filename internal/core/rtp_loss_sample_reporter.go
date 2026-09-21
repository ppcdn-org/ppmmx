package core

import (
	"context"

	"github.com/bluenviron/mediamtx/internal/mmxcontrol"
)

// rtpLossSampleReporterAdapter satisfies webrtc's rtpLossSampleReporter by
// forwarding to a *mmxcontrol.RTPLossSampleClient. The webrtc and mmxcontrol
// packages don't import each other (same reasoning as
// rtpLossAlarmReporterAdapter), so core - which already depends on both - is
// where the conversion happens.
type rtpLossSampleReporterAdapter struct {
	client *mmxcontrol.RTPLossSampleClient
}

func (a rtpLossSampleReporterAdapter) ReportRTPLossSample(ctx context.Context, pathName string, lossPct, bitrateBps float64, minuteUTC string) error {
	return a.client.Report(ctx, mmxcontrol.RTPLossSampleReport{
		PathName:   pathName,
		LossPct:    lossPct,
		BitrateBps: bitrateBps,
		MinuteUTC:  minuteUTC,
	})
}
