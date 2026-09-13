package srt

import (
	"errors"
	"fmt"
	"strings"
	"time"

	mcmpegts "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"

	"github.com/bluenviron/mediamtx/internal/protocols/mpegts"
	"github.com/bluenviron/mediamtx/internal/protocols/publishtoken"
)

// pathCodecFromName returns the codec a publish path's trailing segment
// requires ("h264"/"hevc" - see the design doc's
// /{appId}/{streamName}/{codecType} convention, which SRT expresses through
// the streamID's pathname), or "" when the path carries no codec segment
// and is therefore unconstrained.
//
// Deliberately mirrors the WHIP server's pathCodecFromName so the two
// ingest protocols agree on what a codec path means. Anything that isn't a
// known codec is treated as a plain stream name, not as an invalid codec:
// "demo/av1" is a stream called "av1", not a rejected AV1 path.
func pathCodecFromName(pathName string) string {
	switch {
	case strings.HasSuffix(pathName, "/h264"):
		return "h264"
	case strings.HasSuffix(pathName, "/hevc"):
		return "hevc"
	default:
		return ""
	}
}

// checkTracksMatchPathCodec rejects a publish whose video codec doesn't match
// the codec its path names. Without this a HEVC ladder could land on the
// /h264 path, where players negotiating H264 would find nothing decodable.
//
// videoTracks is ValidateVideoTracks' output, so it is already known to be
// non-empty-or-absent and single-codec; this only compares it against the
// path. An audio-only publish (no video tracks) is left alone - there is no
// video codec to contradict the path.
func checkTracksMatchPathCodec(pathName string, videoTracks []*mcmpegts.Track) error {
	expected := pathCodecFromName(pathName)
	if expected == "" || len(videoTracks) == 0 {
		return nil
	}

	actual := mpegts.VideoCodecName(videoTracks[0].Codec)
	if actual != expected {
		return fmt.Errorf("path %s requires %s video, but the publish carries %s (PID %d is %T)",
			pathName, expected, actualOrUnknown(actual), videoTracks[0].PID, videoTracks[0].Codec)
	}

	return nil
}

func actualOrUnknown(codec string) string {
	if codec == "" {
		return "an unsupported codec"
	}
	return codec
}

// errTokenUncheckable is returned when a publish carries a token but the
// node has no auth key to verify it with. The publish is still allowed (see
// checkPublishToken); this exists so the caller can say so in the log
// instead of leaving the impression the token was verified.
var errTokenUncheckable = errors.New("ignoring publish token: no publish auth key is configured")

// checkPublishToken validates the ppcenter publish token carried in the
// streamID against the path being published to, using the same token format,
// key and codec-binding rule as the WHIP server (see publishtoken).
//
// A token bound to "app/stream/hevc" authenticates only that path - not the
// legacy 2-segment path, and not the other codec's path. When no token is
// present the publish falls back to the pre-existing streamID user/pass
// path, unless the deployment requires one.
//
// Returns errTokenUncheckable (not a rejection) when a token was offered but
// can't be checked - callers must treat that case as allowed.
func (c *conn) checkPublishToken(streamID *streamID) error {
	token := publishTokenFromStreamID(streamID)

	if token == "" {
		if c.publishTokenReq {
			return fmt.Errorf("a publish token is required but none was provided")
		}
		return nil
	}

	if c.publishAuthKey == "" {
		// A token was offered but this node can't check it. Failing closed
		// would break publishers that send one harmlessly, so accept and
		// let the caller log it rather than leaving the impression the
		// token authorized anything.
		return errTokenUncheckable
	}

	claims, err := publishtoken.Decrypt(c.publishAuthKey, token, time.Now())
	if err != nil {
		return fmt.Errorf("publish token authentication failure")
	}
	if !claims.PathMatches(streamID.path) {
		return fmt.Errorf("publish token authentication failure")
	}

	return nil
}

// publishTokenFromStreamID extracts the token from the streamID's optional
// third segment, accepting both a bare token and a "token=<value>" form so
// the segment can later carry other query parameters without breaking
// publishers that send the bare form.
func publishTokenFromStreamID(streamID *streamID) string {
	q := strings.TrimSpace(streamID.query)
	if q == "" {
		return ""
	}

	if !strings.Contains(q, "=") {
		return q
	}

	for _, kv := range strings.Split(q, "&") {
		name, value, found := strings.Cut(kv, "=")
		if found && name == "token" {
			return value
		}
	}

	return ""
}
