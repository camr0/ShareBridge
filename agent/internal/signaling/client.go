package signaling

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"sharebridge/agent/internal/tunnel"
)

// Message is any message received from the signaling server.
type Message struct {
	Type           string `json:"type"`
	Err            string `json:"message,omitempty"`
	Code           string `json:"code,omitempty"`
	Reconnected    bool   `json:"reconnected,omitempty"`
	ExpiresAt      string `json:"expires_at,omitempty"`
	Origin         string `json:"origin,omitempty"`
	CSRPEM         string `json:"csr_pem,omitempty"`
	Fingerprint    string `json:"fingerprint,omitempty"`
	NotAfter       string `json:"not_after,omitempty"`
	IP             string `json:"ip,omitempty"`
	Port           int    `json:"port,omitempty"`
	Status         string `json:"status,omitempty"`
	Nonce          string `json:"nonce,omitempty"`
	Seq            uint64 `json:"seq,omitempty"`
	ShareID        string `json:"share_id,omitempty"`
	Route          string `json:"route,omitempty"`
	LeaseSeconds   int    `json:"lease_seconds,omitempty"`
	Version        int    `json:"version,omitempty"`
	GrantedPort    int    `json:"granted_port,omitempty"`
	PublicIP       string `json:"public_ip,omitempty"`
	WasAlreadyOpen bool   `json:"was_already_open,omitempty"`
	Error          string `json:"error,omitempty"`
	Namespace      string `json:"namespace,omitempty"`
	ChainPEM       string `json:"chain_pem,omitempty"`
	Reason         string `json:"reason,omitempty"`
	AgentID        string `json:"agent_id,omitempty"`

	// Relay/STUN wire fields (§11.1). Additive with omitempty so every
	// existing direct-message JSON shape is unchanged.
	RelayOrigin string `json:"relay_origin,omitempty"` // share_registered
	Challenge   string `json:"challenge,omitempty"`    // stun_challenge packed credential
	Server      string `json:"server,omitempty"`       // stun_challenge UDP listener host:port

	// Raw carries the undecoded JSON payload of this message on the receive
	// path (Listen populates it before dispatch; locally constructed messages
	// leave it empty). Strict re-parsing of versioned control messages —
	// relay_config (§11.1) — runs against the raw bytes so unknown fields and
	// trailing data are rejected exactly once, by the one shared parser.
	Raw json.RawMessage `json:"-"`
}

// OpenAck is the agent-side acknowledgement of an open_signal. Its json tags
// mirror the control-side directctl.OpenAck fields exactly.
type OpenAck struct {
	ShareID        string `json:"share_id"`
	Nonce          string `json:"nonce"`
	Seq            uint64 `json:"seq"`
	GrantedPort    int    `json:"granted_port"`
	PublicIP       string `json:"public_ip"`
	WasAlreadyOpen bool   `json:"was_already_open"`
	Status         string `json:"status"`
	Error          string `json:"error,omitempty"`

	// Validation is the OK-ack guard's validation context (see
	// OpenAckValidation). The daemon's single OK-ack writer fills it from the
	// direct-state generation the open was admitted under. It is process-local
	// metadata and is never serialized: it carries `json:"-"`, and the send
	// path inspects the in-memory value rather than the wire bytes.
	Validation *OpenAckValidation `json:"-"`
}

// OpenAckValidation is the transport guard's validation context for a
// status-"ok" open_ack. A nil Validation on an OK ack means "no validation
// context", which the installed guard rejects: that is what makes an
// unvalidated OK ack fail closed rather than default to the zero generation.
type OpenAckValidation struct {
	// Epoch is the direct-state generation the open was admitted under.
	Epoch uint64
}

type RegisterShareOptions struct {
	ShareURL            string
	PreferredCode       string
	ShareType           string
	IsPasswordProtected bool
	RelayOnly           bool
}

// Client manages a WebSocket connection to the signaling server.
type Client struct {
	serverURL string
	apiKey    string
	agentID   string
	// conn is the current WebSocket. The reconnect loop swaps it in Connect
	// while the tunnel credential requester may Send concurrently, so every
	// access goes through connMu (Task 28 review Minor #3, fixed here).
	conn      *websocket.Conn
	connMu    sync.RWMutex
	OnMessage func(msg Message)
	mu        sync.Mutex

	// openAckGuard is the injected transport gate for status-"ok" open_acks.
	// It is installed once by the daemon (SetOpenAckGuard) and consulted by
	// OpenAck before anything is written. A nil guard makes every OK ack fail
	// closed; error acks never consult it. Guarded by openAckGuardMu.
	openAckGuard   func(OpenAck) error
	openAckGuardMu sync.RWMutex

	// pendingReg receives the share_registered (or error) response for the
	// RegisterShare call currently in flight. nil when no registration pending.
	// All WebSocket reads go through Listen, so RegisterShare must not call
	// conn.Read directly while Listen is running.
	pendingReg   chan Message
	pendingRegMu sync.Mutex
}

func New(serverURL, apiKey, agentID string) *Client {
	return &Client{
		serverURL: serverURL,
		apiKey:    apiKey,
		agentID:   agentID,
	}
}

// SetOpenAckGuard installs the one-time OK-ack approval gate on this client.
// Once installed, every status-"ok" OpenAck must be approved by the guard
// before any bytes are written; a missing or rejecting guard is a hard error
// and nothing reaches the wire. Error acks never consult the guard.
//
// The gate is installed once by construction so a later caller cannot widen
// it: a second install and a nil guard are both refused. A client that never
// installs one can still send error acks; only OK acks fail closed.
func (c *Client) SetOpenAckGuard(guard func(OpenAck) error) error {
	if guard == nil {
		return errors.New("signaling: cannot install a nil OK-ack guard")
	}
	c.openAckGuardMu.Lock()
	defer c.openAckGuardMu.Unlock()
	if c.openAckGuard != nil {
		return errors.New("signaling: OK-ack guard already installed")
	}
	c.openAckGuard = guard
	return nil
}

// openAckGuardSnapshot returns the installed OK-ack guard, if any.
func (c *Client) openAckGuardSnapshot() func(OpenAck) error {
	c.openAckGuardMu.RLock()
	defer c.openAckGuardMu.RUnlock()
	return c.openAckGuard
}

// Connect dials the signaling server with API key auth and sends hello.
func (c *Client) Connect(ctx context.Context) error {
	// WebSocket URL with api_key query param
	params := url.Values{}
	params.Set("api_key", c.apiKey)
	wsURL := c.serverURL + "/ws/agent?" + params.Encode()

	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial signaling server: %w", err)
	}
	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()

	// Send hello message
	if err := c.Send(ctx, map[string]string{
		"type":     "hello",
		"version":  "1.0",
		"agent_id": c.agentID,
	}); err != nil {
		conn.CloseNow()
		return fmt.Errorf("send hello: %w", err)
	}

	return nil
}

// RegisterShareWithOptions sends a register_share message and waits for the
// response. It must not call conn.Read directly — all reads go through Listen.
// The response is delivered via pendingReg, which Listen feeds.
//
// The §11.1 share_registered response carries BOTH origins: origin (the
// direct origin, returned unchanged here) and the additive relay_origin
// (parsed into Message.RelayOrigin). The direct origin is the only value
// callers need: the relay origin is deterministic from it (§6), and the
// daemon derives and binds the pair — both route kinds of one content
// session — via the Binder (§13.1). No agent message ever supplies either
// origin.
func (c *Client) RegisterShareWithOptions(ctx context.Context, opts RegisterShareOptions) (string, string, bool, error) {
	responseCh := make(chan Message, 1)
	c.pendingRegMu.Lock()
	if c.pendingReg != nil {
		c.pendingRegMu.Unlock()
		return "", "", false, fmt.Errorf("registration already in progress")
	}
	c.pendingReg = responseCh
	c.pendingRegMu.Unlock()
	defer func() {
		c.pendingRegMu.Lock()
		c.pendingReg = nil
		c.pendingRegMu.Unlock()
	}()

	msg := map[string]any{
		"type":       "register_share",
		"share_url":  opts.ShareURL,
		"relay_only": opts.RelayOnly,
	}
	if opts.PreferredCode != "" {
		msg["code"] = opts.PreferredCode
	}
	if opts.ShareType != "" {
		msg["share_type"] = opts.ShareType
	}
	if opts.IsPasswordProtected {
		msg["is_password_protected"] = true
	}
	if err := c.Send(ctx, msg); err != nil {
		return "", "", false, fmt.Errorf("send register_share: %w", err)
	}

	select {
	case resp := <-responseCh:
		if resp.Type == "error" {
			return "", "", false, fmt.Errorf("server error: %s", resp.Err)
		}
		return resp.Code, resp.Origin, resp.Reconnected, nil
	case <-ctx.Done():
		return "", "", false, ctx.Err()
	}
}

func (c *Client) UnregisterShare(ctx context.Context, code string) error {
	return c.Send(ctx, map[string]string{"type": "unregister_share", "code": code})
}

// DownloadComplete notifies server of completed download.
func (c *Client) DownloadComplete(ctx context.Context, code string, bytesTransferred int64) error {
	return c.Send(ctx, map[string]any{
		"type":              "download_complete",
		"code":              code,
		"bytes_transferred": bytesTransferred,
	})
}

// SubmitCSR submits a certificate signing request for the agent's namespace.
func (c *Client) SubmitCSR(ctx context.Context, csrPEM string) error {
	return c.Send(ctx, map[string]any{
		"type":    "csr_submit",
		"csr_pem": csrPEM,
	})
}

// ReportEndpoint reports the agent's public endpoint (IP + port). status is
// only present for "close_failed"; port 0 means closed, port > 0 means open.
func (c *Client) ReportEndpoint(ctx context.Context, ip string, port int, status string) error {
	msg := map[string]any{
		"type": "report_endpoint",
		"ip":   ip,
		"port": port,
	}
	if status != "" {
		msg["status"] = status
	}
	return c.Send(ctx, msg)
}

// openAckWire is the on-wire shape of an OpenAck. It adds the message type to
// the OpenAck fields; the embedded struct's json tags (and the non-serialized
// Validation context) carry through. Sending the whole value through Send means
// the transport's OK-ack seal sees the in-memory Validation context.
type openAckWire struct {
	Type string `json:"type"`
	OpenAck
}

// OpenAck acknowledges an open_signal with the granted port and public IP.
//
// A status-"ok" ack is the one privileged open_ack: it tells control the
// mapping is live and its endpoint report has been confirmed. It is therefore
// gated at the transport (see Send): it must carry a validation context and be
// approved by the installed OK-ack guard, or the ack is never written. Error
// acks are not gated and keep working with no guard.
func (c *Client) OpenAck(ctx context.Context, ack OpenAck) error {
	return c.Send(ctx, openAckWire{Type: "open_ack", OpenAck: ack})
}

// TLSReady reports a successfully installed leaf certificate.
func (c *Client) TLSReady(ctx context.Context, fingerprint, notAfter string) error {
	return c.Send(ctx, map[string]any{
		"type":        "tls_ready",
		"fingerprint": fingerprint,
		"not_after":   notAfter,
	})
}

// TLSError reports a certificate installation failure. The agent retries with
// backoff; the control plane treats this as a no-op beyond logging.
func (c *Client) TLSError(ctx context.Context, reason string) error {
	return c.Send(ctx, map[string]any{
		"type":   "tls_error",
		"reason": reason,
	})
}

// ---------- §11.1 relay/STUN wire helpers (plan Task 17) ----------

// maxTelemetryReasonBytes is control's relay_client_state telemetry reason
// bound (control/internal/relayctl rejects anything longer). The bounded
// senders below enforce the same bound so a diagnostic can never get the
// whole message rejected at control.
const maxTelemetryReasonBytes = 256

// Wire bounds for the §11.1 stun_result fields, mirroring what the agent's
// own STUN client (internal/stun) can produce: the challenge echo is the
// 32-hex-character challenge ID, transaction_id is the lowercase hex of the
// 12-byte STUN transaction ID, and receipt is the lowercase hex of the opaque
// integrity-protected attribute bytes (16 bytes today, bounded at 128 bytes).
const (
	maxSTUNResultChallengeLength = 128
	stunTransactionIDHexLength   = 2 * 12
	maxSTUNResultReceiptHexBytes = 2 * 128
)

// STUNChallenge is the parsed §11.1 control → agent stun_challenge payload:
// {version, challenge, server, expires_at}. Challenge is the packed one-use
// credential "<hex id>.<hex secret>" the stun client splits; Secret material
// stays inside the packed field and must never be logged.
type STUNChallenge struct {
	Version   int
	Challenge string
	Server    string
	ExpiresAt time.Time
}

// ParseSTUNChallenge decodes the exact §11.1 stun_challenge wire message.
// Per the Task 7 ruling (unknown-field tolerance for versioned wire
// compatibility, spec §11) unknown JSON fields are tolerated, but trailing
// data after the value is rejected. The parse is structural only: version,
// server, and expiry-vs-clock validation happen in the stun client before
// any request is made (fail-closed, internal/stun.ParseChallenge).
func ParseSTUNChallenge(data []byte) (STUNChallenge, error) {
	var message struct {
		Type      string `json:"type"`
		Version   int    `json:"version"`
		Challenge string `json:"challenge"`
		Server    string `json:"server"`
		ExpiresAt string `json:"expires_at"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&message); err != nil {
		return STUNChallenge{}, fmt.Errorf("decode stun_challenge: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return STUNChallenge{}, errors.New("trailing data after stun_challenge value")
	}
	if message.Type != "stun_challenge" {
		return STUNChallenge{}, fmt.Errorf("unexpected stun_challenge message type %q", message.Type)
	}
	expiresAt, err := time.Parse(time.RFC3339, message.ExpiresAt)
	if err != nil {
		return STUNChallenge{}, fmt.Errorf("parse stun_challenge expires_at: %w", err)
	}
	return STUNChallenge{
		Version:   message.Version,
		Challenge: message.Challenge,
		Server:    message.Server,
		ExpiresAt: expiresAt,
	}, nil
}

// STUNResult is the §11.1 agent → control stun_result payload:
// {challenge, transaction_id, receipt}. Receipt holds the opaque
// integrity-protected attribute bytes; SendSTUNResult hex-encodes them onto
// the wire. It never carries the one-use secret or the mapped address.
type STUNResult struct {
	Challenge     string
	TransactionID string
	Receipt       []byte
}

// SendSTUNResult echoes one observation receipt over the authenticated
// WebSocket. The payload is validated against the exact §11.1 shape before
// anything is written: control cannot claim an observation that deviates
// from it, so an invalid result fails closed here instead.
func (c *Client) SendSTUNResult(ctx context.Context, result STUNResult) error {
	if result.Challenge == "" || len(result.Challenge) > maxSTUNResultChallengeLength {
		return fmt.Errorf("stun_result challenge length %d out of range", len(result.Challenge))
	}
	if len(result.TransactionID) != stunTransactionIDHexLength || !isLowercaseHex(result.TransactionID) {
		return fmt.Errorf("stun_result transaction_id must be %d lowercase hex characters", stunTransactionIDHexLength)
	}
	if len(result.Receipt) == 0 || 2*len(result.Receipt) > maxSTUNResultReceiptHexBytes {
		return fmt.Errorf("stun_result receipt length %d bytes out of range", len(result.Receipt))
	}
	return c.Send(ctx, struct {
		Type          string `json:"type"`
		Challenge     string `json:"challenge"`
		TransactionID string `json:"transaction_id"`
		Receipt       string `json:"receipt"`
	}{
		Type:          "stun_result",
		Challenge:     result.Challenge,
		TransactionID: result.TransactionID,
		Receipt:       hex.EncodeToString(result.Receipt),
	})
}

// RelayClientStatus is one of the four closed §11.1 relay_client_state
// status values. The vocabulary must stay identical to the tunnel manager's
// diagnostics (internal/tunnel.StatusKind); the wire-shape test pins the
// equality so the two cannot drift.
type RelayClientStatus string

// The four statuses control accepts for relay_client_state telemetry.
const (
	RelayStatusStarting RelayClientStatus = "starting"
	RelayStatusRunning  RelayClientStatus = "running"
	RelayStatusStopped  RelayClientStatus = "stopped"
	RelayStatusError    RelayClientStatus = "error"
)

// RelayClientState is one §11.1 agent → control relay_client_state telemetry
// report: {generation, status, reason?}. Telemetry only — it can never
// create relay availability or mutate share lifecycle.
type RelayClientState struct {
	Generation int
	Status     RelayClientStatus
	Reason     string
}

// SendRelayClientState forwards one tunnel diagnostic to control. Generation
// must be non-negative and the status one of the four §11.1 values (the
// tunnel manager's StatusReport is the source of both); an oversize reason
// is truncated at control's telemetry bound rather than dropping the report.
func (c *Client) SendRelayClientState(ctx context.Context, state RelayClientState) error {
	if state.Generation < 0 {
		return fmt.Errorf("relay_client_state generation %d out of range", state.Generation)
	}
	switch state.Status {
	case RelayStatusStarting, RelayStatusRunning, RelayStatusStopped, RelayStatusError:
	default:
		return fmt.Errorf("invalid relay_client_state status %q", string(state.Status))
	}
	return c.Send(ctx, struct {
		Type       string `json:"type"`
		Generation int    `json:"generation"`
		Status     string `json:"status"`
		Reason     string `json:"reason,omitempty"`
	}{
		Type:       "relay_client_state",
		Generation: state.Generation,
		Status:     string(state.Status),
		Reason:     truncateTelemetryReason(state.Reason),
	})
}

// LockdownStatus is the §11.1 agent → control lockdown_status payload:
// {generation, locked}. Advisory fast-path telemetry only; it may suppress
// attempts but can never establish route availability or change share
// lifecycle.
type LockdownStatus struct {
	Generation int
	Locked     bool
}

// SendLockdownStatus reports one lockdown transition. locked is always sent
// (both polarities) so the §11.1 bool field stays explicit on the wire.
func (c *Client) SendLockdownStatus(ctx context.Context, status LockdownStatus) error {
	if status.Generation < 0 {
		return fmt.Errorf("lockdown_status generation %d out of range", status.Generation)
	}
	return c.Send(ctx, struct {
		Type       string `json:"type"`
		Generation int    `json:"generation"`
		Locked     bool   `json:"locked"`
	}{
		Type:       "lockdown_status",
		Generation: status.Generation,
		Locked:     status.Locked,
	})
}

// SendRelayCredentialRequest sends the §11.1 agent → control
// relay_credential_request message: { reason } — the tunnel manager's
// bounded, rate-limited request for a fresh relay_config after the one-use
// credential is burned (e.g. frps restart) or nearing expiry. The reason is
// the closed §11.1 enum shared with the tunnel manager
// (tunnel.CredentialRequestReason); an unknown reason fails closed here
// because control strictly parses it.
func (c *Client) SendRelayCredentialRequest(ctx context.Context, reason tunnel.CredentialRequestReason) error {
	switch reason {
	case tunnel.ReasonReplayRejected, tunnel.ReasonExpired, tunnel.ReasonRestart:
	default:
		return fmt.Errorf("invalid relay_credential_request reason %q", string(reason))
	}
	return c.Send(ctx, struct {
		Type   string `json:"type"`
		Reason string `json:"reason"`
	}{
		Type:   "relay_credential_request",
		Reason: string(reason),
	})
}

// ParseRelayConfig decodes the exact §11.1 control → agent relay_config wire
// message on the receive path. It is a deliberate re-export of Task 9's
// strict tunnel parser (unknown fields and trailing data rejected, bounded
// shapes, RFC3339 expiry) so there is exactly one parser for the message the
// credential requester waits for.
func ParseRelayConfig(data []byte) (tunnel.Config, error) {
	return tunnel.ParseRelayConfig(data)
}

// truncateTelemetryReason bounds a diagnostic reason at control's telemetry
// limit without splitting a UTF-8 rune (a cut rune would end the report in
// an invalid string, which encoding/json then mangles into U+FFFD). Losing a
// suffix of a reason is acceptable; losing the whole telemetry message is
// not.
func truncateTelemetryReason(reason string) string {
	if len(reason) <= maxTelemetryReasonBytes {
		return reason
	}
	cut := maxTelemetryReasonBytes
	// If the byte at the cut is a continuation byte, the rune it belongs to
	// started before the cut: move the cut back to that rune's start so the
	// result ends on a boundary.
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}
	return reason[:cut]
}

// isLowercaseHex reports whether s is a non-empty lowercase hex string.
func isLowercaseHex(s string) bool {
	if s == "" {
		return false
	}
	return strings.IndexFunc(s, func(character rune) bool {
		isHex := (character >= '0' && character <= '9') ||
			(character >= 'a' && character <= 'f')
		return !isHex
	}) < 0
}

// connSnapshot returns the current WebSocket under the connection lock. The
// reconnect loop swaps conn in Connect; every user (Send and Listen) must go
// through this accessor so the swap is race-free.
func (c *Client) connSnapshot() *websocket.Conn {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.conn
}

// Send serializes msg as JSON and writes it to the CURRENT WebSocket. It
// snapshots the connection once so a concurrent reconnect can only move a
// later send to the new socket, never tear this write.
//
// Send is ALSO the transport's OK-ack seal, so every outbound message — the
// typed OpenAck front-end and any raw struct or map a caller hands to Send — is
// inspected before it is written. A status-"ok" open_ack is refused unless the
// injected OK-ack guard approves it; nothing reaches the wire on refusal.
func (c *Client) Send(ctx context.Context, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	if err := c.checkOpenAckSeal(data, msg); err != nil {
		return err
	}
	conn := c.connSnapshot()
	if conn == nil {
		return errors.New("send: signaling connection not established")
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

// checkOpenAckSeal is the fail-closed transport gate for status-"ok"
// open_acks. It classifies the marshalled message; anything that is not an OK
// open_ack passes through untouched. An OK open_ack must be approved by the
// installed guard, and the guard receives the validation context only when the
// caller passed a typed OpenAck (directly or wrapped in openAckWire). A raw
// map/struct that merely looks like an OK open_ack carries no context and is
// therefore rejected — the case a static shape check cannot see.
func (c *Client) checkOpenAckSeal(data []byte, original any) error {
	var probe struct {
		Type   string `json:"type"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil // not a JSON object Send can classify; leave it to the consumer
	}
	if probe.Type != "open_ack" || probe.Status != "ok" {
		return nil
	}
	var validation OpenAck
	switch v := original.(type) {
	case openAckWire:
		validation = v.OpenAck
	case OpenAck:
		validation = v
	default:
		// A raw message that merely encodes an OK open_ack: no validation
		// context can be recovered, so the guard sees none and rejects it.
		validation = OpenAck{Status: probe.Status}
	}
	guard := c.openAckGuardSnapshot()
	if guard == nil {
		return errors.New(`signaling: refusing to send a status "ok" open_ack: no OK-ack guard installed`)
	}
	if err := guard(validation); err != nil {
		return fmt.Errorf(`signaling: refusing to send a status "ok" open_ack: %w`, err)
	}
	return nil
}

// Listen reads messages in a loop and dispatches them.
// It is the sole reader of the WebSocket connection — RegisterShare and
// other callers must not call conn.Read concurrently.
func (c *Client) Listen(ctx context.Context) error {
	// Snapshot the connection for this listen epoch: the reader and the
	// keepalive pings must both target the socket this call was started for,
	// even if a concurrent Connect swaps c.conn.
	conn := c.connSnapshot()
	if conn == nil {
		return errors.New("listen: signaling connection not established")
	}

	// Keepalive ping: sends every 30s to prevent Cloudflare's 100s WebSocket timeout
	// from disconnecting idle agents. Any WebSocket frame resets the timeout.
	keepaliveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := conn.Ping(keepaliveCtx); err != nil {
					return // Connection closed or error
				}
			case <-keepaliveCtx.Done():
				return
			}
		}
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		msg.Raw = data // strict re-parse source for relay_config (§11.1)

		// Route registration responses to RegisterShare if one is in flight.
		if msg.Type == "share_registered" || msg.Type == "error" {
			c.pendingRegMu.Lock()
			ch := c.pendingReg
			c.pendingRegMu.Unlock()
			if ch != nil {
				ch <- msg
				continue
			}
		}

		if c.OnMessage != nil {
			c.OnMessage(msg)
		}
	}
}

// SetOnMessage sets the message handler callback.
func (c *Client) SetOnMessage(handler func(Message)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.OnMessage = handler
}
