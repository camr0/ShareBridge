package handler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/turn"
)

type browserMsg struct {
	Type      string          `json:"type"`
	SDP       string          `json:"sdp,omitempty"`
	Candidate json.RawMessage `json:"candidate,omitempty"`
	HMAC      string          `json:"hmac,omitempty"`
}

func BrowserWS(app core.App, h *hub.Hub, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionCode := r.URL.Query().Get("session")

		records, err := app.FindRecordsByFilter(
			"sessions",
			"code = {:code}",
			"",
			1,
			0,
			map[string]any{"code": sessionCode},
		)
		if err != nil {
			log.Printf("browser_ws: db error looking up session: %v", err)
			http.Error(w, `{"error":"database error"}`, http.StatusInternalServerError)
			return
		}
		if len(records) == 0 {
			http.Error(w, `{"error":"session not found or expired"}`, http.StatusNotFound)
			return
		}
		session := records[0]

		expiresAt := session.GetDateTime("expires_at")
		if !expiresAt.IsZero() && time.Now().After(expiresAt.Time()) {
			http.Error(w, `{"error":"session not found or expired"}`, http.StatusNotFound)
			return
		}

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			log.Printf("browser_ws accept: %v", err)
			return
		}
		defer conn.CloseNow()

		ctx := r.Context()

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

		apiKeyID := session.GetString("api_key_id")

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
				h.SendToAgent(ctx, apiKeyID, map[string]any{
					"type":    "knock",
					"conn_id": connID,
					"code":    sessionCode,
				})

			case "join":
				h.SendToAgent(ctx, apiKeyID, map[string]any{
					"type":    "join",
					"conn_id": connID,
					"code":    sessionCode,
					"hmac":    msg.HMAC,
				})

			case "answer":
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
