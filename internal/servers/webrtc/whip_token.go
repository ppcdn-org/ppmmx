package webrtc

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

// whipTokenClaims is the JSON payload sealed inside a WHIP publish bearer
// token by ppcenter (see vlb/internal/utils.EncryptWHIPToken in the
// ppcenter repo). Field names are part of the wire format shared between
// the two independent implementations and must not change on one side
// without the other.
type whipTokenClaims struct {
	UUID   string `json:"uuid"`
	AppID  string `json:"appId"`
	Stream string `json:"stream"`
	IAT    int64  `json:"iat"`
	EXP    int64  `json:"exp"`
}

// decryptWHIPToken reverses ppcenter's EncryptWHIPToken: AES-256-GCM with
// key=SHA-256(authKey), wire-encoded as base64url(nonce || sealed). GCM's
// authentication tag means any tampering makes decryption fail outright, so
// a successful decrypt is sufficient proof the token was issued by a holder
// of authKey - no separate signature check is needed. Also rejects an
// expired token.
func decryptWHIPToken(authKey, token string, now time.Time) (*whipTokenClaims, error) {
	if authKey == "" {
		return nil, errors.New("whip auth key is not configured")
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
		return nil, errors.New("whip token is malformed")
	}
	nonce, ciphertext := sealed[:nonceSize], sealed[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, err
	}

	var claims whipTokenClaims
	if err := json.Unmarshal(plaintext, &claims); err != nil {
		return nil, err
	}
	if now.Unix() > claims.EXP {
		return nil, errors.New("whip token has expired")
	}
	return &claims, nil
}
