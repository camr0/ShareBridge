package turn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"time"
)

type Credentials struct {
	Username   string `json:"username"`
	Credential string `json:"credential"`
}

// GenerateCredentials creates HMAC-based TURN credentials.
// The username format is {timestamp}:{accountID} and the credential is HMAC-SHA1 of the username.
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
