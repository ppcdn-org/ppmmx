// Package forward relays mmx's live streams to third-party ingest endpoints.
package forward

import (
	"crypto/md5"
	"fmt"
	"strings"
	"time"
)

// DefaultTencentEndpoint is the default WHIP push URL template. The exact
// query-param shape Tencent expects for WHIP has not been confirmed against
// a live account yet (see doc/forward-tencent-design.md §4.1) — this is why
// the endpoint is a template read from config rather than a hardcoded
// format: if it turns out wrong, it can be fixed in the YAML, no code change
// or rebuild needed.
const DefaultTencentEndpoint = "webrtc://{domain}/{app}/{streamKey}" +
	"?txSecret={txSecret}&txTime={txTime}"

// TencentConfig holds the Tencent Cloud account-level (global) settings.
type TencentConfig struct {
	Enable    bool
	Endpoint  string // URL template with {domain}/{app}/{streamKey}/{txSecret}/{txTime} placeholders
	Domain    string
	App       string
	SecretKey string
	TokenDays int
}

// tencentSign computes Tencent Cloud's anti-leech signature. The formula is
// shared across its RTMP/SRT/WebRTC ingest protocols — it's a domain-level
// mechanism, not protocol-specific — and is ported verbatim from
// refer/multipusher/utils/tencent.go (GenTencentTxSecretWithDays):
//
//	txTime   = hex(unix(now + tokenDays*24h))
//	txSecret = md5(secretKey + streamKey + txTime)
func tencentSign(secretKey, streamKey string, tokenDays int, now time.Time) (txTime, txSecret string) {
	expiry := now.Add(time.Duration(tokenDays) * 24 * time.Hour).Unix()
	txTime = fmt.Sprintf("%X", expiry)
	sum := md5.Sum([]byte(secretKey + streamKey + txTime))
	txSecret = fmt.Sprintf("%x", sum)
	return txTime, txSecret
}

// buildTencentWHIPURL fills cfg.Endpoint's placeholders with the given
// stream key and a freshly computed signature. Called once per (re)connect
// attempt so the signature's expiry window always starts from "now".
func buildTencentWHIPURL(cfg TencentConfig, streamKey string) string {
	txTime, txSecret := tencentSign(cfg.SecretKey, streamKey, cfg.TokenDays, time.Now())

	r := strings.NewReplacer(
		"{domain}", cfg.Domain,
		"{app}", cfg.App,
		"{streamKey}", streamKey,
		"{txSecret}", txSecret,
		"{txTime}", txTime,
	)
	return r.Replace(cfg.Endpoint)
}
