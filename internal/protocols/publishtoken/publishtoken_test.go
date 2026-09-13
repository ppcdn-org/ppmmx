package publishtoken

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const authKey = "test-auth-key"

// seal reproduces ppcenter's EncryptWHIPToken so these tests don't depend on
// the ppcenter repo.
func seal(t *testing.T, key string, claims Claims) string {
	t.Helper()
	plaintext, err := json.Marshal(claims)
	require.NoError(t, err)

	k := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(k[:])
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	nonce := make([]byte, gcm.NonceSize())
	_, err = rand.Read(nonce)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(gcm.Seal(nonce, nonce, plaintext, nil))
}

func validClaims(codec string) Claims {
	now := time.Now()
	return Claims{
		UUID: "u1", AppID: "app", Stream: "stream", Codec: codec,
		IAT: now.Add(-time.Hour).Unix(), EXP: now.Add(time.Hour).Unix(),
	}
}

func TestExpectedPath(t *testing.T) {
	require.Equal(t, "app/stream", (&Claims{AppID: "app", Stream: "stream"}).ExpectedPath())
	require.Equal(t, "app/stream/hevc",
		(&Claims{AppID: "app", Stream: "stream", Codec: "hevc"}).ExpectedPath())
}

func TestPathMatches(t *testing.T) {
	legacy := &Claims{AppID: "app", Stream: "stream"}
	h264 := &Claims{AppID: "app", Stream: "stream", Codec: "h264"}
	hevc := &Claims{AppID: "app", Stream: "stream", Codec: "hevc"}

	require.True(t, legacy.PathMatches("app/stream"))
	require.True(t, h264.PathMatches("app/stream/h264"))
	require.True(t, hevc.PathMatches("app/stream/hevc"))

	// a codec token must not authenticate the legacy path, the other
	// codec's path, or another stream
	require.False(t, h264.PathMatches("app/stream"))
	require.False(t, h264.PathMatches("app/stream/hevc"))
	require.False(t, hevc.PathMatches("app/stream/h264"))
	require.False(t, legacy.PathMatches("app/stream/h264"))
	require.False(t, h264.PathMatches("app/other/h264"))
}

func TestDecrypt(t *testing.T) {
	t.Run("round trip", func(t *testing.T) {
		want := validClaims("hevc")
		got, err := Decrypt(authKey, seal(t, authKey, want), time.Now())
		require.NoError(t, err)
		require.Equal(t, want, *got)
	})

	t.Run("expired", func(t *testing.T) {
		claims := validClaims("h264")
		claims.EXP = time.Now().Add(-time.Minute).Unix()
		_, err := Decrypt(authKey, seal(t, authKey, claims), time.Now())
		require.EqualError(t, err, "publish token has expired")
	})

	t.Run("wrong key", func(t *testing.T) {
		_, err := Decrypt(authKey, seal(t, "another-key", validClaims("h264")), time.Now())
		require.Error(t, err)
	})

	t.Run("tampered", func(t *testing.T) {
		token := []byte(seal(t, authKey, validClaims("h264")))
		token[len(token)-1] ^= 0xff
		_, err := Decrypt(authKey, string(token), time.Now())
		require.Error(t, err)
	})

	t.Run("malformed", func(t *testing.T) {
		_, err := Decrypt(authKey, "not-base64!!", time.Now())
		require.Error(t, err)

		_, err = Decrypt(authKey, base64.RawURLEncoding.EncodeToString([]byte("short")), time.Now())
		require.EqualError(t, err, "publish token is malformed")
	})

	t.Run("no auth key configured", func(t *testing.T) {
		_, err := Decrypt("", seal(t, authKey, validClaims("h264")), time.Now())
		require.EqualError(t, err, "publish auth key is not configured")
	})
}
