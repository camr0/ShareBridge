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
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
	"sharebridge/control/internal/config"
	"sharebridge/control/internal/directctl"
	"sharebridge/control/internal/hub"
	"sharebridge/control/internal/middleware"
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
}

var generatedCodeRegex = regexp.MustCompile(`^[a-z0-9]{8}$`)
var externalCodeRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,128}$`)

var errCodeAlreadyInUse = errors.New("code already in use")

// AgentWS handles WebSocket connections from agents.
// It expects the api_key_id to be set in the request context by APIKeyAuth middleware.
// The controller parameter is optional - if nil, origin allocation is disabled.
func AgentWS(app core.App, h *hub.Hub, cfg *config.Config, ctrl *directctl.Controller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Extract API key ID and account ID from context (set by APIKeyAuth middleware)
		apiKeyID := middleware.GetAPIKeyID(r.Context())
		accountID := middleware.GetAccountID(r.Context())
		if apiKeyID == "" {
			http.Error(w, `{"error":"missing api_key"}`, http.StatusUnauthorized)
			return
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
				handleRegisterShare(ctx, conn, h, app, apiKeyID, accountID, agentID, msg, ctrl)

			case "unregister_share":
				if agentID == "" {
					continue
				}
				handleUnregisterShare(ctx, conn, h, app, apiKeyID, msg.Code)

			case "deregister":
				// Agent-initiated lifecycle transition (RevokeSession or
				// pruneExpiredSessions). Marks the control-side row inactive and
				// writes the discriminator (§10).
				if agentID == "" {
					continue
				}
				handleDeregister(ctx, conn, h, app, apiKeyID, msg.Code, msg.Reason)
			}
		}
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
		response := map[string]any{"type": "share_registered", "code": code, "reconnected": false}
		if ctrl != nil {
			response["origin"] = origin
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

	response := map[string]any{"type": "share_registered", "code": msg.Code, "reconnected": reconnected}
	if ctrl != nil {
		response["origin"] = origin
	}
	if exp := session.GetDateTime("expires_at"); !exp.IsZero() {
		response["expires_at"] = exp.Time().Format(time.RFC3339)
	}
	hub.SendDirect(ctx, conn, response)
	log.Printf("share registered: code=%s api_key_id=%s agent_id=%s reconnected=%v", msg.Code, apiKeyID, agentID, reconnected)
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

func handleUnregisterShare(ctx context.Context, conn *websocket.Conn, h *hub.Hub, app core.App, apiKeyID, code string) {
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
func handleDeregister(ctx context.Context, conn *websocket.Conn, h *hub.Hub, app core.App, apiKeyID, code, reason string) {
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
