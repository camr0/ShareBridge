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
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/middleware"
	"sharebridge/server/internal/relay"
	"sharebridge/server/internal/turn"
)

// Agent message types from agent to server
type agentMsg struct {
	Type          string          `json:"type"`
	AgentID       string          `json:"agent_id,omitempty"`
	Code          string          `json:"code,omitempty"`
	ShareURL      string          `json:"share_url,omitempty"` // received for protocol compat, not stored
	ExpiresAt     *time.Time      `json:"expires_at,omitempty"`
	SessionID     string          `json:"session_id,omitempty"`
	SDP           string          `json:"sdp,omitempty"`
	Candidate     json.RawMessage `json:"candidate,omitempty"`
	ConnID        string          `json:"conn_id,omitempty"`
	Value         string          `json:"value,omitempty"`
	HasPassword   bool            `json:"has_password,omitempty"`
	RelayOnly     bool            `json:"relay_only,omitempty"`
	RelayStaticPub string         `json:"relay_static_pub,omitempty"`
}

// codeRegex matches valid share codes: 8-30 chars, alphanumeric + hyphen + underscore
var codeRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,30}$`)

var errCodeAlreadyInUse = errors.New("code already in use")

// AgentWS handles WebSocket connections from agents.
// It expects the api_key_id to be set in the request context by APIKeyAuth middleware.
// The registry parameter is optional - if nil, relay functionality is disabled.
func AgentWS(app core.App, h *hub.Hub, reg *relay.Registry, cfg *config.Config) http.HandlerFunc {
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

		// Cached quota state - refreshed at most once per minute to avoid
		// a DB lookup on every ICE candidate while staying current after quota resets.
		var quotaExceeded bool
		var quotaCheckedAt time.Time

		refreshQuota := func() {
			if time.Since(quotaCheckedAt) < time.Minute {
				return
			}
			accountRecord, err := app.FindRecordById("users", accountID)
			if err != nil {
				log.Printf("agent_ws: quota refresh for account %s: %v", accountID, err)
				return
			}
			quotaExceeded, _ = checkRelayQuota(accountRecord)
			quotaCheckedAt = time.Now()
		}

		// Main message loop
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				if agentID != "" {
					log.Printf("agent disconnected: %s (agent_id: %s)", apiKeyID, agentID)
					h.UnregisterAgent(apiKeyID)
				}
				return
			}

			var msg agentMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}

			switch msg.Type {
			case "hello":
				handleHello(ctx, conn, h, apiKeyID, accountID, msg.AgentID, cfg)
				agentID = msg.AgentID

			case "register_share":
				if agentID == "" {
					hub.SendDirect(ctx, conn, map[string]string{
						"type":    "error",
						"message": "hello required before register_share",
					})
					continue
				}
				handleRegisterShare(ctx, conn, h, app, apiKeyID, accountID, agentID, msg)

			case "offer":
				if agentID == "" {
					continue
				}
				h.ForwardToBrowser(ctx, msg.SessionID, map[string]any{
					"type": "offer",
					"sdp":  msg.SDP,
				})

			case "ice_candidate":
				if agentID == "" {
					continue
				}
				if cfg.HasTurn() {
					refreshQuota()
				}
				if quotaExceeded && isRelayCandidate(msg.Candidate) {
					log.Printf("agent_ws: dropping relay candidate for over-quota account %s", accountID)
					continue
				}
				h.ForwardToBrowser(ctx, msg.SessionID, map[string]any{
					"type":      "ice_candidate",
					"candidate": msg.Candidate,
				})

				case "nonce":
					// Route nonce from agent to the specific browser identified by connID.
					if agentID == "" {
						continue
					}
					log.Printf("agent_ws: forwarding nonce to browser conn_id=%s has_password=%v", msg.ConnID, msg.HasPassword)
					h.ForwardToBrowserByConnID(ctx, msg.ConnID, map[string]any{
						"type":         "nonce",
						"conn_id":      msg.ConnID,
					"value":        msg.Value,
					"has_password": msg.HasPassword,
				})

			case "auth_ok":
				// Browser authentication succeeded - send relay_policy so browser can proceed.
				if agentID == "" {
					continue
				}

				session, err := getSessionByCode(app, msg.Code)
				if err != nil {
					log.Printf("agent_ws: auth_ok session lookup failed for code %s: %v", msg.Code, err)
					continue
				}
				if session == nil {
					log.Printf("agent_ws: auth_ok session not found for code %s", msg.Code)
					continue
				}

				relayOnly := session.GetBool("relay_only")
				expectedStaticPub := session.GetString("relay_static_pub")

				// Determine relay availability: requires relay to be configured, non-relay-only session,
				// and either explicit static pub from session or configured relay JWT secret.
				var relayAllowed bool
				if reg != nil && cfg.RelayJWTSecret != "" && !relayOnly && expectedStaticPub != "" {
					accountRecord, err := app.FindRecordById("users", accountID)
					if err != nil {
						log.Printf("agent_ws: auth_ok account lookup failed for account %s: %v", accountID, err)
						continue
					}
					quotaExceeded, _ := checkRelayQuota(accountRecord)
					relayAllowed = !quotaExceeded
				}

				var browserJWT string
				if relayAllowed {
					sid := relay.NewSID()
					now := time.Now().UTC()

					browserClaims := relay.BrowserPolicyClaims{
						SID:               sid,
						SessionCode:       msg.Code,
						RelayAllowed:      true,
						RelayOnly:         false,
						ExpectedStaticPub: expectedStaticPub,
						RegisteredClaims:  jwt.RegisteredClaims{ID: relay.NewJTI()},
					}
					var err error
					browserJWT, err = relay.SignBrowserPolicyJWT(cfg.RelayJWTSecret, browserClaims, now)
					if err != nil {
						log.Printf("agent_ws: failed to sign browser policy JWT: %v", err)
						continue
					}

					agentJWT, err := relay.SignAgentRelayJWT(cfg.RelayJWTSecret, relay.AgentRelayClaims{SID: sid, AgentID: agentID}, now)
					if err != nil {
						log.Printf("agent_ws: failed to sign agent relay JWT: %v", err)
						continue
					}

					err = reg.CreatePendingSession(relay.PendingSession{
						SID:               sid,
						AccountID:         accountID,
						SessionCode:       msg.Code,
						AgentID:           agentID,
						RelayAllowed:      true,
						RelayOnly:         false,
						ExpectedStaticPub: expectedStaticPub,
						JTI:               browserClaims.RegisteredClaims.ID,
						ExpiresAt:         now.Add(relay.TokenLifetime),
					}, now)
					if err != nil {
						log.Printf("agent_ws: failed to create pending relay session: %v", err)
						continue
					}

					// Send relay_prepare to agent only when relay fallback is actually allowed.
					hub.SendDirect(ctx, conn, map[string]any{
						"type":       "relay_prepare",
						"sid":        sid,
						"code":       msg.Code,
						"expires_at": now.Add(relay.TokenLifetime).Format(time.RFC3339),
						"relay_jwt":  agentJWT,
					})

					log.Printf("agent_ws: relay session prepared: sid=%s code=%s agent_id=%s", sid, msg.Code, agentID)
				}

				// Send relay_policy to browser via hub (always, so browser can proceed)
				h.ForwardToBrowserByConnID(ctx, msg.ConnID, map[string]any{
					"type":          "relay_policy",
					"token":         browserJWT,
					"relay_allowed": relayAllowed,
					"relay_only":    relayOnly,
				})

			case "auth_failed":
				// Track failures per connID. After 3, close the browser WebSocket.
				// Browser receives auth_failed with attempts_remaining so it can re-prompt.
				if agentID == "" {
					continue
				}
				failures := h.IncrementAuthFailure(msg.ConnID)
				log.Printf("HMAC auth failed: conn %s failure %d/3", msg.ConnID, failures)
				if failures >= 3 {
					h.CloseBrowserConnWithError(ctx, msg.ConnID, "too many incorrect password attempts")
				} else {
					h.ForwardToBrowserByConnID(ctx, msg.ConnID, map[string]any{
						"type":               "auth_failed",
						"attempts_remaining": 3 - failures,
					})
				}

			case "session_expired":
				log.Printf("session expired (max downloads): %s", msg.SessionID)
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

	// Build ICE config for agent - always include TURN credentials.
	// Quota enforcement happens in the ice_candidate forwarding path instead,
	// where relay candidates are stripped when the account is over quota.
	var turnCreds *turn.Credentials
	if cfg.HasTurn() {
		turnExpiry := time.Now().Add(24 * time.Hour)
		creds := turn.GenerateCredentials(cfg.TurnSecret, accountID, turnExpiry)
		turnCreds = &creds
	}

	iceServers := turn.BuildICEConfig(&turn.ICEConfigRequest{
		STUNURL:     cfg.STUNURL,
		TurnURL:     cfg.TurnURL(),
		Credentials: turnCreds,
	})

	hub.SendDirect(ctx, conn, map[string]any{
		"type":        "welcome",
		"ice_servers": iceServers,
	})
}

// isRelayCandidate reports whether a raw ICE candidate JSON is a TURN relay candidate.
// The candidate field is a webrtc.ICECandidateInit object with a "candidate" string.
func isRelayCandidate(raw json.RawMessage) bool {
	var init struct {
		Candidate string `json:"candidate"`
	}
	if err := json.Unmarshal(raw, &init); err != nil {
		return false
	}
	return strings.Contains(init.Candidate, " typ relay")
}

// handleRegisterShare processes share registration (new or reconnect)
func handleRegisterShare(
	ctx context.Context,
	conn *websocket.Conn,
	h *hub.Hub,
	app core.App,
	apiKeyID string,
	accountID string,
	agentID string,
	msg agentMsg,
) {
	// Determine the code to use
	code := msg.Code
	reconnected := false

	if code == "" {
		// Generate a new random code
		var err error
		code, err = generateRandomCode()
		if err != nil {
			log.Printf("failed to generate random code: %v", err)
			hub.SendDirect(ctx, conn, map[string]string{
				"type":    "error",
				"message": "failed to generate code",
			})
			return
		}

		// Try to create with collision retry (5 attempts)
		created := false
		for i := 0; i < 5; i++ {
			err = createSession(app, code, apiKeyID, agentID, msg.ExpiresAt, msg.RelayOnly, msg.RelayStaticPub)
			if err == nil {
				created = true
				break
			}

			// Collision, generate new code
			code, err = generateRandomCode()
			if err != nil {
				log.Printf("failed to generate random code: %v", err)
				hub.SendDirect(ctx, conn, map[string]string{
					"type":    "error",
					"message": "failed to generate code",
				})
				return
			}
		}

		if !created {
			hub.SendDirect(ctx, conn, map[string]string{
				"type":    "error",
				"message": "code collision, try again",
			})
			return
		}
	} else {
		// Validate custom code format
		if !codeRegex.MatchString(code) {
			hub.SendDirect(ctx, conn, map[string]string{
				"type":    "error",
				"message": "invalid code format (8-30 chars, alphanumeric + hyphen + underscore)",
			})
			return
		}

		session, reclaimed, err := claimSessionCode(app, code, apiKeyID, accountID, agentID, msg.ExpiresAt, msg.RelayOnly, msg.RelayStaticPub)
		if err != nil {
			if errors.Is(err, errCodeAlreadyInUse) {
				hub.SendDirect(ctx, conn, map[string]string{
					"type":    "error",
					"message": "code already in use",
				})
				return
			}
			if err.Error() == "session owned by different agent" {
				hub.SendDirect(ctx, conn, map[string]string{
					"type":    "error",
					"message": "session owned by different agent",
				})
				return
			}
			log.Printf("db error checking code availability: %v", err)
			hub.SendDirect(ctx, conn, map[string]string{
				"type":    "error",
				"message": "database error",
			})
			return
		}
		if session == nil {
			hub.SendDirect(ctx, conn, map[string]string{
				"type":    "error",
				"message": "failed to retrieve session",
			})
			return
		}
		reconnected = reclaimed
	}

	// Register code with hub
	h.RegisterCode(code, apiKeyID)

	// Get session for response
	session, err := getSessionByCode(app, code)
	if err != nil || session == nil {
		log.Printf("db error getting session after creation: %v", err)
		hub.SendDirect(ctx, conn, map[string]string{
			"type":    "error",
			"message": "failed to retrieve session",
		})
		return
	}

	// Send success response
	response := map[string]any{
		"type":        "share_registered",
		"code":        code,
		"reconnected": reconnected,
	}
	if expiresAt := session.GetDateTime("expires_at"); !expiresAt.IsZero() {
		response["expires_at"] = expiresAt.Time().Format(time.RFC3339)
	}

	hub.SendDirect(ctx, conn, response)
	log.Printf("share registered: code=%s api_key_id=%s agent_id=%s reconnected=%v", code, apiKeyID, agentID, reconnected)
}

// createSession creates a new session record in PocketBase.
// share_url and max_downloads are intentionally not stored — the server is untrusted.
func createSession(app core.App, code, apiKeyID, agentID string, expiresAt *time.Time, relayOnly bool, relayStaticPub string) error {
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
		"code = {:code}",
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

// claimSessionCode atomically creates or reassigns a custom code.
func claimSessionCode(app core.App, code, apiKeyID, accountID, agentID string, expiresAt *time.Time, relayOnly bool, relayStaticPub string) (*core.Record, bool, error) {
	var claimed *core.Record
	reconnected := false

	err := app.RunInTransaction(func(txApp core.App) error {
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
				return err
			}
			if err := createSession(txApp, code, apiKeyID, agentID, expiresAt, relayOnly, relayStaticPub); err != nil {
				return err
			}
			record, getErr := getSessionByCode(txApp, code)
			if getErr != nil {
				return getErr
			}
			claimed = record
			return nil
		}

		if existing.AccountID != accountID {
			return errCodeAlreadyInUse
		}
		if existing.AgentID != "" && existing.AgentID != agentID {
			return errors.New("session owned by different agent")
		}

		record, err := txApp.FindRecordById("sessions", existing.SessionID)
		if err != nil {
			return err
		}
		record.Set("api_key_id", apiKeyID)
		record.Set("agent_id", agentID)
		record.Set("relay_static_pub", relayStaticPub)
		if expiresAt != nil {
			dt, _ := types.ParseDateTime(*expiresAt)
			record.Set("expires_at", dt)
		}
		if err := txApp.Save(record); err != nil {
			return err
		}

		claimed = record
		reconnected = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return claimed, reconnected, nil
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
