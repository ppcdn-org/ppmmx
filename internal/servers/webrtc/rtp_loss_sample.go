package webrtc

import "context"

// rtpLossSampleReporter is the subset of mmxcontrol.RTPLossSampleClient that
// session.runReceiveStatsSummary needs, narrowed to an interface so this
// package doesn't import mmxcontrol (same reasoning as rtpLossAlarmReporter)
// - internal/core bridges the two.
type rtpLossSampleReporter interface {
	ReportRTPLossSample(ctx context.Context, pathName string, lossPct, bitrateBps float64, minuteUTC string) error
}

// rtpLossSampleReporterHook implements the sessionParent hook, returning the
// configured reporter (nil when MMXControl isn't set up - see core.go's
// construction gating).
func (s *Server) rtpLossSampleReporterHook() rtpLossSampleReporter {
	return s.RTPLossSampleReporter
}
