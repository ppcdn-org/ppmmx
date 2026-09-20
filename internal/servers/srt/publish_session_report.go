package srt

import (
	"context"
	"time"

	srt "github.com/datarhei/gosrt"

	"github.com/bluenviron/mediamtx/internal/logger"
)

// publishSessionReporter is the subset of mmxcontrol.PublishSessionClient
// that a SRT publish connection needs, narrowed to an interface so this
// package doesn't import mmxcontrol (same reasoning as srtLossAlarmReporter /
// srtLossSampleReporter) - internal/core bridges the two.
//
// SRT ingest is instrumented exactly like WHIP ingest (see webrtc's
// publish_session_report.go): start and end are the only record that a
// stream ever went live, so they are reported at the lifecycle boundaries
// rather than sampled.
type publishSessionReporter interface {
	ReportPublishStart(ctx context.Context, sessionID, pathName, remoteAddr, userAgent string, startedAt time.Time) error
	ReportPublishEnd(ctx context.Context, sessionID string, endedAt time.Time, endReason string, inboundBytes uint64) error
	// ReportPublishEndAsync is the teardown path: non-blocking, but counted by
	// the reporter so a graceful shutdown can drain it (see core's shutdown).
	ReportPublishEndAsync(sessionID string, endedAt time.Time, endReason string, inboundBytes uint64, onError func(error))
}

// publishSessionReporterHook returns the configured reporter (nil when
// MMXControl isn't set up - see core.go's construction gating). Named with a
// "Hook" suffix to avoid colliding with the publishSessionReporter interface
// name above.
func (s *Server) publishSessionReporterHook() publishSessionReporter {
	return s.PublishSessionReporter
}

// reportPublishStart tells ppcenter that this SRT connection went live. The
// connection UUID is the correlation key: reportPublishEnd sends the same
// one, and ppcenter matches the two into a single history row.
//
// Fire-and-forget, like the WHIP path and the SRT loss alarm: a publish that
// is working must not be held up (or failed) because the control plane is
// briefly unreachable.
func (c *conn) reportPublishStart(pathName string) {
	reporter := c.parent.publishSessionReporterHook()
	if reporter == nil {
		return
	}

	sessionID := c.uuid.String()
	remoteAddr := c.connReq.RemoteAddr().String()
	startedAt := time.Now()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// SRT has no HTTP request, so there is no User-Agent to forward;
		// "SRT" is a protocol marker for the console/admin views.
		if err := reporter.ReportPublishStart(ctx, sessionID, pathName, remoteAddr, "SRT", startedAt); err != nil {
			c.Log(logger.Debug, "publish session start report failed: %v", err)
		}
	}()
}

// reportPublishEnd tells ppcenter the session stopped, carrying the total
// inbound bytes so the history row can show how much was actually ingested.
//
// Deliberately not using c.ctx: by the time this runs (deferred on the way
// out of runPublishReader) that context is typically already cancelled, which
// would abort the very request meant to close out the record. A fresh context
// with its own timeout is what makes the end event survive the session it
// describes.
func (c *conn) reportPublishEnd(sconn srt.Conn, pathName string) {
	reporter := c.parent.publishSessionReporterHook()
	if reporter == nil {
		return
	}

	sessionID := c.uuid.String()
	endedAt := time.Now()

	// Distinguishes a publisher that went away (or was disconnected by the
	// loss guard) from one this node tore down during shutdown, matching the
	// WHIP path's end reasons.
	endReason := "publisher_closed"
	select {
	case <-c.ctx.Done():
		endReason = "session_terminated"
	default:
	}

	var inboundBytes uint64
	if sconn != nil {
		var st srt.Statistics
		sconn.Stats(&st)
		inboundBytes = st.Accumulated.ByteRecv
	}

	// The report itself is fired asynchronously inside the reporter (so a slow
	// control plane can't hold up teardown) but registered synchronously, so
	// this node's graceful shutdown can wait for it - see core's exit path.
	reporter.ReportPublishEndAsync(sessionID, endedAt, endReason, inboundBytes, func(err error) {
		c.Log(logger.Debug, "publish session end report failed: %v", err)
	})
}
