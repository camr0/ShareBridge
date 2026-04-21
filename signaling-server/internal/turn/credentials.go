package turn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"time"
)

// Credentials holds TURN credentials (kept for backward compatibility with BuildICEConfig).
// TURN is no longer supported; only STUN is used for ICE configuration.
type Credentials struct {
	Username   string `json:"username"`
	Credential string `json:"credential"`
}

// GenerateCredentials creates HMAC-based TURN credentials.
// DEPRECATED: TURN is no longer supported. This function is kept for API compatibility.
func GenerateCredentials(secret, accountID string, expiry time.Time) Credentials {
	username := fmt.Sprintf("%d:%s", expiry.Unix(), accountID)

	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	credential := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	return Credentials{
		Username:   username,
		Credential: credential,
	}
}
