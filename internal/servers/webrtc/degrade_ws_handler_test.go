package webrtc

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

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
	ctx := newDegradeAuthTestContext(t, "/live/table1-fwv/ws/whip?token=test-secret", "")
	require.True(t, verifyDegradeAuth("test-secret", ctx))
}

func TestVerifyDegradeAuthAuthorizationHeader(t *testing.T) {
	ctx := newDegradeAuthTestContext(t, "/live/table1-fwv/ws/whip", "Bearer test-secret")
	require.True(t, verifyDegradeAuth("test-secret", ctx))
}

func TestVerifyDegradeAuthQueryParamTakesPriorityOverHeader(t *testing.T) {
	// Both set, and they disagree - query param wins (checked first); this
	// isn't a security-relevant choice (both come from the same client),
	// just documents which one takes effect if a caller sets both.
	ctx := newDegradeAuthTestContext(t, "/live/table1-fwv/ws/whip?token=test-secret", "Bearer wrong-secret")
	require.True(t, verifyDegradeAuth("test-secret", ctx))
}

func TestVerifyDegradeAuthRejectsWrongSecret(t *testing.T) {
	ctx := newDegradeAuthTestContext(t, "/live/table1-fwv/ws/whip?token=wrong-secret", "")
	require.False(t, verifyDegradeAuth("test-secret", ctx))
}

func TestVerifyDegradeAuthRejectsMissingCredentials(t *testing.T) {
	ctx := newDegradeAuthTestContext(t, "/live/table1-fwv/ws/whip", "")
	require.False(t, verifyDegradeAuth("test-secret", ctx))
}

func TestVerifyDegradeAuthRejectsMalformedAuthorizationHeader(t *testing.T) {
	ctx := newDegradeAuthTestContext(t, "/live/table1-fwv/ws/whip", "test-secret") // missing "Bearer " prefix
	require.False(t, verifyDegradeAuth("test-secret", ctx))
}

func TestVerifyDegradeAuthRejectsWhenServerSecretNotConfigured(t *testing.T) {
	// WHIP_WS_SECRET unset server-side - must never accept an
	// empty-string match against an equally-empty client token.
	ctx := newDegradeAuthTestContext(t, "/live/table1-fwv/ws/whip?token=", "")
	require.False(t, verifyDegradeAuth("", ctx))
}
