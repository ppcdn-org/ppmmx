package forward

import "strings"

// mmxPathPlaceholder is substituted in MmxConfig.URL with the forwarding
// path's own name (e.g. "live/table-view"), so a single forwardMmxURL
// template configured on a regex/"all" path (which matches many different
// stream names) still forwards each one under its own name on the target
// node, instead of every match colliding on one fixed target path. Static
// endpoints that don't need this simply omit the placeholder.
const mmxPathPlaceholder = "{path}"

// MmxConfig holds the settings needed to forward a path to another mmx
// node's own WHIP publish endpoint, mmx-to-mmx (as opposed to TencentConfig,
// which forwards to Tencent Cloud's WHIP ingest). Unlike Tencent, a target
// mmx node natively accepts a multi-layer Simulcast WHIP offer (it's the
// same server role mmx uses for OBS ingest), so there's no need to split the
// stream into one WHIP session per quality layer - see buildMmxTarget vs.
// SplitLayers/buildTencentWHIPURL.
type MmxConfig struct {
	Enable bool
	// URL is the target node's WHIP publish endpoint. Either a fixed
	// address (e.g. "http://mmx2-host:8889/live/table-view/whip") or a
	// template containing the "{path}" placeholder (e.g.
	// "http://mmx2-host:8889/{path}/whip"), substituted with this path's
	// own name at (re)connect time - see mmxPathPlaceholder. Per-path (see
	// conf.Path.ForwardMmxURL); there is no per-node/global default since
	// the whole point is to name a specific remote path.
	URL string
	// AuthToken is sent as "Authorization: Bearer <AuthToken>" - the same
	// header mmx's own WHIP publish endpoint checks via checkWHIPDeviceID
	// (see internal/servers/webrtc/http_server.go), so this must be set to
	// the *target* node's WHIP_WS_SECRET, not the local node's.
	AuthToken string
}

// buildMmxTarget resolves the WHIP URL and Bearer token to forward to
// another mmx node, substituting mmxPathPlaceholder in cfg.URL (if present)
// with pathName. Unlike buildTencentWHIPURL, no per-request signing is
// needed - the token is static per-path configuration - but the same (url,
// bearerToken) shape lets forwarderInstance.runInner treat a Tencent target
// and a mmx target identically.
func buildMmxTarget(cfg MmxConfig, pathName string) (whipURL string, bearerToken string) {
	whipURL = strings.ReplaceAll(strings.TrimSpace(cfg.URL), mmxPathPlaceholder, pathName)
	return whipURL, cfg.AuthToken
}
