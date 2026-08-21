package forward

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildMmxTarget(t *testing.T) {
	cfg := MmxConfig{
		Enable:    true,
		URL:       "http://mmx2-host:8889/live/table-view/whip",
		AuthToken: "target-node-whip-secret",
	}

	whipURL, bearerToken := buildMmxTarget(cfg, "live/table-view")

	require.Equal(t, "http://mmx2-host:8889/live/table-view/whip", whipURL)
	require.Equal(t, "target-node-whip-secret", bearerToken)
}

func TestBuildMmxTargetTrimsWhitespace(t *testing.T) {
	cfg := MmxConfig{
		URL:       "  http://mmx2-host:8889/live/table-view/whip  ",
		AuthToken: "secret",
	}

	whipURL, _ := buildMmxTarget(cfg, "live/table-view")

	require.Equal(t, "http://mmx2-host:8889/live/table-view/whip", whipURL)
}

func TestBuildMmxTargetSubstitutesPathPlaceholder(t *testing.T) {
	cfg := MmxConfig{
		Enable:    true,
		URL:       "http://127.0.0.1:8890/{path}/whip",
		AuthToken: "target-node-whip-secret",
	}

	whipURL, _ := buildMmxTarget(cfg, "live/table-view")

	require.Equal(t, "http://127.0.0.1:8890/live/table-view/whip", whipURL)
}

func TestBuildMmxTargetWithoutPlaceholderIgnoresPathName(t *testing.T) {
	cfg := MmxConfig{
		Enable:    true,
		URL:       "http://mmx2-host:8889/live/fixed-target/whip",
		AuthToken: "target-node-whip-secret",
	}

	whipURL, _ := buildMmxTarget(cfg, "live/table-view")

	require.Equal(t, "http://mmx2-host:8889/live/fixed-target/whip", whipURL)
}

func TestForwarderTargetSelectsMmxOverTencent(t *testing.T) {
	f := &Forwarder{
		Tencent: TencentConfig{
			Endpoint:  DefaultTencentEndpoint,
			Domain:    "publish.example.com",
			App:       "live",
			SecretKey: "testsecret",
			TokenDays: 30,
		},
		StreamKey: "mystream",
		Mmx: MmxConfig{
			Enable:    true,
			URL:       "http://mmx2-host:8889/live/table-view/whip",
			AuthToken: "target-node-whip-secret",
		},
	}

	target := f.target()

	require.Equal(t, "http://mmx2-host:8889/live/table-view/whip", target.whipURL)
	require.Equal(t, "target-node-whip-secret", target.bearerToken)
	require.Contains(t, target.logLabel, "mmx:")
}

func TestForwarderTargetSubstitutesPathPlaceholder(t *testing.T) {
	f := &Forwarder{
		Mmx: MmxConfig{
			Enable:    true,
			URL:       "http://127.0.0.1:8890/{path}/whip",
			AuthToken: "target-node-whip-secret",
		},
		PathName: "live/table-view",
	}

	target := f.target()

	require.Equal(t, "http://127.0.0.1:8890/live/table-view/whip", target.whipURL)
}

func TestForwarderTargetFallsBackToTencent(t *testing.T) {
	f := &Forwarder{
		Tencent: TencentConfig{
			Endpoint:  DefaultTencentEndpoint,
			Domain:    "publish.example.com",
			App:       "live",
			SecretKey: "testsecret",
			TokenDays: 30,
		},
		StreamKey: "mystream",
	}

	target := f.target()

	require.Equal(t, tencentWHIPEndpoint, target.whipURL)
	require.Contains(t, target.bearerToken, "webrtc://publish.example.com/live/mystream")
	require.Contains(t, target.logLabel, "tencent:mystream")
}
