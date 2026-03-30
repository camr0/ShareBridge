package handler

import (
	"encoding/json"
	"log"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/hub"
)

type agentMsg struct {
	Type      string          `json:"type"`
	Token     string          `json:"token,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	SDP       string          `json:"sdp,omitempty"`
	Candidate json.RawMessage `json:"candidate,omitempty"`
}

func AgentWS(h *hub.Hub, authToken string) gin.HandlerFunc {
	return func(c *gin.Context) {
		conn, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
			InsecureSkipVerify: true, // allow any origin in dev
		})
		if err != nil {
			log.Printf("agent_ws accept: %v", err)
			return
		}
		defer conn.CloseNow()

		ctx := c.Request.Context()
		var token string

		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				if token != "" {
					log.Printf("agent disconnected: %s", token)
					h.UnregisterAgent(token)
				}
				return
			}

			var msg agentMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}

			switch msg.Type {
			case "register":
				if msg.Token != authToken {
					// token var is still "" here — use SendDirect, not SendToAgent
					hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "unauthorized"})
					conn.Close(websocket.StatusPolicyViolation, "unauthorized")
					return
				}
				token = msg.Token
				h.RegisterAgent(token, conn)
				log.Printf("agent registered: %s", token)
				h.SendToAgent(ctx, token, map[string]string{"type": "registered"})

			case "offer":
				if token == "" {
					continue
				}
				h.ForwardToBrowser(ctx, msg.SessionID, map[string]any{
					"type": "offer",
					"sdp":  msg.SDP,
				})

			case "ice_candidate":
				if token == "" {
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
