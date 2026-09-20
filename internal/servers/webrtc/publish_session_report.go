package webrtc

import (
	"context"
	"time"
)

// publishSessionReporter is the subset of mmxcontrol.PublishSessionClient
// that session.runPublish needs, narrowed to an interface so this package
// doesn't import mmxcontrol (same reasoning as rtpLossAlarmReporter) -
// internal/core bridges the two.
//
// This is what makes a publish session's start and end observable to
// ppcenter. Unlike traffic usage, which is sampled periodically and can
// tolerate a missed tick, these two calls are the only record that a stream
// ever went live, so they are reported at the exact lifecycle boundaries
// rather than polled.
type publishSessionReporter interface {
	ReportPublishStart(ctx context.Context, sessionID, pathName, remoteAddr, userAgent string, startedAt time.Time) error
	ReportPublishEnd(ctx context.Context, sessionID string, endedAt time.Time, endReason string, inboundBytes uint64) error
	// ReportPublishEndAsync is the teardown path: non-blocking, but counted by
	// the reporter so a graceful shutdown can drain it (see core's shutdown).
	ReportPublishEndAsync(sessionID string, endedAt time.Time, endReason string, inboundBytes uint64, onError func(error))
}

// publishSessionReporterHook implements the sessionParent hook, returning
// the configured reporter (nil when MMXControl isn't set up - see core.go's
// construction gating). Named with a "Hook" suffix to avoid colliding with
// the publishSessionReporter interface name above.
func (s *Server) publishSessionReporterHook() publishSessionReporter {
	return s.PublishSessionReporter
}
