package recording

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

// simpleToken mirrors §5.3's simple-mode recipe, matching
// TestSplitRecSimpleAuth's inline construction.
func simpleToken(secret string, req splitRecRequest) string {
	base := secret + req.Time + req.TableID + req.GameRound + req.GameID
	return fmt.Sprintf("%x", md5.Sum([]byte(base)))
}

// serveSplitRec drives h.ServeHTTP end to end with a real JSON body and
// headers, the way the actual HTTP server would, and returns the recorded
// response. Every case here uses a round-end request (GameRound set) for a
// table with no active game, so a request that gets past auth/freshness
// always lands on stopRound's no-active-round no-op (200 "OK") rather than
// depending on startRound's path-resolution logic (covered separately in
// split_ingest_test.go) - this keeps these cases isolated to exactly what
// they're testing: the freshness/nonce gate in ServeHTTP, not what happens
// after it.
func serveSplitRec(h *SplitRecHandler, req splitRecRequest, headers map[string]string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(req)
	httpReq := httptest.NewRequest(http.MethodPost, "/api/split-rec", bytes.NewReader(body))
	for k, v := range headers {
		httpReq.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httpReq
	h.ServeHTTP(ctx)
	return rec
}

func decodeSplitRecResponse(t *testing.T, rec *httptest.ResponseRecorder) splitRecResponse {
	t.Helper()
	var resp splitRecResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body=%s", rec.Body.String())
	return resp
}

func freshnessReq(signingInstant time.Time) splitRecRequest {
	return splitRecRequest{
		Time:      fmt.Sprintf("%d", signingInstant.Unix()),
		TableID:   "table",
		GameRound: "gc",
		GameID:    "game",
	}
}

func TestSplitRecFreshness_SimpleModeAcceptsWithinToleranceEitherDirection(t *testing.T) {
	h := NewSplitRecHandler(nil, nil, test.NilLogger)
	h.ConfigureAuth("simple", "test-secret")

	for _, drift := range []time.Duration{0, -simpleModeTimeTolerance + time.Second, simpleModeTimeTolerance - time.Second} {
		req := freshnessReq(time.Now().Add(drift))
		rec := serveSplitRec(h, req, map[string]string{"Authorization": simpleToken("test-secret", req)})
		resp := decodeSplitRecResponse(t, rec)
		require.Equal(t, http.StatusOK, rec.Code, "drift=%s body=%s", drift, rec.Body.String())
		require.Equal(t, "OK", resp.Msg)
	}
}

func TestSplitRecFreshness_SimpleModeRejectsOutsideToleranceEitherDirection(t *testing.T) {
	h := NewSplitRecHandler(nil, nil, test.NilLogger)
	h.ConfigureAuth("simple", "test-secret")

	for _, drift := range []time.Duration{-simpleModeTimeTolerance - time.Second, simpleModeTimeTolerance + time.Second} {
		req := freshnessReq(time.Now().Add(drift))
		rec := serveSplitRec(h, req, map[string]string{"Authorization": simpleToken("test-secret", req)})
		resp := decodeSplitRecResponse(t, rec)
		require.Equal(t, http.StatusUnauthorized, rec.Code, "drift=%s", drift)
		require.Equal(t, "token expired!", resp.Msg)
	}
}

// TestSplitRecFreshness_DocExampleNowSucceeds pins doc §5.6's own worked
// example (`T=$(date +%s)`, i.e. drift=0) as passing - this is the exact
// case that used to be rejected unconditionally before this fix (see
// docs/test/ppcdn-external-api-test-report.md finding 2).
func TestSplitRecFreshness_DocExampleNowSucceeds(t *testing.T) {
	h := NewSplitRecHandler(nil, nil, test.NilLogger)
	h.ConfigureAuth("simple", "test-secret")

	req := freshnessReq(time.Now())
	rec := serveSplitRec(h, req, map[string]string{"Authorization": simpleToken("test-secret", req)})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

func TestSplitRecFreshness_AdvanceModeAcceptsWithinToleranceEitherDirection(t *testing.T) {
	h := NewSplitRecHandler(nil, nil, test.NilLogger)
	h.ConfigureAuth("advance", "test-secret")

	cases := []struct {
		name  string
		drift time.Duration
	}{
		{"near future", advanceModeTimeTolerance - time.Minute},
		{"near past", -(advanceModeTimeTolerance - time.Minute)},
	}
	for _, c := range cases {
		req := freshnessReq(time.Now().Add(c.drift))
		nonce := "nonce-0123456789-" + c.name // >=16 chars: verifyToken rejects shorter nonces in advance mode
		rec := serveSplitRec(h, req, map[string]string{
			"Authorization":     advanceToken("test-secret", nonce, req),
			"X-Split-Rec-Nonce": nonce,
		})
		resp := decodeSplitRecResponse(t, rec)
		require.Equal(t, http.StatusOK, rec.Code, "case=%s body=%s", c.name, rec.Body.String())
		require.Equal(t, "OK", resp.Msg)
	}
}

func TestSplitRecFreshness_AdvanceModeRejectsBeyondFiveMinutes(t *testing.T) {
	h := NewSplitRecHandler(nil, nil, test.NilLogger)
	h.ConfigureAuth("advance", "test-secret")

	for _, drift := range []time.Duration{advanceModeTimeTolerance + time.Minute, -(advanceModeTimeTolerance + time.Minute)} {
		req := freshnessReq(time.Now().Add(drift))
		nonce := "nonce-out-of-bounds-0123456789" // >=16 chars: verifyToken rejects shorter nonces in advance mode
		rec := serveSplitRec(h, req, map[string]string{
			"Authorization":     advanceToken("test-secret", nonce, req),
			"X-Split-Rec-Nonce": nonce,
		})
		resp := decodeSplitRecResponse(t, rec)
		require.Equal(t, http.StatusUnauthorized, rec.Code, "drift=%s", drift)
		require.Equal(t, "token expired!", resp.Msg)
	}
}

func TestSplitRecFreshness_AdvanceModeRejectsReusedNonce(t *testing.T) {
	h := NewSplitRecHandler(nil, nil, test.NilLogger)
	h.ConfigureAuth("advance", "test-secret")

	req := freshnessReq(time.Now())
	nonce := "nonce-replay-0123456789" // >=16 chars: verifyToken rejects shorter nonces in advance mode
	headers := map[string]string{
		"Authorization":     advanceToken("test-secret", nonce, req),
		"X-Split-Rec-Nonce": nonce,
	}

	first := serveSplitRec(h, req, headers)
	require.Equal(t, http.StatusOK, first.Code, "body=%s", first.Body.String())

	second := serveSplitRec(h, req, headers)
	secondResp := decodeSplitRecResponse(t, second)
	require.Equal(t, http.StatusUnauthorized, second.Code)
	require.Equal(t, "nonce already used!", secondResp.Msg)
}
