package webrtc

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/protocols/publishtoken"
)

const degradeTestAuthKey = "test-auth-key"

// sealDegradeTestToken mints a ppcenter-style publish bearer token so the
// auth tests don't need ppcenter running. Mirrors publishtoken.Decrypt's
// wire format: base64url(nonce || AES-256-GCM(key=SHA-256(authKey))(claims)).
func sealDegradeTestToken(t *testing.T, authKey string, claims publishtoken.Claims) string {
	t.Helper()
	plain, err := json.Marshal(claims)
	require.NoError(t, err)
	key := sha256.Sum256([]byte(authKey))
	block, err := aes.NewCipher(key[:])
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	nonce := []byte("0123456789ab")
	sealed := gcm.Seal(nil, nonce, plain, nil)
	return base64.RawURLEncoding.EncodeToString(append(nonce, sealed...))
}

func validDegradeClaims() publishtoken.Claims {
	return publishtoken.Claims{
		UUID:   "ppobs-test",
		AppID:  "live",
		Stream: "table1-fwv",
		Codec:  "h264",
		IAT:    time.Now().Add(-time.Minute).Unix(),
		EXP:    time.Now().Add(time.Hour).Unix(),
	}
}

func newDegradeAuthTestContext(t *testing.T, target string, authHeader string) *gin.Context {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = req
	return ctx
}

func TestVerifyDegradeAuthQueryParam(t *testing.T) {
	token := sealDegradeTestToken(t, degradeTestAuthKey, validDegradeClaims())
	ctx := newDegradeAuthTestContext(t, "/live/table1-fwv/h264/ws/whip?token="+token, "")
	require.True(t, verifyDegradeAuth(degradeTestAuthKey, "live/table1-fwv/h264", ctx))
}

func TestVerifyDegradeAuthAuthorizationHeader(t *testing.T) {
	token := sealDegradeTestToken(t, degradeTestAuthKey, validDegradeClaims())
	ctx := newDegradeAuthTestContext(t, "/live/table1-fwv/h264/ws/whip", "Bearer "+token)
	require.True(t, verifyDegradeAuth(degradeTestAuthKey, "live/table1-fwv/h264", ctx))
}

func TestVerifyDegradeAuthQueryParamTakesPriorityOverHeader(t *testing.T) {
	// Both set, and they disagree - query param wins (checked first); this
	// isn't a security-relevant choice (both come from the same client),
	// just documents which one takes effect if a caller sets both.
	token := sealDegradeTestToken(t, degradeTestAuthKey, validDegradeClaims())
	ctx := newDegradeAuthTestContext(t, "/live/table1-fwv/h264/ws/whip?token="+token, "Bearer not-a-token")
	require.True(t, verifyDegradeAuth(degradeTestAuthKey, "live/table1-fwv/h264", ctx))
}

func TestVerifyDegradeAuthRejectsTokenForOtherPath(t *testing.T) {
	// A token minted for a different appId/stream/codec must not authenticate
	// this path.
	claims := validDegradeClaims()
	claims.Codec = "hevc"
	token := sealDegradeTestToken(t, degradeTestAuthKey, claims)
	ctx := newDegradeAuthTestContext(t, "/live/table1-fwv/h264/ws/whip?token="+token, "")
	require.False(t, verifyDegradeAuth(degradeTestAuthKey, "live/table1-fwv/h264", ctx))
}

func TestVerifyDegradeAuthRejectsExpiredToken(t *testing.T) {
	claims := validDegradeClaims()
	claims.EXP = time.Now().Add(-time.Minute).Unix()
	token := sealDegradeTestToken(t, degradeTestAuthKey, claims)
	ctx := newDegradeAuthTestContext(t, "/live/table1-fwv/h264/ws/whip?token="+token, "")
	require.False(t, verifyDegradeAuth(degradeTestAuthKey, "live/table1-fwv/h264", ctx))
}

func TestVerifyDegradeAuthRejectsWrongKey(t *testing.T) {
	token := sealDegradeTestToken(t, "some-other-key", validDegradeClaims())
	ctx := newDegradeAuthTestContext(t, "/live/table1-fwv/h264/ws/whip?token="+token, "")
	require.False(t, verifyDegradeAuth(degradeTestAuthKey, "live/table1-fwv/h264", ctx))
}

func TestVerifyDegradeAuthRejectsMissingCredentials(t *testing.T) {
	ctx := newDegradeAuthTestContext(t, "/live/table1-fwv/h264/ws/whip", "")
	require.False(t, verifyDegradeAuth(degradeTestAuthKey, "live/table1-fwv/h264", ctx))
}

func TestVerifyDegradeAuthRejectsMalformedAuthorizationHeader(t *testing.T) {
	ctx := newDegradeAuthTestContext(t, "/live/table1-fwv/h264/ws/whip", "test-secret") // missing "Bearer " prefix
	require.False(t, verifyDegradeAuth(degradeTestAuthKey, "live/table1-fwv/h264", ctx))
}

func TestVerifyDegradeAuthRejectsWhenServerKeyNotConfigured(t *testing.T) {
	// WHIP_AUTH_KEY unset server-side - must never accept an empty-string
	// key against an equally-empty client token.
	token := sealDegradeTestToken(t, "", validDegradeClaims())
	ctx := newDegradeAuthTestContext(t, "/live/table1-fwv/h264/ws/whip?token="+token, "")
	require.False(t, verifyDegradeAuth("", "live/table1-fwv/h264", ctx))
}
