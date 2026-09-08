package handler

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
	"sharebridge/control/internal/config"
	"sharebridge/control/internal/directctl"
	"sharebridge/control/internal/hub"
	"sharebridge/control/internal/middleware"
	"sharebridge/control/internal/relayctl"
)

// flexInt decodes a JSON number or a numeric string. The agent's hello message
// sends `"version":"1.0"` as a string while open_signal.version is a JSON
// number, so a plain int field would reject the hello and drop the message.
type flexInt int

func (f *flexInt) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if s == "" {
			*f = 0
			return nil
		}
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		*f = flexInt(n)
		return nil
	}
	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*f = flexInt(n)
	return nil
}

// Agent message types from agent to server
type agentMsg struct {
	Type                string     `json:"type"`
	AgentID             string     `json:"agent_id,omitempty"`
	Code                string     `json:"code,omitempty"`
	ShareURL            string     `json:"share_url,omitempty"` // received for protocol compat, not stored
	ExpiresAt           *time.Time `json:"expires_at,omitempty"`
	RelayOnly           *bool      `json:"relay_only,omitempty"`
	RelayStaticPub      string     `json:"relay_static_pub,omitempty"`
	ShareType           string     `json:"share_type,omitempty"`
	IsPasswordProtected bool       `json:"is_password_protected,omitempty"`

	// Direct-mode control-plane fields. Json tags mirror the agent's
	// signaling.Message so a single struct decodes both legacy and control
	// message shapes.
	CSRPEM         string  `json:"csr_pem,omitempty"`
	Fingerprint    string  `json:"fingerprint,omitempty"`
	NotAfter       string  `json:"not_after,omitempty"`
	IP             string  `json:"ip,omitempty"`
	Port           int     `json:"port,omitempty"`
	Status         string  `json:"status,omitempty"`
	Nonce          string  `json:"nonce,omitempty"`
	Seq            uint64  `json:"seq,omitempty"`
	ShareID        string  `json:"share_id,omitempty"`
	Route          string  `json:"route,omitempty"`
	LeaseSeconds   int     `json:"lease_seconds,omitempty"`
	Version        flexInt `json:"version,omitempty"`
	GrantedPort    int     `json:"granted_port,omitempty"`
	PublicIP       string  `json:"public_ip,omitempty"`
	WasAlreadyOpen bool    `json:"was_already_open,omitempty"`
	Error          string  `json:"error,omitempty"`
	Reason         string  `json:"reason,omitempty"`

	// Relay telemetry fields (§11.1). Additive with omitempty so existing
	// direct-message JSON shapes are unchanged; both messages are inbound-only
	// and treated as telemetry.
	Generation int  `json:"generation,omitempty"` // relay_client_state / lockdown_status
	Locked     bool `json:"locked,omitempty"`     // lockdown_status

	// STUN result fields (§11.1 stun_result, plan Task 18): the challenge
	// ID echo (ID only — never the packed secret), the STUN transaction ID
	// and the lowercase-hex integrity-protected receipt, which control
	// hex-decodes before TakeObservation. Inbound-only; sizes are validated
	// in directctl before any state is touched.
	Challenge     string `json:"challenge,omitempty"`
	TransactionID string `json:"transaction_id,omitempty"`
	Receipt       string `json:"receipt,omitempty"`
}

var generatedCodeRegex = regexp.MustCompile(`^[a-z0-9]{8}$`)
var externalCodeRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,128}$`)

var errCodeAlreadyInUse = errors.New("code already in use")

// RoutePublisher distributes relay route lifecycle changes to the §11.3
// gateway sync (plan Task 12; implemented by *relayctl.Publisher). It is an
// optional AgentWS dependency: deployments without relay publishing pass no
// implementation and behave exactly as before. Implementations derive every
// field from PocketBase rows keyed by the session record ID — never from
// agent or HTTP parameters (§11.1).
type RoutePublisher interface {
	// PublishAdd publishes the route_add delta after a share registration
	// has committed.
	PublishAdd(sessionRecordID string) error
	// PublishRevoke publishes the route_revoke delta before (or together
	// with) the session's local lifecycle removal.
	PublishRevoke(sessionRecordID string) error
}

// AgentWS handles WebSocket connections from agents.
// It expects the api_key_id to be set in the request context by APIKeyAuth middleware.
// The controller parameter is optional - if nil, origin allocation is disabled.
// routePublishers is an optional variadic dependency: when supplied, its
// first element receives route add/revoke publications alongside share
// lifecycle transitions (plan Task 12).
func AgentWS(app core.App, h *hub.Hub, cfg *config.Config, ctrl *directctl.Controller, routePublishers ...RoutePublisher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Extract API key ID and account ID from context (set by APIKeyAuth middleware)
		apiKeyID := middleware.GetAPIKeyID(r.Context())
		accountID := middleware.GetAccountID(r.Context())
		if apiKeyID == "" {
			http.Error(w, `{"error":"missing api_key"}`, http.StatusUnauthorized)
			return
		}
		var routes RoutePublisher
		if len(routePublishers) > 0 {
			routes = routePublishers[0]
		}

		// Upgrade to WebSocket
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			log.Printf("agent_ws accept: %v", err)
			return
		}
		defer conn.CloseNow()

		// coder/websocket recommends avoiding request.Context() for upgraded
		// WebSocket lifetime.
		ctx := context.Background()
		var agentID string

		// Main message loop
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				if agentID != "" {
					log.Printf("agent disconnected: %s (agent_id: %s)", apiKeyID, agentID)
					h.UnregisterAgent(apiKeyID, conn)
				}
				if ctrl != nil {
					ctrl.AgentDisconnected(apiKeyID, conn)
				}
				return
			}

			var msg agentMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}

			switch msg.Type {
			case "hello":
				if msg.AgentID == "" {
					// Reject an empty hello: emit an error and do NOT enroll the
					// agent (the epoch identity must never be empty).
					handleHello(ctx, conn, h, apiKeyID, accountID, "", cfg)
					continue
				}
				handleHello(ctx, conn, h, apiKeyID, accountID, msg.AgentID, cfg)
				if ctrl != nil {
					ctrl.HandleHello(ctx, conn, apiKeyID, accountID, msg.AgentID)
				}
				agentID = msg.AgentID

			case "csr_submit":
				if agentID == "" {
					hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "hello required before csr_submit"})
					continue
				}
				if ctrl == nil {
					continue
				}
				ctrl.HandleCSRSubmit(ctx, conn, apiKeyID, msg.CSRPEM)

			case "tls_ready":
				if agentID == "" {
					hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "hello required before tls_ready"})
					continue
				}
				if ctrl == nil {
					continue
				}
				ctrl.HandleTLSReady(ctx, conn, apiKeyID, msg.Fingerprint, msg.NotAfter)

			case "tls_error":
				if agentID == "" {
					continue
				}
				if ctrl == nil {
					continue
				}
				ctrl.HandleTLSError(ctx, apiKeyID, msg.Reason)

			case "report_endpoint":
				if agentID == "" {
					hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "hello required before report_endpoint"})
					continue
				}
				if ctrl == nil {
					continue
				}
				ctrl.HandleReportEndpoint(ctx, conn, apiKeyID, msg.IP, msg.Port, msg.Status)

			case "open_ack":
				if agentID == "" {
					hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "hello required before open_ack"})
					continue
				}
				if ctrl == nil {
					continue
				}
				if msg.Status != "ok" && msg.Status != "error" {
					continue
				}
				if msg.Nonce == "" || msg.Seq == 0 {
					continue
				}
				ctrl.HandleOpenAck(conn, apiKeyID, directctl.OpenAck{
					ShareID:        msg.ShareID,
					Nonce:          msg.Nonce,
					Seq:            msg.Seq,
					GrantedPort:    msg.GrantedPort,
					PublicIP:       msg.PublicIP,
					WasAlreadyOpen: msg.WasAlreadyOpen,
					Status:         msg.Status,
					Error:          msg.Error,
				})

			case "register_share":
				if agentID == "" {
					hub.SendDirect(ctx, conn, map[string]string{
						"type":    "error",
						"message": "hello required before register_share",
					})
					continue
				}
				handleRegisterShare(ctx, conn, h, app, apiKeyID, accountID, agentID, msg, ctrl, routes)

			case "unregister_share":
				if agentID == "" {
					continue
				}
				handleUnregisterShare(ctx, conn, h, app, apiKeyID, msg.Code, routes)

			case "deregister":
				// Agent-initiated lifecycle transition (RevokeSession or
				// pruneExpiredSessions). Marks the control-side row inactive and
				// writes the discriminator (§10).
				if agentID == "" {
					continue
				}
				handleDeregister(ctx, conn, h, app, apiKeyID, msg.Code, msg.Reason, routes)

			case "relay_client_state":
				// §11.1 telemetry: bounded, current-epoch only, never availability.
				if agentID == "" {
					hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "hello required before relay_client_state"})
					continue
				}
				if ctrl == nil || !ctrl.IsCurrentEpoch(apiKeyID, conn) {
					continue // stale socket: drop
				}
				if err := relayctl.ValidateRelayClientState(msg.Generation, msg.Status, msg.Reason); err != nil {
					log.Printf("relay_client_state rejected (api_key_id=%s): %v", apiKeyID, err)
					continue
				}
				// Telemetry only (§7.4): relay availability comes exclusively from
				// the gateway presence view (Task 15); nothing here mutates share
				// lifecycle. Diagnostics persistence lands with that task.

			case "lockdown_status":
				// §11.1 advisory fast-path telemetry: may suppress direct attempts
				// agent-side, but can never establish route availability or change
				// share lifecycle.
				if agentID == "" {
					hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "hello required before lockdown_status"})
					continue
				}
				if ctrl == nil || !ctrl.IsCurrentEpoch(apiKeyID, conn) {
					continue // stale socket: drop
				}
				if err := relayctl.ValidateLockdownStatus(msg.Generation, msg.Locked); err != nil {
					log.Printf("lockdown_status rejected (api_key_id=%s): %v", apiKeyID, err)
					continue
				}
				// Advisory only: deliberately no control-side state change (§11.1).

			case "stun_result":
				// §11.1 observation echo: accepted only from the current
				// API-key socket after hello; all validation (sizes, epoch,
				// challenge/transaction/receipt match, single use, expiry)
				// happens in the controller's claim path and never touches
				// relay availability (§10.3).
				if agentID == "" {
					hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "hello required before stun_result"})
					continue
				}
				if ctrl == nil || !ctrl.IsCurrentEpoch(apiKeyID, conn) {
					continue // stale socket: drop
				}
				ctrl.HandleSTUNResult(conn, apiKeyID, msg.Challenge, msg.TransactionID, msg.Receipt)

			case "relay_credential_request":
				// §11.1 (Task 6 amendment): the tunnel manager's bounded,
				// rate-limited request for a fresh relay_config after the
				// one-use credential is burned or nearing expiry. Same guards
				// as stun_result: authenticated session post-hello, current
				// epoch only — a fenced socket never receives credentials.
				if agentID == "" {
					hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "hello required before relay_credential_request"})
					continue
				}
				if ctrl == nil || !ctrl.IsCurrentEpoch(apiKeyID, conn) {
					continue // stale socket: drop
				}
				if err := relayctl.ValidateRelayCredentialRequest(msg.Reason); err != nil {
					log.Printf("relay_credential_request rejected (api_key_id=%s): %v", apiKeyID, err)
					continue
				}
				if !allowRelayCredentialRequest(apiKeyID) {
					// §16.4: rate limits bound credential requests. No reply:
					// the agent's bounded wait re-requests.
					log.Printf("relay_credential_request rate limited (api_key_id=%s)", apiKeyID)
					continue
				}
				handleRelayCredentialRequest(ctx, conn, app, apiKeyID, cfg)
			}
		}
	}
}

// §16.4 rate-limit bounds for relay_credential_request, following the Task 16
// per-agent token-bucket pattern (control/internal/stun): a bucket of N tokens
// that refills one token per interval, per agent, with a bounded number of
// tracked agents and lazy idle reclaim. A malformed or broken agent therefore
// cannot churn credential issuance beyond a bounded sustained rate, and limiter
// state cannot grow without bound.
const (
	// relayCredentialRequestsPerMinute is the bucket capacity AND the
	// sustained grant rate (one token per refill interval): four immediate
	// requests, then four per minute. Legitimate recovery (§15.1/§15.2:
	// frps restart, agent restart, expiry refresh) needs far less; the STUN
	// challenge bucket uses the same 4/minute bound.
	relayCredentialRequestsPerMinute = 4
	// relayCredentialRefillInterval = 60s / relayCredentialRequestsPerMinute.
	relayCredentialRefillInterval = 15 * time.Second
	// maxRelayCredentialAgents bounds tracked buckets (matches the STUN
	// listener's per-agent state cap). Beyond the cap, idle buckets are
	// reclaimed; if none are idle the request fails closed.
	maxRelayCredentialAgents = 4096
	// relayCredentialBucketIdleTTL reclaims buckets of agents that have not
	// requested within the window (same idle horizon as STUN agent state).
	relayCredentialBucketIdleTTL = 10 * time.Minute
)

// relayCredentialBucket is one agent's token bucket. Guarded by
// relayCredentialLimiter.mu.
type relayCredentialBucket struct {
	tokens     float64
	lastRefill time.Time
}

// relayCredentialLimiter is the process-wide per-agent limiter. It is keyed by
// API key (the agent identity) so a reconnecting socket cannot reset its own
// budget: the epoch fencing already drops stale sockets, and this bound
// additionally limits churn across the current epoch.
var relayCredentialLimiter = struct {
	sync.Mutex
	buckets map[string]*relayCredentialBucket
}{buckets: make(map[string]*relayCredentialBucket)}

// allowRelayCredentialRequest reports whether the agent may be served now,
// consuming one token when it may.
func allowRelayCredentialRequest(apiKeyID string) bool {
	return allowRelayCredentialRequestAt(apiKeyID, time.Now())
}

// allowRelayCredentialRequestAt is allowRelayCredentialRequest with an
// injectable clock for tests.
func allowRelayCredentialRequestAt(apiKeyID string, now time.Time) bool {
	relayCredentialLimiter.Lock()
	defer relayCredentialLimiter.Unlock()

	bucket, ok := relayCredentialLimiter.buckets[apiKeyID]
	if !ok {
		if len(relayCredentialLimiter.buckets) >= maxRelayCredentialAgents {
			relayCredentialReclaimIdleLocked(now)
		}
		if len(relayCredentialLimiter.buckets) >= maxRelayCredentialAgents {
			return false // bounded state: fail closed rather than grow
		}
		bucket = &relayCredentialBucket{tokens: relayCredentialRequestsPerMinute, lastRefill: now}
		relayCredentialLimiter.buckets[apiKeyID] = bucket
	}

	// Refill continuously: one token's worth of fraction per elapsed
	// interval, capped at the capacity. The fraction carries across polls
	// because lastRefill always advances to now.
	elapsed := now.Sub(bucket.lastRefill)
	if elapsed > 0 {
		bucket.tokens += elapsed.Seconds() / relayCredentialRefillInterval.Seconds()
		if bucket.tokens > relayCredentialRequestsPerMinute {
			bucket.tokens = relayCredentialRequestsPerMinute
		}
		bucket.lastRefill = now
	}
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

// relayCredentialReclaimIdleLocked drops buckets idle past the TTL. Caller
// holds relayCredentialLimiter.mu.
func relayCredentialReclaimIdleLocked(now time.Time) {
	for agent, bucket := range relayCredentialLimiter.buckets {
		if now.Sub(bucket.lastRefill) >= relayCredentialBucketIdleTTL {
			delete(relayCredentialLimiter.buckets, agent)
		}
	}
}

// handleRelayCredentialRequest serves an already-guarded, already-validated,
// already-rate-checked §11.1 relay_credential_request: it issues a freshly
// signed relay_config for the agent's CURRENT assignment/generation via the
// existing §7.2 signer (a fresh random one-use jti per issue, issued_at and
// expires_at derived from a single clock reading) and sends it on the current
// epoch's connection. Relay-unconfigured deployments and missing assignments
// fail closed: the request is dropped with a diagnostic and credential
// material is never logged (§7.2/§16.6).
func handleRelayCredentialRequest(ctx context.Context, conn *websocket.Conn, app core.App, apiKeyID string, cfg *config.Config) {
	if !cfg.RelayPolicyEnabled() {
		log.Printf("relay_credential_request dropped: relay policy unconfigured (api_key_id=%s)", apiKeyID)
		return
	}
	settings := relayctl.Settings{
		GatewayAddr: cfg.RelayGatewayHost,
		GatewayPort: cfg.RelayGatewayPort,
		PortRange:   relayctl.PortRange{Min: cfg.RelayPortMin, Max: cfg.RelayPortMax},
	}
	if err := settings.Validate(); err != nil {
		log.Printf("relay_credential_request dropped: invalid relay policy (api_key_id=%s): %v", apiKeyID, err)
		return
	}
	signer, err := relayctl.NewSignerFromHexSeed(cfg.RelayAuthKeySeed)
	if err != nil {
		log.Printf("relay_credential_request dropped: relay auth seed unusable (api_key_id=%s)", apiKeyID)
		return
	}

	// The agent row exists (hello enrolled it); look it up read-only — a
	// credential request must never create enrollment state.
	agents, err := app.FindRecordsByFilter("agents", "api_key_id = {:k}", "", 1, 0, map[string]any{"k": apiKeyID})
	if err != nil {
		log.Printf("relay_credential_request dropped: agent lookup failed (api_key_id=%s)", apiKeyID)
		return
	}
	if len(agents) == 0 {
		log.Printf("relay_credential_request dropped: no agent record (api_key_id=%s)", apiKeyID)
		return
	}

	assignment, err := relayctl.EnsureAssignment(app, agents[0], settings.PortRange)
	if err != nil {
		log.Printf("relay assignment failed for %s: %v", apiKeyID, err)
		return
	}
	msg, err := relayctl.BuildRelayConfig(assignment, apiKeyID, settings.GatewayAddr, settings.GatewayPort, signer)
	if err != nil {
		log.Printf("relay credential issue failed for %s: %v", apiKeyID, err)
		return
	}
	if err := hub.SendDirect(ctx, conn, msg); err != nil {
		log.Printf("relay_config send failed for %s: %v", apiKeyID, err)
	}
}

// handleHello processes the hello message and sends welcome response
func handleHello(ctx context.Context, conn *websocket.Conn, h *hub.Hub, apiKeyID string, accountID string, agentID string, cfg *config.Config) {
	if agentID == "" {
		hub.SendDirect(ctx, conn, map[string]string{
			"type":    "error",
			"message": "agent_id required",
		})
		return
	}

	// Register agent with the hub using apiKeyID
	h.RegisterAgent(apiKeyID, conn)
	log.Printf("agent hello received: api_key_id=%s agent_id=%s", apiKeyID, agentID)

	hub.SendDirect(ctx, conn, map[string]any{
		"type": "welcome",
	})
}

// handleRegisterShare processes share registration (new or reconnect). Session
// creation/claim AND origin allocation run in ONE transaction, and the hub is
// only updated after the transaction commits, so an allocation failure cannot
// orphan an active originless session + hub entry (I7).
func handleRegisterShare(
	ctx context.Context,
	conn *websocket.Conn,
	h *hub.Hub,
	app core.App,
	apiKeyID string,
	accountID string,
	agentID string,
	msg agentMsg,
	ctrl *directctl.Controller,
	routes RoutePublisher,
) {
	// Phase 3 serves only direct, unprotected Immich gallery shares. Reject
	// unsupported registrations before any session claim/create or origin
	// allocation so they cannot become live control-plane state.
	if msg.ShareType != "immich" || (msg.RelayOnly != nil && *msg.RelayOnly) || msg.IsPasswordProtected {
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "unsupported share type"})
		return
	}
	if msg.Code == "" {
		// Generated code: create + allocate origin atomically, retrying on a
		// unique-constraint collision (code or origin) with a fresh code.
		var code string
		var session *core.Record
		var origin string
		created := false
		for i := 0; i < 5; i++ {
			var err error
			code, err = generateRandomCode()
			if err != nil {
				log.Printf("failed to generate random code: %v", err)
				hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "failed to generate code"})
				return
			}
			session, origin, err = createSessionAndOrigin(app, code, apiKeyID, agentID, msg, ctrl)
			if err == nil {
				created = true
				break
			}
			if !directctl.IsUniqueViolation(err) {
				log.Printf("db error creating session: %v", err)
				hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "database error"})
				return
			}
		}
		if !created {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "code collision, try again"})
			return
		}
		h.RegisterCode(code, apiKeyID)
		publishRouteAdd(routes, session.Id)
		response := map[string]any{"type": "share_registered", "code": code, "reconnected": false}
		if ctrl != nil {
			response["origin"] = origin
			response["relay_origin"] = controlRelayOrigin(origin)
		}
		if exp := session.GetDateTime("expires_at"); !exp.IsZero() {
			response["expires_at"] = exp.Time().Format(time.RFC3339)
		}
		hub.SendDirect(ctx, conn, response)
		log.Printf("share registered: code=%s api_key_id=%s agent_id=%s reconnected=%v", code, apiKeyID, agentID, false)
		return
	}

	// Custom code: validate, then claim + allocate origin in one transaction.
	if !externalCodeRegex.MatchString(msg.Code) {
		hub.SendDirect(ctx, conn, map[string]string{
			"type":    "error",
			"message": "invalid external code format (8-128 chars, alphanumeric + hyphen + underscore)",
		})
		return
	}
	session, reconnected, origin, err := claimSessionAndOrigin(app, msg.Code, apiKeyID, accountID, agentID, msg, ctrl)
	if err != nil {
		if errors.Is(err, errCodeAlreadyInUse) {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "code already in use"})
			return
		}
		if err.Error() == "session owned by different agent" {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "session owned by different agent"})
			return
		}
		log.Printf("db error checking code availability: %v", err)
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "database error"})
		return
	}
	if session == nil {
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "failed to retrieve session"})
		return
	}

	h.RegisterCode(msg.Code, apiKeyID)

	publishRouteAdd(routes, session.Id)

	response := map[string]any{"type": "share_registered", "code": msg.Code, "reconnected": reconnected}
	if ctrl != nil {
		response["origin"] = origin
		response["relay_origin"] = controlRelayOrigin(origin)
	}
	if exp := session.GetDateTime("expires_at"); !exp.IsZero() {
		response["expires_at"] = exp.Time().Format(time.RFC3339)
	}
	hub.SendDirect(ctx, conn, response)
	log.Printf("share registered: code=%s api_key_id=%s agent_id=%s reconnected=%v", msg.Code, apiKeyID, agentID, reconnected)
}

// controlRelayOrigin derives the §6 relay origin from the persisted direct
// origin. The direct origin is always returned as `origin`; derivation failure
// (never expected for control-allocated origins) yields an empty relay origin
// rather than any agent-influenced value: no agent message may supply either
// origin (§11.1).
func controlRelayOrigin(origin string) string {
	relayOrigin, err := relayctl.RelayOriginFromDirect(origin)
	if err != nil {
		log.Printf("relay origin derivation failed for %q: %v", origin, err)
		return ""
	}
	return relayOrigin
}

// publishRouteAdd forwards a successful registration to the Task 12 route
// publisher (add after successful registration). Publication failure is
// logged and never fails the registration: the share is committed, and the
// publisher's snapshot/lease-refresh reconciles gateway state.
func publishRouteAdd(routes RoutePublisher, sessionRecordID string) {
	if routes == nil {
		return
	}
	if err := routes.PublishAdd(sessionRecordID); err != nil {
		log.Printf("relay route add publish failed for session %s: %v", sessionRecordID, err)
	}
}

// publishRouteRevoke forwards a lifecycle removal to the Task 12 route
// publisher. Callers invoke it BEFORE the local lifecycle removal so the
// gateway stops routing no later than the share disappears control-side.
// The PocketBase rows stay authoritative: if the subsequent save fails, the
// still-active row is simply re-touched by the 30-second lease refresh, so
// the transient state is DB-consistent in both directions; a revoked
// (tombstoned) row can never become routable again.
func publishRouteRevoke(routes RoutePublisher, sessionRecordID string) {
	if routes == nil {
		return
	}
	if err := routes.PublishRevoke(sessionRecordID); err != nil {
		log.Printf("relay route revoke publish failed for session %s: %v", sessionRecordID, err)
	}
}

// createSessionAndOrigin creates a session and (when a controller is present)
// allocates its control-managed origin in a single transaction.
func createSessionAndOrigin(app core.App, code, apiKeyID, agentID string, msg agentMsg, ctrl *directctl.Controller) (*core.Record, string, error) {
	var session *core.Record
	var origin string
	err := app.RunInTransaction(func(txApp core.App) error {
		relayOnly := false
		if msg.RelayOnly != nil {
			relayOnly = *msg.RelayOnly
		}
		if err := createSession(txApp, code, apiKeyID, agentID, msg.ExpiresAt, relayOnly, msg.RelayStaticPub, "", false); err != nil {
			return err
		}
		s, err := getSessionByCode(txApp, code)
		if err != nil {
			return err
		}
		if s == nil {
			return errors.New("failed to retrieve session")
		}
		session = s
		if ctrl != nil {
			o, err := ctrl.AllocateOriginForTx(txApp, apiKeyID, session)
			if err != nil {
				return err
			}
			origin = o
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return session, origin, nil
}

// claimSessionAndOrigin claims (or creates) a custom-coded session and
// allocates its origin in a single transaction.
func claimSessionAndOrigin(app core.App, code, apiKeyID, accountID, agentID string, msg agentMsg, ctrl *directctl.Controller) (*core.Record, bool, string, error) {
	var session *core.Record
	var reconnected bool
	var origin string
	err := app.RunInTransaction(func(txApp core.App) error {
		s, rec, err := claimSessionCodeTx(txApp, code, apiKeyID, accountID, agentID, msg.ExpiresAt, msg.RelayOnly, msg.RelayStaticPub, msg.ShareType, msg.IsPasswordProtected)
		if err != nil {
			return err
		}
		if s == nil {
			return errors.New("failed to retrieve session")
		}
		session, reconnected = s, rec
		if ctrl != nil {
			o, err := ctrl.AllocateOriginForTx(txApp, apiKeyID, session)
			if err != nil {
				return err
			}
			origin = o
		}
		return nil
	})
	return session, reconnected, origin, err
}

func handleUnregisterShare(ctx context.Context, conn *websocket.Conn, h *hub.Hub, app core.App, apiKeyID, code string, routes RoutePublisher) {
	if code == "" {
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "code required"})
		return
	}
	if !externalCodeRegex.MatchString(code) {
		hub.SendDirect(ctx, conn, map[string]string{
			"type":    "error",
			"message": "invalid external code format (8-128 chars, alphanumeric + hyphen + underscore)",
		})
		return
	}
	session, err := getSessionByCode(app, code)
	if err != nil {
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "database error"})
		return
	}
	if session == nil {
		hub.SendDirect(ctx, conn, map[string]string{"type": "share_unregistered", "code": code})
		return
	}
	if session.GetString("api_key_id") != apiKeyID {
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "session not owned by this api key"})
		return
	}
	publishRouteRevoke(routes, session.Id)
	session.Set("is_active", false)
	session.Set("inactive_reason", "revoked")
	if err := app.Save(session); err != nil {
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "database error"})
		return
	}
	h.UnregisterCode(ctx, code, apiKeyID, "share has been removed")
	hub.SendDirect(ctx, conn, map[string]string{"type": "share_unregistered", "code": code})
}

// handleDeregister applies an agent-initiated lifecycle transition
// (RevokeSession → "revoked", pruneExpiredSessions → "expired"). Unlike
// unregister_share it is fire-and-forget: the agent does not wait for an ack.
func handleDeregister(ctx context.Context, conn *websocket.Conn, h *hub.Hub, app core.App, apiKeyID, code, reason string, routes RoutePublisher) {
	if code == "" || !externalCodeRegex.MatchString(code) {
		return
	}
	session, err := getSessionByCode(app, code)
	if err != nil || session == nil {
		return
	}
	if session.GetString("api_key_id") != apiKeyID {
		return
	}
	inactiveReason := reason
	if inactiveReason != "expired" && inactiveReason != "revoked" && inactiveReason != "unsupported" {
		inactiveReason = "revoked"
	}
	publishRouteRevoke(routes, session.Id)
	session.Set("is_active", false)
	session.Set("inactive_reason", inactiveReason)
	if err := app.Save(session); err != nil {
		log.Printf("deregister: save session %s: %v", code, err)
		return
	}
	h.UnregisterCode(ctx, code, apiKeyID, "share deregistered")
}

// createSession creates a new session record in PocketBase.
// share_url and max_downloads are intentionally not stored — the server is untrusted.
func createSession(app core.App, code, apiKeyID, agentID string, expiresAt *time.Time, relayOnly bool, relayStaticPub, shareType string, isPasswordProtected bool) error {
	col, err := app.FindCollectionByNameOrId("sessions")
	if err != nil {
		return err
	}

	record := core.NewRecord(col)
	record.Set("code", code)
	record.Set("api_key_id", apiKeyID)
	record.Set("agent_id", agentID)
	record.Set("relay_only", relayOnly)
	record.Set("relay_static_pub", relayStaticPub)
	record.Set("share_type", shareType)
	record.Set("is_password_protected", isPasswordProtected)
	record.Set("is_active", true)

	if expiresAt != nil {
		dt, _ := types.ParseDateTime(*expiresAt)
		record.Set("expires_at", dt)
	}

	return app.Save(record)
}

// getSessionByCode retrieves a session by its code
func getSessionByCode(app core.App, code string) (*core.Record, error) {
	records, err := app.FindRecordsByFilter(
		"sessions",
		"code = {:code} && is_active = true",
		"",
		1,
		0,
		map[string]any{"code": code},
	)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}
	return records[0], nil
}

// claimSessionCodeTx creates or reassigns a custom code within an already-open
// transaction (txApp). It is the transactional body of the former
// claimSessionCode; the caller owns the transaction so origin allocation can
// commit atomically with the claim (I7). Soft-deleted (inactive) sessions are
// still reclaimed — the sessions.code unique index means a fresh row for the
// same code is impossible — but AllocateOriginForTx then refreshes a stale
// origin whose namespace no longer matches the agent's.
func claimSessionCodeTx(txApp core.App, code, apiKeyID, accountID, agentID string, expiresAt *time.Time, relayOnly *bool, relayStaticPub, shareType string, isPasswordProtected bool) (*core.Record, bool, error) {
	var existing struct {
		SessionID string `db:"session_id"`
		AccountID string `db:"account_id"`
		AgentID   string `db:"agent_id"`
	}

	err := txApp.DB().
		NewQuery(`SELECT s.id AS session_id, ak.account_id, s.agent_id
			FROM sessions s
			JOIN api_keys ak ON ak.id = s.api_key_id
			WHERE s.code = {:code}
			LIMIT 1`).
		Bind(dbx.Params{"code": code}).
		One(&existing)

	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, false, err
		}
		createRelayOnly := false
		if relayOnly != nil {
			createRelayOnly = *relayOnly
		}
		if err := createSession(txApp, code, apiKeyID, agentID, expiresAt, createRelayOnly, relayStaticPub, shareType, isPasswordProtected); err != nil {
			return nil, false, err
		}
		record, getErr := getSessionByCode(txApp, code)
		if getErr != nil {
			return nil, false, getErr
		}
		return record, false, nil
	}

	if existing.AccountID != accountID {
		return nil, false, errCodeAlreadyInUse
	}
	if existing.AgentID != "" && existing.AgentID != agentID {
		return nil, false, errors.New("session owned by different agent")
	}

	record, err := txApp.FindRecordById("sessions", existing.SessionID)
	if err != nil {
		return nil, false, err
	}
	record.Set("api_key_id", apiKeyID)
	record.Set("agent_id", agentID)
	if relayOnly != nil {
		record.Set("relay_only", *relayOnly)
	}
	record.Set("relay_static_pub", relayStaticPub)
	record.Set("share_type", shareType)
	record.Set("is_password_protected", isPasswordProtected)
	record.Set("is_active", true)
	record.Set("inactive_reason", "")
	if expiresAt != nil {
		dt, _ := types.ParseDateTime(*expiresAt)
		record.Set("expires_at", dt)
	}
	if err := txApp.Save(record); err != nil {
		return nil, false, err
	}
	return record, true, nil
}

// generateRandomCode generates a random 8-character code
func generateRandomCode() (string, error) {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	const length = 8

	result := make([]byte, length)
	for i := range result {
		randIdx, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", err
		}
		result[i] = charset[randIdx.Int64()]
	}
	return string(result), nil
}
