// Package stun implements the agent half of the §10.1 control-observed STUN
// cross-check (plan Task 17). Control delivers a one-use short-term
// credential in a stun_challenge over the agent's authenticated WebSocket
// ({version, challenge, server, expires_at}); the client sends exactly ONE
// STUN Binding request to the control UDP 3478 listener with
// USERNAME=<challenge id> and MESSAGE-INTEGRITY keyed with the one-use
// secret, requires a transaction-matched and integrity-verified Binding
// response, and extracts the integrity-protected receipt attribute as opaque
// bytes. The caller echoes only {challenge, transaction_id, receipt} back
// over the WebSocket: never the secret (useless once burned and never
// requested again) and never the mapped address (control records the actual
// UDP source itself, §10.1 step 3).
//
// Failure semantics are fail-closed: a challenge that is expired, malformed,
// or of an unsupported version is dropped before any network activity; a
// challenge consumed by one request is never retried (the credential burns
// on the first request, valid or not, exactly as control's listener
// enforces). A lost or spoofed response therefore loses the observation and
// the agent falls back to relay per §10.3 — a re-request only happens when
// control issues a fresh challenge.
package stun

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/pion/stun/v3"
)

// AttrReceipt is the custom comprehension-optional attribute carrying the
// integrity-protected observation receipt in the Binding success response.
// Its value (0xFF01, private-use range) is a shared contract with the
// control-side listener (control/internal/stun.AttrReceipt); the VALUE of
// the attribute is opaque to this client — it is extracted bounded and
// echoed unmodified, never interpreted.
const AttrReceipt = stun.AttrType(0xFF01)

// ChallengeVersion is the exact wire version of the §11.1 stun_challenge
// message this client accepts; anything else fails closed.
const ChallengeVersion = 1

// DefaultResponseTimeout bounds the wait for the integrity-protected
// response. It is deliberately a sub-window of the 60-second challenge TTL
// (§10.1): a UDP RTT needs milliseconds, and the WebSocket echo must still
// land inside control's claim window afterwards.
const DefaultResponseTimeout = 5 * time.Second

// Wire bounds mirroring the control listener's issuance sizes: the challenge
// field packs "<hex(id)>.<hex(secret)>" (16-byte ID, 32-byte secret), and
// receipts are extracted only within a bounded length.
const (
	challengeIDHexLength     = 32  // 16-byte challenge ID, hex-encoded
	challengeSecretHexLength = 64  // 32-byte one-use secret, hex-encoded
	maxChallengeFieldLength  = 128 // 32 + 1 + 64 plus headroom
	maxReceiptSize           = 128 // control sends 16 bytes; reject anything larger
	maxDatagramSize          = 1500
)

// Fail-closed rejection taxonomy. All errors are safe to log: they never
// contain the challenge ID, the secret, or receipt material (§16.6).
var (
	ErrUnsupportedVersion = errors.New("stun challenge: unsupported version")
	ErrMalformedChallenge = errors.New("stun challenge: malformed challenge, server, or expiry fields")
	ErrChallengeExpired   = errors.New("stun challenge: expired")
	ErrServerUnresolvable = errors.New("stun challenge: server address unresolvable")
	ErrResponseTimeout    = errors.New("stun exchange: no valid integrity-protected response in time")
	ErrChallengeRejected  = errors.New("stun exchange: challenge rejected (expired, reused, or bad integrity)")
)

// Challenge is one validated one-use short-term credential. Construct it
// only through ParseChallenge; the fields are unexported so the packed wire
// material cannot bypass validation. The secret must never be logged and is
// never echoed anywhere (§16.6).
type Challenge struct {
	id        string
	secret    []byte
	server    string
	expiresAt time.Time
}

// ParseChallenge validates the §11.1 stun_challenge fields before use
// (§10.1: expired or invalid challenges are dropped with no request). The
// challenge wire field packs the credential as
// "<hex challenge id>.<hex one-use secret>"; the ID doubles as the STUN
// USERNAME, the secret keys MESSAGE-INTEGRITY. now is the caller's single
// clock reading; Exchange re-checks expiry against its own reading.
func ParseChallenge(version int, challengeField string, server string, expiresAt time.Time, now time.Time) (Challenge, error) {
	if version != ChallengeVersion {
		return Challenge{}, fmt.Errorf("%w %d, want %d", ErrUnsupportedVersion, version, ChallengeVersion)
	}
	if expiresAt.IsZero() {
		return Challenge{}, fmt.Errorf("%w: missing expiry", ErrMalformedChallenge)
	}
	if challengeField == "" || len(challengeField) > maxChallengeFieldLength {
		return Challenge{}, fmt.Errorf("%w: challenge field length out of range", ErrMalformedChallenge)
	}
	idPart, secretPart, found := strings.Cut(challengeField, ".")
	if !found || strings.Contains(secretPart, ".") {
		return Challenge{}, fmt.Errorf("%w: challenge field is not <id>.<secret>", ErrMalformedChallenge)
	}
	if len(idPart) != challengeIDHexLength || len(secretPart) != challengeSecretHexLength {
		return Challenge{}, fmt.Errorf("%w: challenge id or secret length out of range", ErrMalformedChallenge)
	}
	secret, err := hex.DecodeString(secretPart)
	if err != nil {
		return Challenge{}, fmt.Errorf("%w: secret is not hex", ErrMalformedChallenge)
	}
	if _, err := hex.DecodeString(idPart); err != nil {
		return Challenge{}, fmt.Errorf("%w: challenge id is not hex", ErrMalformedChallenge)
	}
	if err := validateServerField(server); err != nil {
		return Challenge{}, err
	}
	challenge := Challenge{
		id:        idPart,
		secret:    secret,
		server:    server,
		expiresAt: expiresAt,
	}
	if err := challenge.Validate(now); err != nil {
		return Challenge{}, err
	}
	return challenge, nil
}

// validateServerField checks the wire `server` value syntactically: a
// bounded host:port with a usable port. Name resolution stays in Exchange so
// parse-time validation is deterministic.
func validateServerField(server string) error {
	if server == "" || len(server) > 253+6 {
		return fmt.Errorf("%w: server field length out of range", ErrMalformedChallenge)
	}
	_, portText, err := net.SplitHostPort(server)
	if err != nil {
		return fmt.Errorf("%w: server is not host:port", ErrMalformedChallenge)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("%w: server port out of range", ErrMalformedChallenge)
	}
	return nil
}

// Validate re-checks the challenge against a single clock reading. Exchange
// calls it right before any network activity so an expired credential can
// never produce a request.
func (challenge Challenge) Validate(now time.Time) error {
	if !now.Before(challenge.expiresAt) {
		return ErrChallengeExpired
	}
	return nil
}

// Result is the observation echo for the §11.1 stun_result message:
// {challenge, transaction_id, receipt}. Receipt holds the opaque
// integrity-protected attribute bytes (16 bytes today) echoed unmodified;
// the mapped address is deliberately absent — control records the actual
// UDP source itself.
type Result struct {
	ChallengeID   string
	TransactionID string
	Receipt       []byte
}

// ClientOption adjusts test-visible knobs (clock seam, response timeout).
type ClientOption func(*Client)

// withNow overrides the clock seam (tests use it to prove expiry).
func withNow(now func() time.Time) ClientOption {
	return func(client *Client) { client.now = now }
}

// withResponseTimeout overrides the sub-window UDP response deadline.
func withResponseTimeout(timeout time.Duration) ClientOption {
	return func(client *Client) { client.responseTimeout = timeout }
}

// Client exchanges one challenge for one observation receipt. It holds no
// per-challenge state, so a single client may serve every challenge the
// daemon receives.
type Client struct {
	now             func() time.Time
	responseTimeout time.Duration
}

// NewClient returns a client with production defaults.
func NewClient(options ...ClientOption) *Client {
	client := &Client{
		now:             time.Now,
		responseTimeout: DefaultResponseTimeout,
	}
	for _, option := range options {
		option(client)
	}
	return client
}

// Exchange runs the §10.1 agent flow for one challenge: validate (single
// clock reading, no request on failure), resolve the server, send exactly
// ONE integrity-protected Binding request, and wait for a
// transaction-matched, integrity-verified response carrying a bounded
// receipt. Garbage or spoofed datagrams never win: anything that fails
// validation is skipped until the deadline. The request is never
// retransmitted — the credential burns on first use by design.
func (client *Client) Exchange(ctx context.Context, challenge Challenge) (Result, error) {
	now := client.now() // single clock reading: expiry and deadline derive from it
	if err := challenge.Validate(now); err != nil {
		return Result{}, err
	}
	serverAddr, err := net.ResolveUDPAddr("udp", challenge.server)
	if err != nil {
		return Result{}, fmt.Errorf("%w %q: %v", ErrServerUnresolvable, challenge.server, err)
	}

	deadline := now.Add(client.responseTimeout)
	if ctxDeadline, hasDeadline := ctx.Deadline(); hasDeadline && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	conn, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		return Result{}, fmt.Errorf("%w: dial: %v", ErrServerUnresolvable, err)
	}
	defer conn.Close()

	// Cancellation watcher: closes the socket when ctx is done so a
	// cancelled caller is not stuck until the read deadline. It always
	// terminates (ctx.Done or the deferred close signal), so nothing leaks.
	watcherDone := make(chan struct{})
	defer close(watcherDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-watcherDone:
		}
	}()

	request, err := buildBindingRequest(challenge.id, challenge.secret)
	if err != nil {
		return Result{}, fmt.Errorf("%w: build request: %v", ErrMalformedChallenge, err)
	}
	// Exactly one send per challenge (§10.1: challenges are single-use).
	if _, err := conn.Write(request.Raw); err != nil {
		return Result{}, fmt.Errorf("%w: send: %v", ErrResponseTimeout, err)
	}

	_ = conn.SetReadDeadline(deadline)
	buffer := make([]byte, maxDatagramSize)
	for {
		datagramSize, err := conn.Read(buffer)
		if err != nil {
			return Result{}, fmt.Errorf("%w: awaiting response: %v", ErrResponseTimeout, err)
		}
		result, outcome := parseSTUNResponse(challenge, request.TransactionID, buffer[:datagramSize])
		switch outcome {
		case responseAccepted:
			return result, nil
		case responseRejected:
			// A known-transaction error response means the credential is
			// dead at the listener (expired, reused, bad integrity): no
			// valid response can follow, so stop instead of waiting out the
			// deadline. A forged error can at worst suppress the echo —
			// fail-closed either way.
			return Result{}, ErrChallengeRejected
		case responseSkip:
			// Not ours, malformed, unmatched transaction, bad integrity, or
			// unusable receipt: keep reading until the deadline.
		}
	}
}

// buildBindingRequest constructs the §10.1 Binding request: USERNAME carries
// the cleartext challenge ID (how control identifies the one-use credential)
// and MESSAGE-INTEGRITY — added last, so its HMAC covers exactly the wire
// bytes — is keyed with the one-use secret (RFC 8489 §9 short-term
// credentials). The message is never re-encoded after the integrity pass.
func buildBindingRequest(challengeID string, secret []byte) (*stun.Message, error) {
	request := stun.New()
	if err := request.Build(
		stun.TransactionID,
		stun.BindingRequest,
		stun.NewUsername(challengeID),
		stun.NewShortTermIntegrity(string(secret)),
	); err != nil {
		return nil, err
	}
	return request, nil
}

// responseOutcome classifies one received datagram.
type responseOutcome int

const (
	responseSkip     responseOutcome = iota // not ours or not acceptable: keep reading
	responseRejected                        // known-transaction error response: credential dead
	responseAccepted
)

// parseSTUNResponse validates one datagram against the challenge (whose ID
// doubles as the §11.1 echo value) and the one-use secret. Only a Binding
// success response with a matching transaction ID, valid MESSAGE-INTEGRITY,
// and a bounded receipt attribute is accepted; the receipt is copied out as
// opaque bytes.
func parseSTUNResponse(challenge Challenge, transactionID [stun.TransactionIDSize]byte, datagram []byte) (Result, responseOutcome) {
	if len(datagram) == 0 || !stun.IsMessage(datagram) {
		return Result{}, responseSkip
	}
	response := stun.New()
	// Copy: Decode and Get alias their input; the caller's read buffer is
	// reused across datagrams.
	response.Raw = append([]byte(nil), datagram...)
	if err := response.Decode(); err != nil {
		return Result{}, responseSkip
	}
	if response.TransactionID != transactionID {
		return Result{}, responseSkip // someone else's transaction: ignore
	}
	switch response.Type {
	case stun.BindingError:
		return Result{}, responseRejected
	case stun.BindingSuccess:
		// MESSAGE-INTEGRITY is mandatory before anything is accepted.
		if !response.Contains(stun.AttrMessageIntegrity) {
			return Result{}, responseSkip
		}
		if err := stun.NewShortTermIntegrity(string(challenge.secret)).Check(response); err != nil {
			return Result{}, responseSkip // spoofed or corrupted: keep reading
		}
		receipt, err := response.Get(AttrReceipt)
		if err != nil || len(receipt) == 0 || len(receipt) > maxReceiptSize {
			return Result{}, responseSkip
		}
		return Result{
			ChallengeID:   challenge.id,
			TransactionID: hex.EncodeToString(transactionID[:]),
			Receipt:       append([]byte(nil), receipt...),
		}, responseAccepted
	default:
		return Result{}, responseSkip
	}
}
