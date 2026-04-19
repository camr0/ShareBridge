package relay

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const TokenLifetime = 120 * time.Second

type BrowserPolicyClaims struct {
	SID               string `json:"sid"`
	SessionCode       string `json:"session_code"`
	RelayAllowed      bool   `json:"relay_allowed"`
	RelayOnly         bool   `json:"relay_only"`
	ExpectedStaticPub string `json:"expected_agent_static_pub"`
	Purpose           string `json:"purpose"`
	jwt.RegisteredClaims
}

type AgentRelayClaims struct {
	SID     string `json:"sid"`
	AgentID string `json:"agent_id"`
	Purpose string `json:"purpose"`
	jwt.RegisteredClaims
}

func SignBrowserPolicyJWT(secret string, claims BrowserPolicyClaims, issuedAt time.Time) (string, error) {
	claims.Purpose = "browser_policy"
	claims.RegisteredClaims = jwt.RegisteredClaims{
		ID:        claims.RegisteredClaims.ID,
		IssuedAt:  jwt.NewNumericDate(issuedAt),
		ExpiresAt: jwt.NewNumericDate(issuedAt.Add(TokenLifetime)),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

func VerifyBrowserPolicyJWT(secret, token string, now time.Time) (*BrowserPolicyClaims, error) {
	parser := jwt.NewParser(jwt.WithTimeFunc(func() time.Time { return now }), jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	claims := &BrowserPolicyClaims{}
	_, err := parser.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}
	if claims.Purpose != "browser_policy" {
		return nil, errors.New("relay: invalid token purpose")
	}
	return claims, nil
}

func SignAgentRelayJWT(secret string, claims AgentRelayClaims, issuedAt time.Time) (string, error) {
	claims.Purpose = "agent_relay"
	claims.RegisteredClaims = jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(issuedAt),
		ExpiresAt: jwt.NewNumericDate(issuedAt.Add(TokenLifetime)),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
}

func VerifyAgentRelayJWT(secret, token string, now time.Time) (*AgentRelayClaims, error) {
	parser := jwt.NewParser(jwt.WithTimeFunc(func() time.Time { return now }), jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	claims := &AgentRelayClaims{}
	_, err := parser.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}
	if claims.Purpose != "agent_relay" {
		return nil, errors.New("relay: invalid token purpose")
	}
	return claims, nil
}

func NewSID() string { return newTokenID("sid") }

func NewJTI() string { return newTokenID("jti") }

func newTokenID(prefix string) string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(buf)
}