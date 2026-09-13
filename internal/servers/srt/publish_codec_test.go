package srt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	mcmpegts "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/protocols/publishtoken"
)

const testPublishAuthKey = "test-publish-auth-key"

// makeTestPublishToken seals claims in the same AES-256-GCM wire format
// ppcenter's EncryptWHIPToken produces, so these tests don't depend on the
// ppcenter repo. codec is "" for a legacy 2-segment-path token.
func makeTestPublishToken(t *testing.T, authKey, appID, stream, codec string, exp time.Time) string {
	t.Helper()
	claims := publishtoken.Claims{
		AppID: appID, Stream: stream, Codec: codec,
		IAT: exp.Add(-time.Hour).Unix(), EXP: exp.Unix(),
	}
	plaintext, err := json.Marshal(claims)
	require.NoError(t, err)

	key := sha256.Sum256([]byte(authKey))
	block, err := aes.NewCipher(key[:])
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	nonce := make([]byte, gcm.NonceSize())
	_, err = rand.Read(nonce)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, plaintext, nil))
}

func validTestPublishToken(t *testing.T, appID, stream, codec string) string {
	t.Helper()
	return makeTestPublishToken(t, testPublishAuthKey, appID, stream, codec, time.Now().Add(time.Hour))
}

func TestPathCodecFromName(t *testing.T) {
	require.Equal(t, "h264", pathCodecFromName("testapp/teststream/h264"))
	require.Equal(t, "hevc", pathCodecFromName("testapp/teststream/hevc"))

	// no codec segment: unconstrained legacy path
	require.Equal(t, "", pathCodecFromName("testapp/teststream"))
	require.Equal(t, "", pathCodecFromName(""))

	// an unknown trailing segment is a stream name, not an invalid codec
	require.Equal(t, "", pathCodecFromName("testapp/av1"))
}

func TestCheckTracksMatchPathCodec(t *testing.T) {
	track := func(pid uint16, codec tscodecs.Codec) *mcmpegts.Track {
		return &mcmpegts.Track{PID: pid, Codec: codec}
	}
	h264 := []*mcmpegts.Track{track(100, &tscodecs.H264{})}
	h265 := []*mcmpegts.Track{track(100, &tscodecs.H265{})}

	t.Run("matching codec passes", func(t *testing.T) {
		require.NoError(t, checkTracksMatchPathCodec("app/stream/h264", h264))
		require.NoError(t, checkTracksMatchPathCodec("app/stream/hevc", h265))
	})

	t.Run("legacy path accepts either codec", func(t *testing.T) {
		require.NoError(t, checkTracksMatchPathCodec("app/stream", h264))
		require.NoError(t, checkTracksMatchPathCodec("app/stream", h265))
	})

	t.Run("mismatched codec is rejected", func(t *testing.T) {
		err := checkTracksMatchPathCodec("app/stream/hevc", h264)
		require.EqualError(t, err,
			"path app/stream/hevc requires hevc video, but the publish carries h264 (PID 100 is *codecs.H264)")

		err = checkTracksMatchPathCodec("app/stream/h264", h265)
		require.EqualError(t, err,
			"path app/stream/h264 requires h264 video, but the publish carries hevc (PID 100 is *codecs.H265)")
	})

	t.Run("audio-only publish has no video codec to contradict", func(t *testing.T) {
		require.NoError(t, checkTracksMatchPathCodec("app/stream/hevc", nil))
	})

	t.Run("unsupported video codec on a codec path is rejected", func(t *testing.T) {
		err := checkTracksMatchPathCodec("app/stream/h264",
			[]*mcmpegts.Track{track(100, &tscodecs.MPEG4Video{})})
		require.EqualError(t, err,
			"path app/stream/h264 requires h264 video, but the publish carries "+
				"an unsupported codec (PID 100 is *codecs.MPEG4Video)")
	})
}

func TestPublishTokenFromStreamID(t *testing.T) {
	require.Equal(t, "abc", publishTokenFromStreamID(&streamID{query: "abc"}))
	require.Equal(t, "abc", publishTokenFromStreamID(&streamID{query: "token=abc"}))
	require.Equal(t, "abc", publishTokenFromStreamID(&streamID{query: "x=1&token=abc"}))
	require.Equal(t, "", publishTokenFromStreamID(&streamID{query: ""}))
	require.Equal(t, "", publishTokenFromStreamID(&streamID{query: "x=1"}))
}

func TestCheckPublishToken(t *testing.T) {
	newConn := func(required bool, authKey string) *conn {
		return &conn{publishAuthKey: authKey, publishTokenReq: required}
	}

	legacy := validTestPublishToken(t, "testapp", "teststream", "")
	h264 := validTestPublishToken(t, "testapp", "teststream", "h264")
	hevc := validTestPublishToken(t, "testapp", "teststream", "hevc")

	for _, ca := range []struct {
		name    string
		token   string
		path    string
		allowed bool
	}{
		{"h264 token on h264 path", h264, "testapp/teststream/h264", true},
		{"hevc token on hevc path", hevc, "testapp/teststream/hevc", true},
		{"legacy token on legacy path", legacy, "testapp/teststream", true},

		{"h264 token on hevc path", h264, "testapp/teststream/hevc", false},
		{"hevc token on h264 path", hevc, "testapp/teststream/h264", false},
		{"legacy token on codec path", legacy, "testapp/teststream/h264", false},
		{"codec token on legacy path", h264, "testapp/teststream", false},
		{"token for another stream", h264, "testapp/otherstream/h264", false},
	} {
		t.Run(ca.name, func(t *testing.T) {
			c := newConn(false, testPublishAuthKey)
			err := c.checkPublishToken(&streamID{path: ca.path, query: ca.token})
			if ca.allowed {
				require.NoError(t, err)
			} else {
				require.EqualError(t, err, "publish token authentication failure")
			}
		})
	}

	t.Run("expired token is rejected", func(t *testing.T) {
		expired := makeTestPublishToken(t, testPublishAuthKey,
			"testapp", "teststream", "h264", time.Now().Add(-time.Minute))
		c := newConn(false, testPublishAuthKey)
		require.Error(t, c.checkPublishToken(
			&streamID{path: "testapp/teststream/h264", query: expired}))
	})

	t.Run("tampered token is rejected", func(t *testing.T) {
		tampered := []byte(h264)
		tampered[len(tampered)-1] ^= 0xff
		c := newConn(false, testPublishAuthKey)
		require.Error(t, c.checkPublishToken(
			&streamID{path: "testapp/teststream/h264", query: string(tampered)}))
	})

	t.Run("token signed with another key is rejected", func(t *testing.T) {
		foreign := makeTestPublishToken(t, "some-other-key",
			"testapp", "teststream", "h264", time.Now().Add(time.Hour))
		c := newConn(false, testPublishAuthKey)
		require.Error(t, c.checkPublishToken(
			&streamID{path: "testapp/teststream/h264", query: foreign}))
	})

	t.Run("no token is allowed unless required", func(t *testing.T) {
		c := newConn(false, testPublishAuthKey)
		require.NoError(t, c.checkPublishToken(&streamID{path: "testapp/teststream"}))

		c = newConn(true, testPublishAuthKey)
		require.EqualError(t, c.checkPublishToken(&streamID{path: "testapp/teststream"}),
			"a publish token is required but none was provided")
	})

	t.Run("token is ignored when no auth key is configured", func(t *testing.T) {
		c := newConn(false, "")
		// allowed, but surfaced so the caller can log it
		require.ErrorIs(t, c.checkPublishToken(
			&streamID{path: "testapp/teststream/h264", query: h264}), errTokenUncheckable)
	})
}
