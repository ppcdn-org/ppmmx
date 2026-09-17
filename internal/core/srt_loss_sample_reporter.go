package core

import (
	"context"

	"github.com/bluenviron/mediamtx/internal/mmxcontrol"
)

// srtLossSampleReporterAdapter satisfies srt.srtLossSampleReporter by
// forwarding to a *mmxcontrol.SRTLossSampleClient. The srt and mmxcontrol
// packages don't import each other (same reasoning as
// srtLossAlarmReporterAdapter), so core - which already depends on both - is
// where the conversion happens.
type srtLossSampleReporterAdapter struct {
	client *mmxcontrol.SRTLossSampleClient
}

func (a srtLossSampleReporterAdapter) ReportSRTLossSample(
	ctx context.Context,
	pathName string,
	recoverablePct, unrecoverablePct, bitrateBps float64,
	minuteUTC string,
) error {
	return a.client.Report(ctx, mmxcontrol.SRTLossSampleReport{
		PathName:         pathName,
		RecoverablePct:   recoverablePct,
		UnrecoverablePct: unrecoverablePct,
		BitrateBps:       bitrateBps,
		MinuteUTC:        minuteUTC,
	})
}
