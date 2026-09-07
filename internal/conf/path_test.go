package conf

import (
	"testing"

	"github.com/bluenviron/gortsplib/v5"
	"github.com/stretchr/testify/require"
)

func TestPathClone(t *testing.T) {
	original := &Path{
		Name:                "example",
		RTSPTransport:       RTSPTransport{new(gortsplib.ProtocolUDP)},
		SourceAnyPortEnable: new(true),
		RecordPath:          "/var/recordings",
	}

	clone := original.Clone()
	require.Equal(t, original, clone)
}

func TestPathWHEPVideoTrackCount(t *testing.T) {
	for _, count := range []int{0, 1, 4} {
		path := &Path{}
		path.setDefaults()
		path.Source = "whep://localhost/stream"
		path.WHEPVideoTrackCount = count
		require.NoError(t, path.validate(&Conf{}, "test", false, nil))
	}

	for _, count := range []int{-1, 5} {
		path := &Path{}
		path.setDefaults()
		path.Source = "whep://localhost/stream"
		path.WHEPVideoTrackCount = count
		require.EqualError(t, path.validate(&Conf{}, "test", false, nil),
			"'whepVideoTrackCount' must be between 0 and 4")
	}
}

func TestIsValidPathName(t *testing.T) {
	for _, ca := range []struct {
		name   string
		path   string
		errMsg string
	}{
		{
			name: "valid nested path",
			path: "group/cam1",
		},
		{
			name: "valid dots inside segment",
			path: "cam.v1/main",
		},
		{
			name:   "parent directory",
			path:   "../cam1",
			errMsg: "can't contain dot path segments",
		},
		{
			name:   "embedded parent directory",
			path:   "group/../cam1",
			errMsg: "can't contain dot path segments",
		},
		{
			name:   "current directory",
			path:   "./cam1",
			errMsg: "can't contain dot path segments",
		},
	} {
		t.Run(ca.name, func(t *testing.T) {
			err := IsValidPathName(ca.path)
			if ca.errMsg != "" {
				require.EqualError(t, err, ca.errMsg)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// A forwardMmx target normally carries no token in the config file: every
// target is a node the same operator runs and they share one pre-shared
// secret, so validate fills each empty token from MMX_FORWARD_SECRET
// (Conf.WebRTCForwardSecret) and the secret stays in .env only.
func TestPathForwardMmxTokenFromSharedSecret(t *testing.T) {
	newPath := func(targets []ForwardMmxTarget, singleURL, singleToken string) *Path {
		path := &Path{}
		path.setDefaults()
		path.ForwardMmx = true
		path.ForwardMmxURL = singleURL
		path.ForwardMmxToken = singleToken
		path.ForwardMmxTargets = targets
		return path
	}
	conf := &Conf{ForwardMmxEnable: true, WebRTCForwardSecret: "shared-secret"}

	path := newPath([]ForwardMmxTarget{
		{URL: "http://node-a:8889/{path}/whip"},
		{URL: "http://node-b:8889/{path}/whip", Token: "per-target-token"},
	}, "", "")
	require.NoError(t, path.validate(conf, "test", false, nil))
	require.Equal(t, "shared-secret", path.ForwardMmxTargets[0].Token)
	require.Equal(t, "per-target-token", path.ForwardMmxTargets[1].Token,
		"an explicitly configured token must survive the shared-secret fill")

	// The legacy single-target fields resolve the same way.
	path = newPath(nil, "http://node-a:8889/{path}/whip", "")
	require.NoError(t, path.validate(conf, "test", false, nil))
	require.Equal(t, "shared-secret", path.ForwardMmxToken)

	// With no secret configured anywhere, the error names the env var to
	// set rather than a 'token' field that is no longer meant to appear in
	// the config file at all.
	path = newPath([]ForwardMmxTarget{{URL: "http://node-a:8889/{path}/whip"}}, "", "")
	require.EqualError(t, path.validate(&Conf{ForwardMmxEnable: true}, "test", false, nil),
		"path 'test': forwardMmx target 0: no token: set MMX_FORWARD_SECRET in .env")
}
