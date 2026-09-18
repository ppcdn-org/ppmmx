// Package healthlog filters the node's log stream down to structured health
// events and reports them in batches to ppcenter's
// POST /internal/mmx/v1/log-events. It is the node-side half of the AI
// health report's "log evidence" dimension - see
// docs/design/ppcdn-ai-live-health-report.zh-CN.md §5.2.3.
//
// The node only ever reports an event SUMMARY, never raw log text, and never
// blocks the media path: the collector uses a bounded in-memory queue and
// drops the oldest events (counting them) when full.
package healthlog

// Category is a stable enum shared with ppcenter (models.NodeLogCategory*).
// Existing values must never be renamed - the report's determinism depends on
// recognising a category across node and control-plane versions. A node may
// introduce a new category before ppcenter knows it; ppcenter stores it
// without rejecting the batch.
const (
	CategoryPublishSessionStart   = "publish.session.start"
	CategoryPublishSessionEnd     = "publish.session.end"
	CategoryPublishReconnect      = "publish.reconnect"
	CategoryPublishAuthReject     = "publish.auth.reject"
	CategoryDegradeTransition     = "degrade.transition"
	CategoryDegradeAlert          = "degrade.alert"
	CategorySRTLossSample         = "srt.loss.sample"
	CategoryEdgeOriginResolveFail = "edge.origin.resolve.fail"
	CategoryEdgeOriginReconnect   = "edge.origin.reconnect"
	CategoryRecordUploadFail      = "record.segment.upload.fail"
	CategoryRecordDiskHighWater   = "record.disk.high_water"
	CategoryNodeControlOutage     = "node.control.outage"
	CategoryNodeErrorDump         = "node.error.dump"
)

// Event is the wire structure reported to ppcenter, matching
// nodeLogEventPayload in ppcenter/internal/apis/node_log_event_v1.go.
type Event struct {
	// EventID is a node-generated UUID used by ppcenter as the idempotency
	// key, so a retried batch does not stack duplicate evidence.
	EventID string `json:"eventId"`
	// Ts is RFC3339 (nano) UTC.
	Ts string `json:"ts"`
	// Level is debug|info|warn|error.
	Level string `json:"level"`
	// Category is one of the Category* constants.
	Category string `json:"category"`
	// StreamPath is "appId/streamName[...]" for stream-scoped events, empty
	// for node-wide ones.
	StreamPath string `json:"streamPath,omitempty"`
	SessionID  string `json:"sessionId,omitempty"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message"`
	// Fields carries the small amount of structured context the classifier
	// could extract (e.g. layers, bitratePercent). ppcenter sanitizes it
	// again before storage.
	Fields map[string]any `json:"fields,omitempty"`
}
