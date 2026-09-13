// Package publishtoken decrypts and validates the publish bearer tokens
// ppcenter issues. Both ingest protocols use the same token: WHIP carries
// it in the Authorization header, SRT in the streamID's third segment (see
// docs/design/whip-hevc-h264-multitrack-simulcast-design.zh-CN.md §3.2).
// They share one format, one key and one codec-binding rule so that the two
// paths can't drift into separate auth semantics.
package publishtoken

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// Claims is the JSON payload sealed inside a publish bearer token by
// ppcenter (see vlb/internal/utils.EncryptWHIPToken in the ppcenter repo).
// Field names are part of the wire format shared between the two
// independent implementations and must not change on one side without the
// other.
//
// Codec is empty for a token bound to the legacy 2-segment path
// (appId/stream); when non-empty ("h264"/"hevc") the token is only valid
// for the corresponding 3-segment codec path (appId/stream/codec) - see
// PathMatches.
type Claims struct {
	UUID   string `json:"uuid"`
	AppID  string `json:"appId"`
	Stream string `json:"stream"`
	Codec  string `json:"codec,omitempty"`
	IAT    int64  `json:"iat"`
	EXP    int64  `json:"exp"`
}

// ExpectedPath is the one path this token authenticates: appId/stream for a
// legacy token, appId/stream/codec for a codec-bound one.
func (c *Claims) ExpectedPath() string {
	p := c.AppID + "/" + c.Stream
	if c.Codec != "" {
		p += "/" + c.Codec
	}
	return p
}

// PathMatches reports whether this token authenticates pathName. A
// codec-bound token is only valid for its own 3-segment codec path - it
// must not also authenticate the legacy 2-segment path or a different
// codec's path, and vice versa.
func (c *Claims) PathMatches(pathName string) bool {
	return c.ExpectedPath() == pathName
}

// Decrypt reverses ppcenter's EncryptWHIPToken: AES-256-GCM with
// key=SHA-256(authKey), wire-encoded as base64url(nonce || sealed). GCM's
// authentication tag means any tampering makes decryption fail outright, so
// a successful decrypt is sufficient proof the token was issued by a holder
// of authKey - no separate signature check is needed. Also rejects an
// expired token.
func Decrypt(authKey, token string, now time.Time) (*Claims, error) {
	if authKey == "" {
		return nil, errors.New("publish auth key is not configured")
	}
	sealed, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, err
	}

	key := sha256.Sum256([]byte(authKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonceSize := gcm.NonceSize()
	if len(sealed) < nonceSize {
		return nil, errors.New("publish token is malformed")
	}
	nonce, ciphertext := sealed[:nonceSize], sealed[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}

	var claims Claims
	if err := json.Unmarshal(plaintext, &claims); err != nil {
		return nil, err
	}
	if now.Unix() > claims.EXP {
		return nil, errors.New("publish token has expired")
	}
	return &claims, nil
}
