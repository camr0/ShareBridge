package handler

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"log"
	"math/big"
	"regexp"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/config"
	"opencloudshare/server/internal/db"
	"opencloudshare/server/internal/hub"
	"opencloudshare/server/internal/turn"
)

// Agent message types from agent to server
type agentMsg struct {
	Type       string          `json:"type"`
	AgentID    string          `json:"agent_id,omitempty"`
	Code       string          `json:"code,omitempty"`
	ShareURL   string          `json:"share_url,omitempty"`
	ExpiresAt  *time.Time      `json:"expires_at,omitempty"`
	MaxDownloads *int          `json:"max_downloads,omitempty"`
	SessionID  string          `json:"session_id,omitempty"`
	SDP        string          `json:"sdp,omitempty"`
	Candidate  json.RawMessage `json:"candidate,omitempty"`
}

// codeRegex matches valid share codes: 8-30 chars, alphanumeric + hyphen + underscore
var codeRegex = regexp.MustCompile(`^[a-zA-Z0-9_-]{8,30}$`)

func AgentWS(h *hub.Hub, apiKeyRepo *db.APIKeyRepo, sessionRepo *db.SessionRepo, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Extract and validate API key from query param
		apiKeyFull := c.Query("api_key")
		if apiKeyFull == "" {
			c.JSON(401, gin.H{"error": "missing api_key"})
			return
		}

		apiKey, err := apiKeyRepo.Validate(apiKeyFull)
		if err != nil || apiKey == nil {
			c.JSON(401, gin.H{"error": "invalid api_key"})
			return
		}

		// Upgrade to WebSocket
		conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			log.Printf("agent_ws accept: %v", err)
			return
		}
		defer conn.CloseNow()

		ctx := c.Request.Context()
		var agentID string

		// Main message loop
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				if agentID != "" {
					log.Printf("agent disconnected: %s (agent_id: %s)", apiKey.ID, agentID)
					h.UnregisterAgent(apiKey.ID)
				}
				return
			}

			var msg agentMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}

			switch msg.Type {
			case "hello":
				handleHello(ctx, conn, h, apiKey.ID, msg.AgentID, cfg)
				agentID = msg.AgentID

			case "register_share":
				if agentID == "" {
					hub.SendDirect(ctx, conn, map[string]string{
						"type":    "error",
						"message": "hello required before register_share",
					})
					continue
				}
				handleRegisterShare(ctx, conn, h, apiKey, sessionRepo, agentID, msg)

			case "download_complete":
				if agentID == "" {
					continue
				}
				handleDownloadComplete(ctx, conn, sessionRepo, msg.Code)

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

			case "auth_failed":
				log.Printf("auth failed on session %s", msg.SessionID)

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
		"type":       "welcome",
		"ice_servers": iceServers,
	})
}

// handleRegisterShare processes share registration (new or reconnect)
func handleRegisterShare(
	ctx context.Context,
	conn *websocket.Conn,
	h *hub.Hub,
	apiKey *db.APIKey,
	sessionRepo *db.SessionRepo,
	agentID string,
	msg agentMsg,
) {
	// Validate share_url is provided
	if msg.ShareURL == "" {
		hub.SendDirect(ctx, conn, map[string]string{
			"type":    "error",
			"message": "share_url required",
		})
		return
	}

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
			session := &db.Session{
				Code:         code,
				APIKeyID:     apiKey.ID,
				AgentID:      agentID,
				ShareURL:     msg.ShareURL,
				ExpiresAt:    msg.ExpiresAt,
				MaxDownloads: msg.MaxDownloads,
			}
			err = sessionRepo.Create(session)
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
		available, err := sessionRepo.IsCodeAvailable(code, apiKey.ID)
		if err != nil {
			log.Printf("db error checking code availability: %v", err)
			hub.SendDirect(ctx, conn, map[string]string{
				"type":    "error",
				"message": "database error",
			})
			return
		}

		if available {
			// Code is available (new or owned by this API key)
			existingSession, err := sessionRepo.GetByCode(code)
			if err != nil {
				log.Printf("db error getting session: %v", err)
				hub.SendDirect(ctx, conn, map[string]string{
					"type":    "error",
					"message": "database error",
				})
				return
			}

			if existingSession != nil {
				// Verify agent_id matches for reconnection
				if existingSession.AgentID != agentID {
					hub.SendDirect(ctx, conn, map[string]string{
						"type":    "error",
						"message": "session owned by different agent",
					})
					return
				}
				// Reconnecting to existing session - update timestamps
				existingSession.AgentID = agentID
				if err := sessionRepo.Update(existingSession); err != nil {
					log.Printf("db error updating session: %v", err)
					hub.SendDirect(ctx, conn, map[string]string{
						"type":    "error",
						"message": "database error",
					})
					return
				}
				reconnected = true
				log.Printf("session reconnected: code=%s api_key=%s agent_id=%s", code, apiKey.ID, agentID)
			} else {
				// Creating new session with custom code
				session := &db.Session{
					Code:         code,
					APIKeyID:     apiKey.ID,
					AgentID:      agentID,
					ShareURL:     msg.ShareURL,
					ExpiresAt:    msg.ExpiresAt,
					MaxDownloads: msg.MaxDownloads,
				}
				if err := sessionRepo.Create(session); err != nil {
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
	h.RegisterCode(code, apiKey.ID)

	// Get session for response
	session, err := sessionRepo.GetByCode(code)
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
	if session.ExpiresAt != nil {
		response["expires_at"] = session.ExpiresAt.Format(time.RFC3339)
	}

	hub.SendDirect(ctx, conn, response)
	log.Printf("share registered: code=%s api_key=%s agent_id=%s reconnected=%v", code, apiKey.ID, agentID, reconnected)
}

// handleDownloadComplete increments the download count for a session
func handleDownloadComplete(
	ctx context.Context,
	conn *websocket.Conn,
	sessionRepo *db.SessionRepo,
	code string,
) {
	if code == "" {
		hub.SendDirect(ctx, conn, map[string]string{
			"type":    "error",
			"message": "code required",
		})
		return
	}

	if err := sessionRepo.IncrementDownloadCount(code); err != nil {
		log.Printf("failed to increment download count for %s: %v", code, err)
		hub.SendDirect(ctx, conn, map[string]string{
			"type":    "error",
			"message": "failed to update download count",
		})
		return
	}

	log.Printf("download complete recorded for session %s", code)
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
