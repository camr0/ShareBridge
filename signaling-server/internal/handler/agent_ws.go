package handler

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log"
	"math/big"
	"net/http"
	"regexp"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/middleware"
	"sharebridge/server/internal/turn"
)

// Agent message types from agent to server
type agentMsg struct {
	Type        string          `json:"type"`
	AgentID     string          `json:"agent_id,omitempty"`
	Code        string          `json:"code,omitempty"`
	ShareURL    string          `json:"share_url,omitempty"` // received for protocol compat, not stored
	ExpiresAt   *time.Time      `json:"expires_at,omitempty"`
	SessionID   string          `json:"session_id,omitempty"`
	SDP         string          `json:"sdp,omitempty"`
	Candidate   json.RawMessage `json:"candidate,omitempty"`
	ConnID      string          `json:"conn_id,omitempty"`
	Value       string          `json:"value,omitempty"`
	HasPassword bool            `json:"has_password,omitempty"`
}

// codeRegex matches valid share codes: 8-30 chars, alphanumeric + hyphen + underscore
var codeRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,30}$`)

// AgentWS handles WebSocket connections from agents.
// It expects the api_key_id to be set in the request context by APIKeyAuth middleware.
func AgentWS(app core.App, h *hub.Hub, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Extract API key ID and account ID from context (set by APIKeyAuth middleware)
		apiKeyID := middleware.GetAPIKeyID(r.Context())
		accountID := middleware.GetAccountID(r.Context())
		if apiKeyID == "" {
			http.Error(w, `{"error":"missing api_key"}`, http.StatusUnauthorized)
			return
		}

		// Upgrade to WebSocket
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			log.Printf("agent_ws accept: %v", err)
			return
		}
		defer conn.CloseNow()

		ctx := r.Context()
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
				handleHello(ctx, conn, h, apiKeyID, msg.AgentID, cfg)
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
				h.ForwardToBrowser(ctx, msg.SessionID, map[string]any{
					"type":      "ice_candidate",
					"candidate": msg.Candidate,
				})

			case "nonce":
				// Route nonce from agent to the specific browser identified by connID.
				if agentID == "" {
					continue
				}
				h.ForwardToBrowserByConnID(ctx, msg.ConnID, map[string]any{
					"type":         "nonce",
					"conn_id":      msg.ConnID,
					"value":        msg.Value,
					"has_password": msg.HasPassword,
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
func handleHello(ctx context.Context, conn *websocket.Conn, h *hub.Hub, apiKeyID string, agentID string, cfg *config.Config) {
	if agentID == "" {
		hub.SendDirect(ctx, conn, map[string]string{
			"type":    "error",
			"message": "agent_id required",
		})
		return
	}

	// Register agent with the hub using apiKeyID
	h.RegisterAgent(apiKeyID, conn)
	log.Printf("agent hello received: api_key=%s agent_id=%s", apiKeyID, agentID)

	// Build ICE config for agent
	var turnCreds *turn.Credentials
	if cfg.HasTurn() {
		turnExpiry := time.Now().Add(24 * time.Hour)
		creds := turn.GenerateCredentials(cfg.TurnSecret, apiKeyID, turnExpiry)
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
			err = createSession(app, code, apiKeyID, agentID, msg.ExpiresAt)
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

		// Check if code exists and is owned by this API key
		available, existingSession, err := isCodeAvailable(app, code, apiKeyID, accountID)
		if err != nil {
			log.Printf("db error checking code availability: %v", err)
			hub.SendDirect(ctx, conn, map[string]string{
				"type":    "error",
				"message": "database error",
			})
			return
		}

		if available {
			if existingSession != nil {
				// Verify agent_id matches for reconnection
				existingAgentID := existingSession.GetString("agent_id")
				if existingAgentID != agentID {
					hub.SendDirect(ctx, conn, map[string]string{
						"type":    "error",
						"message": "session owned by different agent",
					})
					return
				}
				// Reconnecting to existing session - update timestamps
				existingSession.Set("agent_id", agentID)
				if err := app.Save(existingSession); err != nil {
					log.Printf("db error updating session: %v", err)
					hub.SendDirect(ctx, conn, map[string]string{
						"type":    "error",
						"message": "database error",
					})
					return
				}
				reconnected = true
				log.Printf("session reconnected: code=%s api_key=%s agent_id=%s", code, apiKeyID, agentID)
			} else {
				// Creating new session with custom code
				err := createSession(app, code, apiKeyID, agentID, msg.ExpiresAt)
				if err != nil {
					log.Printf("db error creating session: %v", err)
					hub.SendDirect(ctx, conn, map[string]string{
						"type":    "error",
						"message": "failed to create session",
					})
					return
				}
			}
		} else {
			// Code is taken by a different API key
			hub.SendDirect(ctx, conn, map[string]string{
				"type":    "error",
				"message": "code already in use",
			})
			return
		}
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
	log.Printf("share registered: code=%s api_key=%s agent_id=%s reconnected=%v", code, apiKeyID, agentID, reconnected)
}

// createSession creates a new session record in PocketBase.
// share_url and max_downloads are intentionally not stored — the server is untrusted.
func createSession(app core.App, code, apiKeyID, agentID string, expiresAt *time.Time) error {
	col, err := app.FindCollectionByNameOrId("sessions")
	if err != nil {
		return err
	}

	record := core.NewRecord(col)
	record.Set("code", code)
	record.Set("api_key_id", apiKeyID)
	record.Set("agent_id", agentID)

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

// isCodeAvailable checks whether a code can be claimed by the requesting API key.
// Returns (available, existingSession, error).
//   - available = true if the code is free, owned by the same key, or owned by a
//     different key that belongs to the same account (key rotation case).
//   - When available=true and existingSession!=nil, the session's api_key_id is
//     updated in-place so the new key takes over ownership.
func isCodeAvailable(app core.App, code string, apiKeyID string, accountID string) (bool, *core.Record, error) {
	session, err := getSessionByCode(app, code)
	if err != nil {
		return false, nil, err
	}
	if session == nil {
		return true, nil, nil
	}

	existingKeyID := session.GetString("api_key_id")
	if existingKeyID == apiKeyID {
		// Same key — straightforward reconnect.
		return true, session, nil
	}

	// Different key: check if it belongs to the same account (key rotation).
	existingKey, err := app.FindRecordById("api_keys", existingKeyID)
	if err != nil {
		// Key record missing (e.g. deleted) — treat code as available.
		return true, session, nil
	}
	existingAccountID := existingKey.GetString("account_id")
	if existingAccountID != accountID {
		// Owned by a different account — not available.
		return false, nil, nil
	}

	// Same account, different key (key rotation). Update ownership.
	session.Set("api_key_id", apiKeyID)
	if err := app.Save(session); err != nil {
		return false, nil, err
	}
	return true, session, nil
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
