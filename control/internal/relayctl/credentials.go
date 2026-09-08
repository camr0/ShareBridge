// Package relayctl owns control-side relay tunnel policy: stable relay-port
// assignment (§4.5/§12), short-lived Ed25519-signed tunnel credentials
// (§7.2), the control→agent relay_config wire message (§11.1), and the
// deterministic relay-origin derivation (§6). Later tasks grow this package
// with the control↔gateway protocol, route publisher and presence view; it
// deliberately depends only on PocketBase core so that growth stays
// acyclic (directctl may import relayctl, never the reverse).
package relayctl

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// Issuer and Audience are the normative §7.2 credential claims every token
// carries; the gateway's authorization adapter rejects any other audience.
const (
	Issuer   = "sharebridge-control"
	Audience = "sharebridge-relay"
)

// CredentialTTL is the §7.2 admission lifetime: credentials are short-lived
// (default ten minutes), while an accepted tunnel may stay connected beyond
// token expiry; a reconnect always requires a freshly issued credential.
const CredentialTTL = 10 * time.Minute

// tokenPrefix versions the credential envelope so the gateway can fail closed
// on unknown formats: "<prefix>.<base64url(payload)>.<base64url(signature)>".
const tokenPrefix = "sbrelay1"

// PortRange is the operator-configured narrow range of gateway-loopback FRP
// proxy ports (§4.5: frps allowPorts). Allocation never leaves it.
type PortRange struct {
	Min int
	Max int
}

// NewPortRange validates an inclusive [min, max] range inside the usable TCP
// port space.
func NewPortRange(min, max int) (PortRange, error) {
	if min < 1024 || max > 65535 || min > max {
		return PortRange{}, fmt.Errorf("invalid relay port range [%d, %d]", min, max)
	}
	return PortRange{Min: min, Max: max}, nil
}

// Contains reports whether port lies inside the range.
func (r PortRange) Contains(port int) bool {
	return port >= r.Min && port <= r.Max
}

// Size returns the number of ports in the range.
func (r PortRange) Size() int {
	return r.Max - r.Min + 1
}

// RelayAssignment is the control-owned tunnel assignment shared with later
// tasks (publisher, presence): agent record ID, namespace, proxy name, relay
// port and generation.
type RelayAssignment struct {
	AgentRecordID string
	Namespace     string
	ProxyName     string
	RelayPort     int
	Generation    int
}

// CredentialClaims carries exactly the normative §7.2 claim set. The JSON keys
// are the signed payload; unknown keys are ignored on parse.
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

// Signer signs relay credentials with an Ed25519 relay-auth key. Control holds
// the private key; the gateway holds only PublicKey() (§7.2).
type Signer struct {
	privateKey ed25519.PrivateKey
	// nowFn returns the signing time. nil means time.Now; tests substitute a
	// fixed clock to prove reconnect-time freshness.
	nowFn func() time.Time
}

// NewSigner builds a signer from a 32-byte Ed25519 seed (operator-provisioned,
// e.g. hex-decoded from the environment).
func NewSigner(seed []byte) (*Signer, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("relay auth seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	return &Signer{privateKey: ed25519.NewKeyFromSeed(seed)}, nil
}

// NewSignerFromHexSeed builds a signer from the operator-provisioned 64-character
// hex Ed25519 seed (config RELAY_AUTH_KEY_SEED). The gateway holds only the
// matching public key (§7.2).
func NewSignerFromHexSeed(seedHex string) (*Signer, error) {
	seed, err := hex.DecodeString(seedHex)
	if err != nil {
		return nil, fmt.Errorf("relay auth seed is not valid hex: %w", err)
	}
	return NewSigner(seed)
}

// GenerateSigner creates a signer with a fresh random key (test/development
// convenience; production keys come from persistent operator state).
func GenerateSigner() (*Signer, error) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Signer{privateKey: privateKey}, nil
}

// PublicKey returns the verification key handed to the relay gateway.
func (s *Signer) PublicKey() ed25519.PublicKey {
	publicKey, _ := s.privateKey.Public().(ed25519.PublicKey)
	return publicKey
}

// SetNowFunc overrides the signing clock (test-only). A nil fn restores
// time.Now.
func (s *Signer) SetNowFunc(fn func() time.Time) {
	s.nowFn = fn
}

func (s *Signer) now() time.Time {
	if s.nowFn != nil {
		return s.nowFn()
	}
	return time.Now()
}

// SignCredential mints a fresh credential for the assignment: issued_at now,
// expires_at now+CredentialTTL, a fresh random JTI, and an Ed25519 signature
// over the exact claim payload. Credential material is returned to the caller
// for delivery inside the authenticated agent WebSocket; it is never logged.
func (s *Signer) SignCredential(assignment RelayAssignment, apiKeyID string) (CredentialClaims, []byte, error) {
	if apiKeyID == "" {
		return CredentialClaims{}, nil, errors.New("api key id required for credential")
	}
	if assignment.AgentRecordID == "" || assignment.Namespace == "" || assignment.ProxyName == "" {
		return CredentialClaims{}, nil, errors.New("incomplete relay assignment")
	}
	if assignment.RelayPort <= 0 || assignment.RelayPort > 65535 {
		return CredentialClaims{}, nil, fmt.Errorf("relay port %d out of range", assignment.RelayPort)
	}
	if assignment.Generation < 0 {
		return CredentialClaims{}, nil, fmt.Errorf("relay generation %d out of range", assignment.Generation)
	}

	issuedAt := s.now().UTC()
	claims := CredentialClaims{
		Issuer:        Issuer,
		Audience:      Audience,
		APIKeyID:      apiKeyID,
		AgentRecordID: assignment.AgentRecordID,
		Namespace:     assignment.Namespace,
		ProxyName:     assignment.ProxyName,
		RelayPort:     assignment.RelayPort,
		Generation:    assignment.Generation,
		IssuedAt:      issuedAt,
		ExpiresAt:     issuedAt.Add(CredentialTTL),
	}
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return CredentialClaims{}, nil, fmt.Errorf("generate jti: %w", err)
	}
	claims.JTI = hex.EncodeToString(jti)

	payload, err := json.Marshal(claims)
	if err != nil {
		return CredentialClaims{}, nil, err
	}
	signature := ed25519.Sign(s.privateKey, payload)
	token := tokenPrefix + "." +
		base64RawURL(payload) + "." +
		base64RawURL(signature)
	return claims, []byte(token), nil
}

// VerifyCredential checks the token envelope and Ed25519 signature and returns
// the verified claims. Semantic admission checks (audience, expiry, replay,
// generation currency) belong to the gateway's authorization adapter; this
// only proves authenticity and parseability of the claim set.
func VerifyCredential(publicKey ed25519.PublicKey, token []byte) (CredentialClaims, error) {
	var claims CredentialClaims
	parts := strings.Split(string(token), ".")
	if len(parts) != 3 || parts[0] != tokenPrefix {
		return claims, errors.New("malformed relay credential envelope")
	}
	payload, err := decodeBase64RawURL(parts[1])
	if err != nil {
		return claims, fmt.Errorf("credential payload: %w", err)
	}
	signature, err := decodeBase64RawURL(parts[2])
	if err != nil {
		return claims, fmt.Errorf("credential signature: %w", err)
	}
	if !ed25519.Verify(publicKey, payload, signature) {
		return claims, errors.New("invalid relay credential signature")
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return claims, fmt.Errorf("credential claims: %w", err)
	}
	return claims, nil
}

// base64RawURL encodes without padding to keep tokens compact and URL-safe.
func base64RawURL(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func decodeBase64RawURL(encoded string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(encoded)
}

// EnsureAssignment returns the agent's relay assignment, allocating a stable
// port from the configured range when needed (§4.5/§12):
//
//   - an agent with an in-range nonzero relay_port keeps it (stability across
//     reconnects and epochs) and its generation does not move;
//   - a first allocation picks a random free port in the range and starts at
//     generation 1;
//   - an existing port that fell outside the (re)configured range is
//     reassigned and the generation increments, fencing every outstanding
//     credential from the superseded assignment;
//   - allocation is transactional and retries the partial unique index race
//     (idx_agents_relay_port) with a fresh candidate, so two agents can never
//     share a nonzero port.
func EnsureAssignment(app core.App, agentRecord *core.Record, portRange PortRange) (RelayAssignment, error) {
	var assignment RelayAssignment
	err := app.RunInTransaction(func(txApp core.App) error {
		a, err := ensureAssignmentTx(txApp, agentRecord, portRange)
		if err != nil {
			return err
		}
		assignment = a
		return nil
	})
	return assignment, err
}

func ensureAssignmentTx(txApp core.App, agentRecord *core.Record, portRange PortRange) (RelayAssignment, error) {
	// Re-read inside the transaction so a concurrent rotation/allocation on
	// this row cannot be clobbered by our save.
	persisted, err := txApp.FindRecordById("agents", agentRecord.Id)
	if err != nil {
		return RelayAssignment{}, fmt.Errorf("load agent: %w", err)
	}

	currentPort := persisted.GetInt("relay_port")
	currentGeneration := persisted.GetInt("relay_generation")
	namespace := persisted.GetString("namespace")
	if namespace == "" {
		return RelayAssignment{}, errors.New("agent namespace missing")
	}

	if currentPort != 0 && portRange.Contains(currentPort) {
		// Stable: keep port and generation across reconnects and epochs.
		return RelayAssignment{
			AgentRecordID: persisted.Id,
			Namespace:     namespace,
			ProxyName:     ProxyNameFor(namespace),
			RelayPort:     currentPort,
			Generation:    currentGeneration,
		}, nil
	}
	if currentPort == 0 || !portRange.Contains(currentPort) {
		// First allocation (0 → 1) or reassignment of an out-of-range port
		// (bump fences every outstanding credential of the old assignment,
		// §7.2/§12).
		currentGeneration++
	}

	// Pick a random port from the free set; on a lost race against a concurrent
	// allocation (unique-violation), retry with a fresh candidate.
	free, err := freeRelayPortsTx(txApp, portRange)
	if err != nil {
		return RelayAssignment{}, err
	}
	if len(free) == 0 {
		return RelayAssignment{}, fmt.Errorf("relay port range [%d, %d] exhausted", portRange.Min, portRange.Max)
	}

	const maxAttempts = 16
	var lastErr error
	for attempt := 0; attempt < maxAttempts && len(free) > 0; attempt++ {
		candidate := free[randomInt(len(free))]
		persisted.Set("relay_port", candidate)
		persisted.Set("relay_generation", currentGeneration)
		if err := txApp.Save(persisted); err != nil {
			lastErr = err
			if !isUniqueViolation(err) {
				return RelayAssignment{}, err // real validation/DB error
			}
			// Another transaction took the candidate: drop it and retry.
			free = removePort(free, candidate)
			continue
		}
		return RelayAssignment{
			AgentRecordID: persisted.Id,
			Namespace:     namespace,
			ProxyName:     ProxyNameFor(namespace),
			RelayPort:     candidate,
			Generation:    currentGeneration,
		}, nil
	}
	if lastErr != nil {
		return RelayAssignment{}, fmt.Errorf("relay port allocation failed: %w", lastErr)
	}
	return RelayAssignment{}, fmt.Errorf("relay port range [%d, %d] exhausted", portRange.Min, portRange.Max)
}

// freeRelayPortsTx returns the ports of the range not currently assigned to
// any agent (the partial unique index only constrains nonzero ports).
func freeRelayPortsTx(txApp core.App, portRange PortRange) ([]int, error) {
	rows := []struct {
		RelayPort int `db:"relay_port"`
	}{}
	err := txApp.DB().
		NewQuery(`SELECT relay_port FROM agents WHERE relay_port IS NOT NULL AND relay_port != 0`).
		All(&rows)
	if err != nil {
		return nil, err
	}
	taken := make(map[int]bool, len(rows))
	for _, row := range rows {
		taken[row.RelayPort] = true
	}
	free := make([]int, 0, portRange.Size())
	for port := portRange.Min; port <= portRange.Max; port++ {
		if !taken[port] {
			free = append(free, port)
		}
	}
	return free, nil
}

func removePort(ports []int, port int) []int {
	filtered := ports[:0]
	for _, candidate := range ports {
		if candidate != port {
			filtered = append(filtered, candidate)
		}
	}
	return filtered
}

func randomInt(upperBound int) int {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(upperBound)))
	if err != nil {
		return 0 // crypto/rand failure is catastrophic; 0 still yields a valid candidate
	}
	return int(n.Int64())
}

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint failure,
// matching both the raw driver text and PocketBase's normalized validation
// error. Duplicated from directctl on purpose: relayctl must not import
// directctl (the dependency direction is directctl → relayctl).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint failed") ||
		strings.Contains(msg, "value must be unique")
}

// ProxyNameFor derives the deterministic single-proxy name control assigns for
// the agent's namespace (§4.5: the agent never accepts arbitrary proxy
// definitions). Namespaces are "sb"+8 lowercase hex, so the result is bounded
// and charset-safe for FRP.
func ProxyNameFor(namespace string) string {
	return "sb-" + namespace
}

// RelayOriginFromDirect deterministically derives the §6 relay origin by
// inserting ".relay." before the namespace component of the persisted direct
// origin ("label.namespace.domain" → "label.relay.namespace.domain").
func RelayOriginFromDirect(directOrigin string) (string, error) {
	firstDot := strings.Index(directOrigin, ".")
	if firstDot <= 0 || firstDot == len(directOrigin)-1 {
		return "", fmt.Errorf("not a direct origin: %q", directOrigin)
	}
	return directOrigin[:firstDot] + ".relay." + directOrigin[firstDot+1:], nil
}

// RelayConfigVersion versions the §11.1 relay_config wire message.
const RelayConfigVersion = 1

// RelayConfigMessage is the exact control→agent relay_config payload (§11.1):
// { version, generation, gateway_addr, gateway_port, proxy_name, relay_port,
// credential, expires_at }, plus the standard wire framing key "type" shared
// by every message on the agent WebSocket.
type RelayConfigMessage struct {
	Type        string `json:"type"`
	Version     int    `json:"version"`
	Generation  int    `json:"generation"`
	GatewayAddr string `json:"gateway_addr"`
	GatewayPort int    `json:"gateway_port"`
	ProxyName   string `json:"proxy_name"`
	RelayPort   int    `json:"relay_port"`
	Credential  string `json:"credential"`
	ExpiresAt   string `json:"expires_at"`
}

// Settings is the operator-supplied relay tunnel policy control needs to emit
// relay_config.
type Settings struct {
	GatewayAddr string // public relay gateway hostname agents connect to
	GatewayPort int    // public FRP transport port
	PortRange   PortRange
}

// Validate rejects an unusable policy up front.
func (s Settings) Validate() error {
	if strings.TrimSpace(s.GatewayAddr) == "" {
		return errors.New("relay gateway address required")
	}
	if s.GatewayPort <= 0 || s.GatewayPort > 65535 {
		return fmt.Errorf("relay gateway port %d out of range", s.GatewayPort)
	}
	if _, err := NewPortRange(s.PortRange.Min, s.PortRange.Max); err != nil {
		return err
	}
	return nil
}

// BuildRelayConfig assigns-and-signs: it mints a fresh credential for the
// assignment and renders the §11.1 wire message. The credential lives only in
// the returned message; callers must never log it.
func BuildRelayConfig(assignment RelayAssignment, apiKeyID string, gatewayAddr string, gatewayPort int, signer *Signer) (RelayConfigMessage, error) {
	claims, token, err := signer.SignCredential(assignment, apiKeyID)
	if err != nil {
		return RelayConfigMessage{}, err
	}
	return RelayConfigMessage{
		Type:        "relay_config",
		Version:     RelayConfigVersion,
		Generation:  claims.Generation,
		GatewayAddr: gatewayAddr,
		GatewayPort: gatewayPort,
		ProxyName:   claims.ProxyName,
		RelayPort:   claims.RelayPort,
		Credential:  string(token),
		ExpiresAt:   claims.ExpiresAt.Format(time.RFC3339),
	}, nil
}

// Relay client telemetry bounds (§11.1/§12): statuses are a closed enum and
// reasons are bounded at the application layer regardless of schema limits.
const (
	maxTelemetryReasonLen = 256
)

// ValidateRelayClientState validates the bounded §11.1 relay_client_state
// payload: generation must be non-negative, status one of the four exact
// values, and the optional reason at most 256 characters. It is validation
// only: the message is telemetry and can never create relay availability or
// mutate share lifecycle.
func ValidateRelayClientState(generation int, status string, reason string) error {
	if generation < 0 {
		return fmt.Errorf("relay client generation %d out of range", generation)
	}
	switch status {
	case "starting", "running", "stopped", "error":
	default:
		return fmt.Errorf("invalid relay client status %q", status)
	}
	if len(reason) > maxTelemetryReasonLen {
		return fmt.Errorf("relay client reason exceeds %d characters", maxTelemetryReasonLen)
	}
	return nil
}

// ValidateLockdownStatus validates the bounded §11.1 lockdown_status payload:
// advisory fast-path telemetry only; it may suppress attempts but can never
// establish route availability or change share lifecycle.
func ValidateLockdownStatus(generation int, locked bool) error {
	if generation < 0 {
		return fmt.Errorf("lockdown generation %d out of range", generation)
	}
	return nil
}

// RelayCredentialRequestReason enumerates the exact §11.1
// relay_credential_request reason values. Anything else is rejected before
// any state is touched: the message is the trigger for a freshly signed
// credential, so the parse is strict.
const (
	ReasonReplayRejected = "replay_rejected"
	ReasonExpired        = "expired"
	ReasonRestart        = "restart"
)

// ValidateRelayCredentialRequest validates the bounded §11.1
// relay_credential_request payload: the reason must be exactly one of the
// three enumerated values. Rejection diagnostics never include the raw
// agent-supplied value beyond its length, so unbounded input cannot reach
// the log.
func ValidateRelayCredentialRequest(reason string) error {
	switch reason {
	case ReasonReplayRejected, ReasonExpired, ReasonRestart:
		return nil
	default:
		return fmt.Errorf("invalid relay credential request reason (len=%d, want replay_rejected|expired|restart)", len(reason))
	}
}
