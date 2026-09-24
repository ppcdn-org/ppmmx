package webrtc

import "context"

// rtpLossAlarmReporter is the subset of mmxcontrol.RTPLossAlarmClient that
// session.runReceiveStatsSummary needs, narrowed to an interface so this
// package doesn't import mmxcontrol (same reasoning as srt's
// srtLossAlarmReporter) - internal/core bridges the two.
type rtpLossAlarmReporter interface {
	ReportRTPLoss(ctx context.Context, pathName string, lossPct, bitrateBps float64, sustainedSec int) error
}

// rtpLossAlarmEnabled implements the sessionParent hook.
func (s *Server) rtpLossAlarmEnabled() bool {
	return s.RTPLossAlarmEnable
}

// rtpLossAlarmThresholdPct implements the sessionParent hook.
func (s *Server) rtpLossAlarmThresholdPct() float64 {
	return s.RTPLossAlarmThresholdPct
}

// rtpLossAlarmReporterHook implements the sessionParent hook, returning the
// configured reporter (nil when RTPLossAlarmEnable is false or MMXControl
// isn't set up - see core.go's construction gating). Named with a "Hook"
// suffix to avoid colliding with the rtpLossAlarmReporter interface name
// above.
func (s *Server) rtpLossAlarmReporterHook() rtpLossAlarmReporter {
	return s.RTPLossAlarmReporter
}

// rtpLossRecycleEnabled implements the sessionParent hook.
func (s *Server) rtpLossRecycleEnabled() bool {
	return s.RTPLossRecycleEnable
}

// rtpLossRecycleThresholdPct implements the sessionParent hook.
func (s *Server) rtpLossRecycleThresholdPct() float64 {
	return s.RTPLossRecycleThresholdPct
}

// rtpLossRecycleSec implements the sessionParent hook.
func (s *Server) rtpLossRecycleSec() int {
	return s.RTPLossRecycleSec
}
