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
	"sharebridge/server/internal/relay"
)

type browserMsg struct {
	Type          string `json:"type"`
	HMAC          string `json:"hmac,omitempty"`
	BrowserPeerID string `json:"browser_peer_id,omitempty"` // browser's libp2p peer ID
}

func BrowserWS(app core.App, sessionHub *hub.Hub, cfg *config.Config, rly *relay.Relay) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionCode := r.URL.Query().Get("session")
		records, err := app.FindRecordsByFilter("sessions", "code = {:code}", "", 1, 0, map[string]any{"code": sessionCode})
		if err != nil || len(records) == 0 {
			http.Error(w, `{"error":"session not found or expired"}`, http.StatusNotFound)
			return
		}
		sessionRecord := records[0]
		expiresAt := sessionRecord.GetDateTime("expires_at")
		if !expiresAt.IsZero() && time.Now().After(expiresAt.Time()) {
			http.Error(w, `{"error":"session not found or expired"}`, http.StatusNotFound)
			return
		}

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			log.Printf("browser_ws accept: %v", err)
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()

		_, agentConnected := sessionHub.GetAgentConn(sessionCode)
		if !agentConnected {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "agent not connected"})
			conn.Close(websocket.StatusNormalClosure, "agent not connected")
			return
		}
		if err := sessionHub.PairSession(sessionCode, conn); err != nil {
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "agent not connected"})
			conn.Close(websocket.StatusNormalClosure, "agent not connected")
			return
		}
		defer sessionHub.UnpairSession(sessionCode)

		connID := generateConnID()
		sessionHub.RegisterBrowserConn(connID, conn)
		defer sessionHub.UnregisterBrowserConn(connID)

		apiKeyID := sessionRecord.GetString("api_key_id")
		apiKeyRecord, err := app.FindRecordById("api_keys", apiKeyID)
		if err != nil {
			log.Printf("browser_ws: lookup api_key %s: %v", apiKeyID, err)
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "internal error"})
			return
		}
		accountID := apiKeyRecord.GetString("account_id")
		accountRecord, err := app.FindRecordById("users", accountID)
		if err != nil {
			log.Printf("browser_ws: lookup account %s: %v", accountID, err)
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "internal error"})
			return
		}

		// Relay-only + quota exceeded: reject immediately.
		// JWT issuance (and the non-relay_only quota check) happens in agent_ws on auth_ok.
		relayOnly := sessionRecord.GetBool("relay_only")
		quotaExceeded, _ := checkRelayQuota(accountRecord)
		if relayOnly && quotaExceeded {
			hub.SendDirect(ctx, conn, map[string]string{
				"type":    "error",
				"message": "file host's relay quota exceeded - this share requires relay which is unavailable",
			})
			conn.Close(websocket.StatusNormalClosure, "relay quota exceeded")
			return
		}

		log.Printf("browser connected to session %s (conn %s)", sessionCode, connID)

		// Main message loop.
		// JWT is NOT issued here — the server does not know whether a share has
		// a password (that knowledge lives on the agent only). JWT issuance happens
		// in agent_ws when the agent sends auth_ok.
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
				if msg.BrowserPeerID != "" {
					sessionHub.RememberBrowserPeerID(connID, msg.BrowserPeerID)
				}
				sessionHub.SendToAgent(ctx, apiKeyID, map[string]any{
					"type": "knock", "conn_id": connID, "code": sessionCode,
				})

			case "join":
				if msg.BrowserPeerID != "" {
					sessionHub.RememberBrowserPeerID(connID, msg.BrowserPeerID)
				}
				sessionHub.SendToAgent(ctx, apiKeyID, map[string]any{
					"type": "join", "conn_id": connID, "code": sessionCode, "hmac": msg.HMAC,
				})
			}
		}
	}
}

func generateConnID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}