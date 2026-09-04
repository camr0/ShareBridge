package frpplugin

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	credentialPrefix   = "sbrelay1"
	credentialIssuer   = "sharebridge-control"
	credentialAudience = "sharebridge-relay"
	credentialLifetime = 10 * time.Minute

	maxCredentialBytes = 4096
	maxIdentifierBytes = 128
)

// CredentialClaims mirrors the exact control-issued §7.2 signed claim set.
// This package deliberately does not import the control module: the relay is a
// separately deployed trust boundary and receives only the Ed25519 public key.
type CredentialClaims struct {
	Issuer        string    `json:"iss"`
	Audience      string    `json:"aud"`
	APIKeyID      string    `json:"api_key_id"`
	AgentRecordID string    `json:"agent_record_id"`
	Namespace     string    `json:"namespace"`
	ProxyName     string    `json:"proxy_name"`
	RelayPort     int       `json:"relay_port"`
	Generation    int       `json:"generation"`
	IssuedAt      time.Time `json:"issued_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	JTI           string    `json:"jti"`
}

type verifiedCredential struct {
	claims    CredentialClaims
	tokenHash [sha256.Size]byte
}

func verifyCredential(publicKey ed25519.PublicKey, token string, now time.Time) (verifiedCredential, error) {
	var verified verifiedCredential
	if len(publicKey) != ed25519.PublicKeySize {
		return verified, errors.New("invalid control public key")
	}
	if token == "" || len(token) > maxCredentialBytes {
		return verified, errors.New("invalid credential size")
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != credentialPrefix {
		return verified, errors.New("malformed credential envelope")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) == 0 || len(payload) > maxCredentialBytes {
		return verified, errors.New("malformed credential payload")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != ed25519.SignatureSize {
		return verified, errors.New("malformed credential signature")
	}
	if !ed25519.Verify(publicKey, payload, signature) {
		return verified, errors.New("invalid credential signature")
	}

	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&verified.claims); err != nil {
		return verifiedCredential{}, fmt.Errorf("invalid credential claims: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return verifiedCredential{}, errors.New("invalid trailing credential data")
	}
	if err := validateCredentialClaims(verified.claims, now); err != nil {
		return verifiedCredential{}, err
	}
	verified.tokenHash = sha256.Sum256([]byte(token))
	return verified, nil
}

func validateCredentialClaims(claims CredentialClaims, now time.Time) error {
	if claims.Issuer != credentialIssuer || claims.Audience != credentialAudience {
		return errors.New("invalid credential authority")
	}
	if !boundedIdentifier(claims.APIKeyID) || !boundedIdentifier(claims.AgentRecordID) || !boundedIdentifier(claims.JTI) {
		return errors.New("invalid credential identity")
	}
	if !validNamespace(claims.Namespace) || claims.ProxyName != "sb-"+claims.Namespace || len(claims.ProxyName) > maxIdentifierBytes {
		return errors.New("invalid credential proxy identity")
	}
	if claims.RelayPort <= 0 || claims.RelayPort > 65535 || claims.Generation < 0 {
		return errors.New("invalid credential assignment")
	}
	if claims.IssuedAt.IsZero() || claims.ExpiresAt.IsZero() || claims.IssuedAt.After(now) || !claims.ExpiresAt.After(now) {
		return errors.New("credential outside admission window")
	}
	lifetime := claims.ExpiresAt.Sub(claims.IssuedAt)
	if lifetime <= 0 || lifetime > credentialLifetime {
		return errors.New("invalid credential lifetime")
	}
	return nil
}

func boundedIdentifier(value string) bool {
	return value != "" && len(value) <= maxIdentifierBytes
}

func validNamespace(namespace string) bool {
	if len(namespace) != 10 || !strings.HasPrefix(namespace, "sb") {
		return false
	}
	for _, character := range namespace[2:] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
