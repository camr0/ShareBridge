package handler

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/hub"
	"opencloudshare/server/internal/session"
)

type browserMsg struct {
	Type      string          `json:"type"`
	SDP       string          `json:"sdp,omitempty"`
	Candidate json.RawMessage `json:"candidate,omitempty"`
}

func BrowserWS(h *hub.Hub, sessions *session.Manager, stunURL string) gin.HandlerFunc {
	return func(c *gin.Context) {
		sessionID := c.Query("session")
		s, ok := sessions.Get(sessionID)
		if !ok {
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

		if err := h.PairSession(sessionID, s.Token, conn); err != nil {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "agent not connected"})
			conn.Close(websocket.StatusNormalClosure, "agent not connected")
			return
		}
		defer h.UnpairSession(sessionID)

		log.Printf("browser joined session %s", sessionID)

		// Send ICE config to browser
		hub.SendDirect(ctx, conn, map[string]any{
			"type": "ice_config",
			"ice_servers": []map[string]string{
				{"urls": stunURL},
			},
		})

		// Notify agent that browser has joined
		h.SendToAgent(ctx, s.Token, map[string]string{
			"type":       "join",
			"session_id": sessionID,
		})

		// Relay messages from browser to agent
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				log.Printf("browser disconnected from session %s", sessionID)
				return
			}

			var msg browserMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}

			switch msg.Type {
			case "answer":
				h.ForwardToAgent(ctx, sessionID, map[string]any{
					"type":       "answer",
					"session_id": sessionID,
					"sdp":        msg.SDP,
				})
			case "ice_candidate":
				h.ForwardToAgent(ctx, sessionID, map[string]any{
					"type":       "ice_candidate",
					"session_id": sessionID,
					"candidate":  msg.Candidate,
				})
			}
		}
	}
}
