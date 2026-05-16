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
	Type                string          `json:"type"`
	AgentID             string          `json:"agent_id,omitempty"`
	Code                string          `json:"code,omitempty"`
	ShareURL            string          `json:"share_url,omitempty"` // received for protocol compat, not stored
	ExpiresAt           *time.Time      `json:"expires_at,omitempty"`
	SessionID           string          `json:"session_id,omitempty"`
	SDP                 string          `json:"sdp,omitempty"`
	Candidate           json.RawMessage `json:"candidate,omitempty"`
	ConnID              string          `json:"conn_id,omitempty"`
	Value               string          `json:"value,omitempty"`
	Password            string          `json:"password,omitempty"`
	HasPassword         bool            `json:"has_password,omitempty"`
	RelayOnly           *bool           `json:"relay_only,omitempty"`
	RelayStaticPub      string          `json:"relay_static_pub,omitempty"`
	ShareType           string          `json:"share_type,omitempty"`
	IsPasswordProtected bool            `json:"is_password_protected,omitempty"`
}

var generatedCodeRegex = regexp.MustCompile(`^[a-z0-9]{8}$`)
var externalCodeRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,128}$`)

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

			case "unregister_share":
				if agentID == "" {
					continue
				}
				handleUnregisterShare(ctx, conn, h, app, apiKeyID, msg.Code)

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
				// TURN removed - relay candidates now handled by secure relay
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

				// Determine relay availability: requires relay to be configured,
				// a valid static pub key from the session, and (for relay-only sessions,
				// relay is mandatory; for direct sessions, relay is optional fallback).
				var relayAllowed bool
				if reg != nil && cfg.RelayJWTSecret != "" && expectedStaticPub != "" {
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
						RelayOnly:         relayOnly,
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
						RelayOnly:         relayOnly,
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

			case "auth_fail":
				if agentID == "" {
					continue
				}
				failures := h.IncrementImmichAuthFailure(msg.ConnID)
				log.Printf("Immich auth failed: conn %s failure %d/5", msg.ConnID, failures)
				if failures >= 5 {
					h.CloseBrowserConnWithError(ctx, msg.ConnID, "too many incorrect password attempts")
				} else {
					h.ForwardToBrowserByConnID(ctx, msg.ConnID, map[string]any{
						"type":               "auth_fail",
						"attempts_remaining": 5 - failures,
					})
				}

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

	// Build ICE config for agent - STUN-only (no TURN)
	iceServers := turn.BuildICEConfig(&turn.ICEConfigRequest{
		STUNURL: cfg.STUNURL,
	})

	hub.SendDirect(ctx, conn, map[string]any{
		"type":        "welcome",
		"ice_servers": iceServers,
	})
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
			relayOnly := false
			if msg.RelayOnly != nil {
				relayOnly = *msg.RelayOnly
			}
			err = createSession(app, code, apiKeyID, agentID, msg.ExpiresAt, relayOnly, msg.RelayStaticPub, "", false)
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
		if !externalCodeRegex.MatchString(code) {
			hub.SendDirect(ctx, conn, map[string]string{
				"type":    "error",
				"message": "invalid external code format (8-128 chars, alphanumeric + hyphen + underscore)",
			})
			return
		}

		session, reclaimed, err := claimSessionCode(app, code, apiKeyID, accountID, agentID, msg.ExpiresAt, msg.RelayOnly, msg.RelayStaticPub, msg.ShareType, msg.IsPasswordProtected)
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
	if err := app.Delete(session); err != nil {
		hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "database error"})
		return
	}
	h.UnregisterCode(ctx, code, apiKeyID, "share has been removed")
	hub.SendDirect(ctx, conn, map[string]string{"type": "share_unregistered", "code": code})
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
func claimSessionCode(app core.App, code, apiKeyID, accountID, agentID string, expiresAt *time.Time, relayOnly *bool, relayStaticPub, shareType string, isPasswordProtected bool) (*core.Record, bool, error) {
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
			createRelayOnly := false
			if relayOnly != nil {
				createRelayOnly = *relayOnly
			}
			if err := createSession(txApp, code, apiKeyID, agentID, expiresAt, createRelayOnly, relayStaticPub, shareType, isPasswordProtected); err != nil {
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
		if relayOnly != nil {
			record.Set("relay_only", *relayOnly)
		}
		record.Set("relay_static_pub", relayStaticPub)
		record.Set("share_type", shareType)
		record.Set("is_password_protected", isPasswordProtected)
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
