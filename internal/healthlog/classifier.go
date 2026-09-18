package healthlog

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bluenviron/mediamtx/internal/logger"
)

// The node's log messages are pre-formatted strings (the logger only carries
// format+args), so the collector classifies by the stable bracketed tags and
// markers the producers already emit. Only entries that match one of these
// shapes become events; everything else is ignored, which is what keeps the
// reported volume (and cost) bounded.
var (
	streamPathPattern = regexp.MustCompile(`path=([^\s]+)`)
	layersPattern     = regexp.MustCompile(`layers=(\d+)`)
	bitratePattern    = regexp.MustCompile(`bitrate=(\d+)%`)
)

// healthEventMarkers are the bracketed tags/markers the producers emit. The
// collector checks the format string against these before formatting, so an
// unrelated log line costs only a few substring scans on the hot path.
var healthEventMarkers = []string{
	"[degrade]",
	"[publish-stats]",
	"[upload]",
	"[publish-whitelist]",
	"SRT unrecovered loss=",
	"reclaim disk space",
	"decode error",
	"processing errors",
}

// mightBeHealthEvent is a cheap pre-filter; Classify remains the authority.
func mightBeHealthEvent(format string) bool {
	for _, marker := range healthEventMarkers {
		if strings.Contains(format, marker) {
			return true
		}
	}
	return false
}

// Classify maps one formatted log line to a health event. ok=false means the
// line is not a recognised health signal and must not be reported.
func Classify(t time.Time, level logger.Level, message string) (Event, bool) {
	message = strings.TrimSpace(message)
	if message == "" {
		return Event{}, false
	}

	category, code := classifyCategory(message)
	if category == "" {
		return Event{}, false
	}

	event := Event{
		EventID:  newEventID(),
		Ts:       t.UTC().Format(time.RFC3339Nano),
		Level:    levelString(level),
		Category: category,
		Code:     code,
		Message:  truncateRunes(message, 512),
	}
	if m := streamPathPattern.FindStringSubmatch(message); len(m) == 2 {
		event.StreamPath = strings.Trim(m[1], "/ ")
	}
	if category == CategoryDegradeTransition || category == CategoryDegradeAlert {
		fields := map[string]any{}
		if m := layersPattern.FindStringSubmatch(message); len(m) == 2 {
			if v, err := strconv.Atoi(m[1]); err == nil {
				fields["layers"] = v
			}
		}
		if m := bitratePattern.FindStringSubmatch(message); len(m) == 2 {
			if v, err := strconv.Atoi(m[1]); err == nil {
				fields["bitratePercent"] = v
			}
		}
		if len(fields) > 0 {
			event.Fields = fields
		}
	}
	return event, true
}

func classifyCategory(message string) (category, code string) {
	switch {
	case strings.Contains(message, "[degrade]"):
		switch {
		case strings.Contains(message, "still non-compliant, giving up"):
			return CategoryDegradeAlert, "terminal"
		case strings.Contains(message, "recovering ->"):
			return CategoryDegradeTransition, "recover"
		case strings.Contains(message, "-> layers="):
			return CategoryDegradeTransition, "step_down"
		case strings.Contains(message, "max layers set to"):
			return CategoryDegradeTransition, "max_layers_set"
		case strings.Contains(message, "WS push failed"):
			return CategoryNodeControlOutage, "degrade_push_failed"
		case strings.Contains(message, "executor connection rejected"):
			return CategoryNodeControlOutage, "executor_auth_reject"
		default:
			return "", ""
		}
	case strings.Contains(message, "[publish-stats]"):
		if strings.Contains(message, "reconnected") {
			return CategoryPublishReconnect, "reconnected"
		}
		return "", ""
	case strings.HasPrefix(message, "SRT unrecovered loss=") && strings.Contains(message, "forcing disconnect"):
		// The forced reconnect that follows a sustained SRT loss burst is a
		// publisher reconnect from the platform's point of view.
		return CategoryPublishReconnect, "srt_loss_disconnect"
	case strings.Contains(message, "[upload]"):
		switch {
		case strings.Contains(message, "failed (attempt") || strings.Contains(message, "gave up after"):
			return CategoryRecordUploadFail, "upload_failed"
		case strings.Contains(message, "report to ppcenter failed"):
			return CategoryNodeControlOutage, "upload_report_failed"
		default:
			return "", ""
		}
	case strings.Contains(message, "[publish-whitelist]") && strings.Contains(message, "app sync fetch failed"):
		return CategoryNodeControlOutage, "app_sync_failed"
	case strings.Contains(message, "reclaim disk space"):
		return CategoryRecordDiskHighWater, "reclaim"
	case strings.Contains(message, "decode error") || strings.Contains(message, "processing errors"):
		// errordumper aggregation output (see internal/errordumper): a
		// catch-all signal that a stream is producing decode/processing
		// errors even when no loss alarm tripped.
		return CategoryNodeErrorDump, "media_decode"
	default:
		return "", ""
	}
}

func levelString(level logger.Level) string {
	switch level {
	case logger.Debug:
		return "debug"
	case logger.Warn:
		return "warn"
	case logger.Error:
		return "error"
	default:
		return "info"
	}
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}
