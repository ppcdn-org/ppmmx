package core

import (
	"context"
	"time"

	"github.com/bluenviron/mediamtx/internal/mmxcontrol"
)

// publishSessionReporterAdapter satisfies webrtc's publishSessionReporter by
// forwarding to a *mmxcontrol.PublishSessionClient. The webrtc and
// mmxcontrol packages don't import each other (same reasoning as
// rtpLossAlarmReporterAdapter), so core - which already depends on both - is
// where the conversion happens.
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
