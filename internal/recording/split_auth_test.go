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

func advanceToken(secret, nonce string, req splitRecRequest) string {
	canonical, _ := json.Marshal(struct {
		Time  string `json:"time"`
		Table string `json:"table"`
		GC    string `json:"gc"`
		Game  string `json:"game"`
		Nonce string `json:"nonce"`
	}{req.Time, req.Table, req.GC, req.Game, nonce})
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(canonical)
	return "HMAC-SHA256 " + hex.EncodeToString(mac.Sum(nil))
}

func TestSplitRecSimpleAuthCompatibility(t *testing.T) {
	h := NewSplitRecHandler(nil, nil, test.NilLogger)
	req := splitRecRequest{Time: "2000000000", Table: "table", GC: "gc", Game: "game"}
	legacy := fmt.Sprintf("%x", md5.Sum([]byte("lotto-dvr"+req.Time+req.Table+req.GC+req.Game)))
	require.True(t, h.verifyToken(legacy, "", req))
	require.True(t, h.verifyToken(legacy, "ignored-in-simple-mode", req))
	require.False(t, h.verifyToken(advanceToken("secret", "0123456789abcdef", req), "0123456789abcdef", req))
}

func TestSplitRecAdvanceAuth(t *testing.T) {
	h := NewSplitRecHandler(nil, nil, test.NilLogger)
	h.ConfigureAuth("advance", "test-secret")
	req := splitRecRequest{
		Time:  fmt.Sprintf("%d", time.Now().Add(time.Minute).Unix()),
		Table: "table",
		GC:    "gc",
		Game:  "game",
	}
	nonce := "0123456789abcdef"
	token := advanceToken("test-secret", nonce, req)
	require.True(t, h.verifyToken(token, nonce, req))

	modified := req
	modified.Game = "other-game"
	require.False(t, h.verifyToken(token, nonce, modified))
	require.False(t, h.verifyToken(token, "different-nonce-1", req))
	require.False(t, h.verifyToken(advanceToken("wrong-secret", nonce, req), nonce, req))
	require.False(t, h.verifyToken(token, "short", req))
	require.False(t, h.verifyToken(fmt.Sprintf("%x", md5.Sum([]byte("lotto-dvr"+req.Time+req.Table+req.GC+req.Game))), nonce, req))
}

func TestSplitRecAdvanceNonceReplay(t *testing.T) {
	h := NewSplitRecHandler(nil, nil, test.NilLogger)
	expiry := time.Now().Add(time.Minute)
	require.True(t, h.useNonce("0123456789abcdef", expiry))
	require.False(t, h.useNonce("0123456789abcdef", expiry))
}
