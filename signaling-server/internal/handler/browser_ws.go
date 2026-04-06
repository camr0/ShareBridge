package handler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/db"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/turn"
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

		session, err := sessionRepo.GetByCode(sessionCode)
		if err != nil {
			log.Printf("browser_ws: db error looking up session: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "database error"})
			return
		}
		if session == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found or expired"})
			return
		}
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

		_, agentOK := h.GetAgentConn(sessionCode)
		if !agentOK {
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

		// Generate a unique connID for this browser connection.
		// Used to correlate knock/nonce/join messages and route auth failures.
		connID := generateConnID()
		h.RegisterBrowserConn(connID, conn)
		defer h.UnregisterBrowserConn(connID)

		log.Printf("browser connected to session %s (conn %s)", sessionCode, connID)

		// Send ICE config immediately so the browser can set up RTCPeerConnection
		// while the knock/nonce round-trip happens in parallel.
		var turnCreds *turn.Credentials
		if cfg.HasTurn() {
			turnExpiry := time.Now().Add(24 * time.Hour)
			creds := turn.GenerateCredentials(cfg.TurnSecret, sessionCode, turnExpiry)
			turnCreds = &creds
		}
		iceServers := turn.BuildICEConfig(&turn.ICEConfigRequest{
			STUNURL:     cfg.STUNURL,
			TurnURL:     cfg.TurnURL(),
			Credentials: turnCreds,
		})
		hub.SendDirect(ctx, conn, map[string]any{
			"type":        "ice_config",
			"ice_servers": iceServers,
		})

		// NOTE: we do NOT notify the agent here (no join message).
		// The agent is notified only after the browser passes HMAC verification
		// via the knock → nonce → join(hmac) flow.

		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				log.Printf("browser disconnected from session %s (conn %s)", sessionCode, connID)
				return
			}

			var msg browserMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}

			switch msg.Type {
			case "knock":
				// Forward knock to agent with connID and session code added by server.
				h.SendToAgent(ctx, session.APIKeyID, map[string]any{
					"type":    "knock",
					"conn_id": connID,
					"code":    sessionCode,
				})

			case "join":
				// Forward join to agent with connID, code, and HMAC. Agent verifies.
				h.SendToAgent(ctx, session.APIKeyID, map[string]any{
					"type":    "join",
					"conn_id": connID,
					"code":    sessionCode,
					"hmac":    msg.HMAC,
				})

			case "answer":
				// Include connID as peer_id so agent can look up the right peer.
				h.ForwardToAgent(ctx, sessionCode, map[string]any{
					"type":       "answer",
					"session_id": sessionCode,
					"peer_id":    connID,
					"sdp":        msg.SDP,
				})

			case "ice_candidate":
				h.ForwardToAgent(ctx, sessionCode, map[string]any{
					"type":       "ice_candidate",
					"session_id": sessionCode,
					"peer_id":    connID,
					"candidate":  msg.Candidate,
				})
			}
		}
	}
}

// generateConnID returns a random 32-char hex string for use as a connection ID.
func generateConnID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
