package forward

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTencentSign(t *testing.T) {
	// now + 30 days must land exactly on unix 1735689600, whose values below
	// were computed independently (not by re-running tencentSign's own
	// formula) via: hex(1735689600) and md5("testsecret"+"mystream"+that hex.
	now := time.Unix(1733097600, 0).UTC()

	txTime, txSecret := tencentSign("testsecret", "mystream", 30, now)

	require.Equal(t, "67748580", txTime)
	require.Equal(t, "4bd9bab0818cc1e0ca29c0eaf77a9395", txSecret)
}

func TestBuildTencentWHIPURL(t *testing.T) {
	cfg := TencentConfig{
		Endpoint:  DefaultTencentEndpoint,
		Domain:    "publish.example.com",
		App:       "live",
		SecretKey: "testsecret",
		TokenDays: 30,
	}

	got := buildTencentWHIPURL(cfg, "mystream")

	require.Contains(t, got, "webrtc://publish.example.com/live/mystream")
	require.Contains(t, got, "txSecret=")
	require.Contains(t, got, "txTime=")
	require.NotContains(t, got, "{", "no placeholder should survive substitution")
}
