package handler

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/config"
	"opencloudshare/server/internal/db"
	"opencloudshare/server/internal/hub"
	"opencloudshare/server/internal/turn"
)

type browserMsg struct {
	Type      string          `json:"type"`
	SDP       string          `json:"sdp,omitempty"`
	Candidate json.RawMessage `json:"candidate,omitempty"`
	HMAC      string          `json:"hmac,omitempty"`
}

func BrowserWS(h *hub.Hub, sessionRepo *db.SessionRepo, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionCode := c.Query("session")

		// Lookup session in database
		session, err := sessionRepo.GetByCode(sessionCode)
		if err != nil {
			log.Printf("browser_ws: error looking up session: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
			return
		}
		if session == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found or expired"})
			return
		}

		// Check expiry
		if session.ExpiresAt != nil && time.Now().After(*session.ExpiresAt) {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found or expired"})
			return
		}

		conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			log.Printf("browser_ws accept: %v", err)
			return
		}
		defer conn.CloseNow()

		ctx := c.Request.Context()

		// Check if agent is connected before pairing
		_, ok := h.GetAgentConn(sessionCode)
		if !ok {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "agent not connected"})
			conn.Close(websocket.StatusNormalClosure, "agent not connected")
			return
		}

		if err := h.PairSession(sessionCode, conn); err != nil {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "agent not connected"})
			conn.Close(websocket.StatusNormalClosure, "agent not connected")
			return
		}
		defer h.UnpairSession(sessionCode)

		log.Printf("browser joined session %s", sessionCode)

		// Build ICE config
		var turnCreds *turn.Credentials
		if cfg.HasTurn() {
			// Cap TURN credential TTL at 24h
			turnExpiry := time.Now().Add(24 * time.Hour)
			creds := turn.GenerateCredentials(cfg.TurnSecret, sessionCode, turnExpiry)
			turnCreds = &creds
		}

		iceServers := turn.BuildICEConfig(&turn.ICEConfigRequest{
			STUNURL:     cfg.STUNURL,
			TurnURL:     cfg.TurnURL(),
			Credentials: turnCreds,
		})

		// Send ICE config to browser
		hub.SendDirect(ctx, conn, map[string]any{
			"type":       "ice_config",
			"ice_servers": iceServers,
		})

		// Notify agent that browser has joined
		h.SendToAgent(ctx, session.APIKeyID, map[string]string{
			"type":       "join",
			"session_id": sessionCode,
		})

		// Relay messages from browser to agent
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				log.Printf("browser disconnected from session %s", sessionCode)
				return
			}

			var msg browserMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}

			switch msg.Type {
			case "answer":
				h.ForwardToAgent(ctx, sessionCode, map[string]any{
					"type":       "answer",
					"session_id": sessionCode,
					"sdp":        msg.SDP,
				})
			case "ice_candidate":
				h.ForwardToAgent(ctx, sessionCode, map[string]any{
					"type":       "ice_candidate",
					"session_id": sessionCode,
					"candidate":  msg.Candidate,
				})
			}
		}
	}
}
