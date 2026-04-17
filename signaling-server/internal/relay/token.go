package relay

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims is the ShareBridge JWT payload.
type Claims struct {
	JTI           string `json:"jti"`
	ShareCode     string `json:"share_code"`
	BrowserPeerID string `json:"browser_peer_id"`
	RelayAllowed  bool   `json:"relay_allowed"`
	DCUtRAllowed  bool   `json:"dcutr_allowed"`
	ExpiresAt     int64  `json:"exp"`
}

// Issuer signs and validates JWTs with HS256.
type Issuer struct {
	secret []byte
	ttl    time.Duration
}

func NewIssuer(secret []byte, ttl time.Duration) *Issuer {
	return &Issuer{secret: secret, ttl: ttl}
}

func (i *Issuer) Issue(c Claims) (string, error) {
	if len(i.secret) < 32 {
		return "", errors.New("jwt secret must be >= 32 bytes")
	}
	if c.JTI == "" {
		c.JTI = randomHex(16)
	}
	c.ExpiresAt = time.Now().Add(i.ttl).Unix()
	mapClaims := jwt.MapClaims{
		"jti": c.JTI, "share_code": c.ShareCode,
		"browser_peer_id": c.BrowserPeerID,
		"relay_allowed": c.RelayAllowed, "dcutr_allowed": c.DCUtRAllowed,
		"exp": c.ExpiresAt,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, mapClaims).SignedString(i.secret)
}

func (i *Issuer) Validate(tok string) (*Claims, error) {
	parsed, err := jwt.Parse(tok, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return i.secret, nil
	})
	if err != nil {
		return nil, err
	}
	mc, ok := parsed.Claims.(jwt.MapClaims)
	if !ok || !parsed.Valid {
		return nil, errors.New("invalid claims")
	}
	c := &Claims{}
	if v, ok := mc["jti"].(string); ok {
		c.JTI = v
	}
	if v, ok := mc["share_code"].(string); ok {
		c.ShareCode = v
	}
	if v, ok := mc["browser_peer_id"].(string); ok {
		c.BrowserPeerID = v
	}
	if v, ok := mc["relay_allowed"].(bool); ok {
		c.RelayAllowed = v
	}
	if v, ok := mc["dcutr_allowed"].(bool); ok {
		c.DCUtRAllowed = v
	}
	if v, ok := mc["exp"].(float64); ok {
		c.ExpiresAt = int64(v)
	}
	if c.JTI == "" {
		return nil, errors.New("missing jti")
	}
	return c, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}