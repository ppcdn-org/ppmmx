package recording

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/bluenviron/mediamtx/internal/test"
)

// fakeAppSecretLookup is a controllable AppSecretLookup for testing
// verifyToken against a per-app credential instead of a deployment-wide
// shared secret.
type fakeAppSecretLookup struct {
	secrets map[string]string
}

func (f *fakeAppSecretLookup) AppSecret(appID string) (string, bool) {
	secret, ok := f.secrets[appID]
	return secret, ok
}

func advanceToken(secret, nonce string, req splitRecRequest) string {
	canonical, _ := json.Marshal(struct {
		Time      string `json:"time"`
		AppID     string `json:"appId"`
		TableID   string `json:"tableId"`
		GameRound string `json:"gameRound"`
		GameID    string `json:"gameId"`
		Nonce     string `json:"nonce"`
	}{req.Time, req.AppID, req.TableID, req.GameRound, req.GameID, nonce})
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(canonical)
	return "HMAC-SHA256 " + hex.EncodeToString(mac.Sum(nil))
}

func TestSplitRecSimpleAuth(t *testing.T) {
	h := NewSplitRecHandler(nil, nil, test.NilLogger)
	h.ConfigureAuth("simple")
	h.SetAppSecretLookup(&fakeAppSecretLookup{secrets: map[string]string{
		"app1": "test-secret",
		"app2": "other-app-secret",
	}})
	req := splitRecRequest{Time: "2000000000", AppID: "app1", TableID: "table", GameRound: "gc", GameID: "game"}
	token := fmt.Sprintf("%x", md5.Sum([]byte("test-secret"+req.Time+req.AppID+req.TableID+req.GameRound+req.GameID)))
	require.True(t, h.verifyToken(token, "", req))
	require.True(t, h.verifyToken(token, "ignored-in-simple-mode", req))
	require.False(t, h.verifyToken(fmt.Sprintf("%x", md5.Sum([]byte("wrong-secret"+req.Time+req.AppID+req.TableID+req.GameRound+req.GameID))), "", req))
	require.False(t, h.verifyToken(advanceToken("secret", "0123456789abcdef", req), "0123456789abcdef", req))

	// AppID is signed, not just character-set validated: a token computed
	// for one appId must not verify against a request with a different
	// appId substituted in, even though app2 is itself a valid, known app
	// with its own (different) secret.
	tampered := req
	tampered.AppID = "app2"
	require.False(t, h.verifyToken(token, "", tampered))
}

func TestSplitRecAdvanceAuth(t *testing.T) {
	h := NewSplitRecHandler(nil, nil, test.NilLogger)
	h.ConfigureAuth("advance")
	h.SetAppSecretLookup(&fakeAppSecretLookup{secrets: map[string]string{
		"app1": "test-secret",
		"app2": "other-app-secret",
	}})
	req := splitRecRequest{
		Time:      fmt.Sprintf("%d", time.Now().Add(time.Minute).Unix()),
		AppID:     "app1",
		TableID:   "table",
		GameRound: "gc",
		GameID:    "game",
	}
	nonce := "0123456789abcdef"
	token := advanceToken("test-secret", nonce, req)
	require.True(t, h.verifyToken(token, nonce, req))

	modified := req
	modified.GameID = "other-game"
	require.False(t, h.verifyToken(token, nonce, modified))
	require.False(t, h.verifyToken(token, "different-nonce-1", req))
	require.False(t, h.verifyToken(advanceToken("wrong-secret", nonce, req), nonce, req))
	require.False(t, h.verifyToken(token, "short", req))
	require.False(t, h.verifyToken(fmt.Sprintf("%x", md5.Sum([]byte("test-secret"+req.Time+req.AppID+req.TableID+req.GameRound+req.GameID))), nonce, req))

	tampered := req
	tampered.AppID = "app2"
	require.False(t, h.verifyToken(token, nonce, tampered))
}

func TestSplitRecNoSecretConfiguredFailsClosed(t *testing.T) {
	req := splitRecRequest{Time: "2000000000", AppID: "app1", TableID: "table", GameRound: "gc", GameID: "game"}

	h := NewSplitRecHandler(nil, nil, test.NilLogger)
	// authMode defaults to "simple" with no AppSecretLookup wired in
	// (SetAppSecretLookup never called) - the unkeyed MD5 token an outside
	// caller could compute without knowing any secret must still be
	// rejected.
	unkeyed := fmt.Sprintf("%x", md5.Sum([]byte(req.Time+req.AppID+req.TableID+req.GameRound+req.GameID)))
	require.False(t, h.verifyToken(unkeyed, "", req))

	h.ConfigureAuth("advance")
	require.False(t, h.verifyToken(advanceToken("", "0123456789abcdef", req), "0123456789abcdef", req))
}

// TestSplitRecUnknownAppFailsClosed covers an appId that isn't in the
// synced whitelist at all (never valid, deleted, arrears): verifyToken
// must reject it even with an otherwise well-formed token, rather than
// falling through to an empty-secret comparison.
func TestSplitRecUnknownAppFailsClosed(t *testing.T) {
	h := NewSplitRecHandler(nil, nil, test.NilLogger)
	h.ConfigureAuth("simple")
	h.SetAppSecretLookup(&fakeAppSecretLookup{secrets: map[string]string{"app1": "test-secret"}})

	req := splitRecRequest{Time: "2000000000", AppID: "unknown-app", TableID: "table", GameRound: "gc", GameID: "game"}
	unkeyed := fmt.Sprintf("%x", md5.Sum([]byte(req.Time+req.AppID+req.TableID+req.GameRound+req.GameID)))
	require.False(t, h.verifyToken(unkeyed, "", req))
}

func TestSplitRecAdvanceNonceReplay(t *testing.T) {
	h := NewSplitRecHandler(nil, nil, test.NilLogger)
	expiry := time.Now().Add(time.Minute)
	require.True(t, h.useNonce("0123456789abcdef", expiry))
	require.False(t, h.useNonce("0123456789abcdef", expiry))
}
