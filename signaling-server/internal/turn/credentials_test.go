package turn

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"testing"
	"time"
)

func TestGenerateCredentials(t *testing.T) {
	secret := "test-secret"
	accountID := "abc123"
	expiry := time.Unix(1700000000, 0)

	creds := GenerateCredentials(secret, accountID, expiry)

	// Username should be timestamp:accountID
	expectedUsername := "1700000000:abc123"
	if creds.Username != expectedUsername {
		t.Errorf("username = %q, want %q", creds.Username, expectedUsername)
	}

	// Credential should be HMAC-SHA1 of username
	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(expectedUsername))
	expectedCred := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if creds.Credential != expectedCred {
		t.Errorf("credential = %q, want %q", creds.Credential, expectedCred)
	}
}

func TestGenerateCredentials_DifferentSecrets(t *testing.T) {
	creds1 := GenerateCredentials("secret1", "account_abc", time.Now())
	creds2 := GenerateCredentials("secret2", "account_abc", time.Now())

	if creds1.Credential == creds2.Credential {
		t.Error("different secrets should produce different credentials")
	}
}
