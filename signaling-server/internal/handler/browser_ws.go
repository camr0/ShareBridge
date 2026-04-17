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
		if cfg.DebugWS {
			log.Printf("browser_ws: incoming method=%s path=%s rawQuery=%q remote=%s host=%s origin=%q upgrade=%q connection=%q version=%q key_present=%t extensions=%q ua=%q",
				r.Method,
				r.URL.Path,
				r.URL.RawQuery,
				r.RemoteAddr,
				r.Host,
				r.Header.Get("Origin"),
				r.Header.Get("Upgrade"),
				r.Header.Get("Connection"),
				r.Header.Get("Sec-WebSocket-Version"),
				r.Header.Get("Sec-WebSocket-Key") != "",
				r.Header.Get("Sec-WebSocket-Extensions"),
				r.Header.Get("User-Agent"),
			)
		}
		records, err := app.FindRecordsByFilter("sessions", "code = {:code}", "", 1, 0, map[string]any{"code": sessionCode})
		if err != nil || len(records) == 0 {
			if cfg.DebugWS {
				log.Printf("browser_ws: session lookup miss code=%q err=%v", sessionCode, err)
			}
			http.Error(w, `{"error":"session not found or expired"}`, http.StatusNotFound)
			return
		}
		sessionRecord := records[0]
		expiresAt := sessionRecord.GetDateTime("expires_at")
		if !expiresAt.IsZero() && time.Now().After(expiresAt.Time()) {
			if cfg.DebugWS {
				log.Printf("browser_ws: session expired code=%q expires_at=%s", sessionCode, expiresAt.Time().Format(time.RFC3339))
			}
			http.Error(w, `{"error":"session not found or expired"}`, http.StatusNotFound)
			return
		}

		if cfg.DebugWS {
			log.Printf("browser_ws: accepting code=%q relay_only=%t api_key_id=%s", sessionCode, sessionRecord.GetBool("relay_only"), sessionRecord.GetString("api_key_id"))
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			log.Printf("browser_ws accept: %v", err)
			return
		}
		if cfg.DebugWS {
			log.Printf("browser_ws: accepted code=%q remote=%s subprotocol=%q", sessionCode, r.RemoteAddr, conn.Subprotocol())
		}
		defer conn.CloseNow()
		ctx := r.Context()

		_, agentConnected := sessionHub.GetAgentConn(sessionCode)
		if !agentConnected {
			if cfg.DebugWS {
				log.Printf("browser_ws: agent not connected code=%q", sessionCode)
			}
			hub.SendDirect(ctx, conn, map[string]string{"type": "error", "message": "agent not connected"})
			conn.Close(websocket.StatusNormalClosure, "agent not connected")
			return
		}
		if err := sessionHub.PairSession(sessionCode, conn); err != nil {
			if cfg.DebugWS {
				log.Printf("browser_ws: pair failed code=%q err=%v", sessionCode, err)
			}
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
				if cfg.DebugWS {
					log.Printf("browser_ws: read ended code=%q conn=%s err=%v", sessionCode, connID, err)
				}
				log.Printf("browser disconnected from session %s (conn %s)", sessionCode, connID)
				return
			}
			var msg browserMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				if cfg.DebugWS {
					log.Printf("browser_ws: invalid json code=%q conn=%s err=%v raw=%q", sessionCode, connID, err, string(data))
				}
				continue
			}
			if cfg.DebugWS {
				log.Printf("browser_ws: recv code=%q conn=%s type=%q browser_peer_id_present=%t hmac_present=%t", sessionCode, connID, msg.Type, msg.BrowserPeerID != "", msg.HMAC != "")
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
