package core

import (
	"context"
	"time"

	"github.com/bluenviron/mediamtx/internal/mmxcontrol"
)

// publishSessionReporterAdapter satisfies both webrtc's and srt's
// publishSessionReporter interfaces (they are structurally identical) by
// forwarding to a *mmxcontrol.PublishSessionClient. Neither server package
// imports mmxcontrol (same reasoning as rtpLossAlarmReporterAdapter), so core
// - which already depends on all three - is where the conversion happens.
type publishSessionReporterAdapter struct {
	client *mmxcontrol.PublishSessionClient
}

func (a publishSessionReporterAdapter) ReportPublishStart(
	ctx context.Context,
	sessionID, pathName, remoteAddr, userAgent string,
	startedAt time.Time,
) error {
	return a.client.ReportPublishStart(ctx, sessionID, pathName, remoteAddr, userAgent, startedAt)
}

func (a publishSessionReporterAdapter) ReportPublishEnd(
	ctx context.Context,
	sessionID string,
	endedAt time.Time,
	endReason string,
	inboundBytes uint64,
) error {
	return a.client.ReportPublishEnd(ctx, sessionID, endedAt, endReason, inboundBytes)
}

// ReportPublishEndAsync is what the servers call on teardown: non-blocking,
// but tracked by the client's in-flight set so a graceful shutdown can drain
// it. See PublishSessionClient.ReportPublishEndAsync.
func (a publishSessionReporterAdapter) ReportPublishEndAsync(
	sessionID string,
	endedAt time.Time,
	endReason string,
	inboundBytes uint64,
	onError func(error),
) {
	a.client.ReportPublishEndAsync(sessionID, endedAt, endReason, inboundBytes, onError)
}
