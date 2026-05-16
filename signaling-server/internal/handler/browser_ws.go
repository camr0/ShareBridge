package handler

import (
	"context"
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

func BrowserWS(app core.App, sessionHub *hub.Hub, cfg *config.Config) http.HandlerFunc {
	return func(responseWriter http.ResponseWriter, request *http.Request) {
		sessionCode := request.URL.Query().Get("session")
		log.Printf("browser_ws: request received session=%q remote=%s ua=%q upgrade=%q connection=%q wskey=%q",
			sessionCode,
			request.RemoteAddr,
			request.UserAgent(),
			request.Header.Get("Upgrade"),
			request.Header.Get("Connection"),
			request.Header.Get("Sec-WebSocket-Key"),
		)

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
			http.Error(responseWriter, `{"error":"database error"}`, http.StatusInternalServerError)
			return
		}
		if len(records) == 0 {
			log.Printf("browser_ws: session lookup returned no records for code=%q", sessionCode)
			http.Error(responseWriter, `{"error":"session not found or expired"}`, http.StatusNotFound)
			return
		}
		sessionRecord := records[0]

		expiresAt := sessionRecord.GetDateTime("expires_at")
		if !expiresAt.IsZero() && time.Now().After(expiresAt.Time()) {
			log.Printf("browser_ws: session expired code=%q expires_at=%s now=%s",
				sessionCode,
				expiresAt.Time().UTC().Format(time.RFC3339),
				time.Now().UTC().Format(time.RFC3339),
			)
			http.Error(responseWriter, `{"error":"session not found or expired"}`, http.StatusNotFound)
			return
		}

		log.Printf("browser_ws: session lookup succeeded code=%q api_key_id=%q relay_only=%v",
			sessionCode,
			sessionRecord.GetString("api_key_id"),
			sessionRecord.GetBool("relay_only"),
		)
		browserConn, err := websocket.Accept(responseWriter, request, &websocket.AcceptOptions{
			InsecureSkipVerify: true,
		})
		if err != nil {
			log.Printf("browser_ws accept: %v", err)
			return
		}
		defer browserConn.CloseNow()

		// coder/websocket recommends not using request.Context() for upgraded
		// connection lifetime, because it can be canceled surprisingly after
		// the HTTP upgrade machinery completes.
		requestCtx := context.Background()

		_, agentConnected := sessionHub.GetAgentConn(sessionCode)
		if !agentConnected {
			sessionHub.SendDirect(requestCtx, browserConn, map[string]string{"type": "error", "message": "agent not connected"})
			browserConn.Close(websocket.StatusNormalClosure, "agent not connected")
			return
		}

		if err := sessionHub.PairSession(sessionCode, browserConn); err != nil {
			sessionHub.SendDirect(requestCtx, browserConn, map[string]string{"type": "error", "message": "agent not connected"})
			browserConn.Close(websocket.StatusNormalClosure, "agent not connected")
			return
		}
		defer sessionHub.UnpairSession(sessionCode, browserConn)

		connID := generateConnID()
		sessionHub.RegisterBrowserConn(connID, browserConn)
		defer sessionHub.UnregisterBrowserConn(connID)

		log.Printf("browser connected to session %s (conn %s)", sessionCode, connID)

		// Look up the API key to get the account for quota checking
		apiKeyID := sessionRecord.GetString("api_key_id")
		apiKeyRecord, err := app.FindRecordById("api_keys", apiKeyID)
		if err != nil {
			log.Printf("browser_ws: error looking up api_key %s: %v", apiKeyID, err)
			sessionHub.SendDirect(requestCtx, browserConn, map[string]string{"type": "error", "message": "internal error"})
			browserConn.Close(websocket.StatusInternalError, "internal error")
			return
		}

		accountID := apiKeyRecord.GetString("account_id")
		if accountID == "" {
			log.Printf("browser_ws: api_key %s has empty account_id", apiKeyID)
			sessionHub.SendDirect(requestCtx, browserConn, map[string]string{"type": "error", "message": "internal error"})
			browserConn.Close(websocket.StatusInternalError, "internal error")
			return
		}

		// Look up the account for quota checking
		accountRecord, err := app.FindRecordById("users", accountID)
		if err != nil {
			log.Printf("browser_ws: error looking up account %s: %v", accountID, err)
			sessionHub.SendDirect(requestCtx, browserConn, map[string]string{"type": "error", "message": "internal error"})
			browserConn.Close(websocket.StatusInternalError, "internal error")
			return
		}

		// Check quota
		quotaExceeded, periodEnd := checkRelayQuota(accountRecord)

		// Check relay_only - if session requires relay and quota exceeded, reject immediately
		relayOnly := sessionRecord.GetBool("relay_only")
		if relayOnly && quotaExceeded {
			log.Printf("browser_ws: relay_only session %s with quota exceeded, rejecting", sessionCode)
			sessionHub.SendDirect(requestCtx, browserConn, map[string]string{
				"type":    "error",
				"message": "file host's relay quota exceeded - this share requires secure relay which is unavailable",
			})
			browserConn.Close(websocket.StatusNormalClosure, "relay quota exceeded")
			return
		}

		// Send ICE config - STUN-only (no TURN)
		iceServers := turn.BuildICEConfig(&turn.ICEConfigRequest{
			STUNURL: cfg.STUNURL,
		})

		log.Printf("browser_ws: sending ICE config for session %s: STUN=%s", sessionCode, cfg.STUNURL)

		if quotaExceeded {
			msg := map[string]any{
				"type":                 "ice_config",
				"ice_servers":          iceServers,
				"relay_only":           relayOnly,
				"relay_quota_exceeded": true,
				"quota_period_end":     periodEnd.Format(time.RFC3339),
			}
			log.Printf("browser_ws: sending ice_config (quota exceeded): %+v", msg)
			sessionHub.SendDirect(requestCtx, browserConn, msg)
		} else {
			msg := map[string]any{
				"type":        "ice_config",
				"ice_servers": iceServers,
				"relay_only":  relayOnly,
			}
			log.Printf("browser_ws: sending ice_config: %+v", msg)
			sessionHub.SendDirect(requestCtx, browserConn, msg)
		}

		// Relay-only sessions skip direct browser setup, so the server must kick
		// off the initial HMAC challenge instead of waiting for a browser-side
		// "knock" message.
		if relayOnly {
			log.Printf("browser_ws: relay_only session %s - server initiating nonce challenge for conn %s", sessionCode, connID)
			if err := sessionHub.SendToAgent(requestCtx, apiKeyID, map[string]any{
				"type":    "knock",
				"conn_id": connID,
				"code":    sessionCode,
			}); err != nil {
				log.Printf("browser_ws: initial relay-only knock send failed for session %s conn %s: %v", sessionCode, connID, err)
			}
		}

		for {
			_, data, err := browserConn.Read(requestCtx)
			if err != nil {
				log.Printf("browser disconnected from session %s (conn %s): %v", sessionCode, connID, err)
				return
			}

			var msg browserMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}

			switch msg.Type {
			case "knock":
				sessionHub.SendToAgent(requestCtx, apiKeyID, map[string]any{
					"type":    "knock",
					"conn_id": connID,
					"code":    sessionCode,
				})

			case "join":
				sessionHub.SendToAgent(requestCtx, apiKeyID, map[string]any{
					"type":    "join",
					"conn_id": connID,
					"code":    sessionCode,
					"hmac":    msg.HMAC,
				})

			case "answer":
				sessionHub.ForwardToAgent(requestCtx, sessionCode, map[string]any{
					"type":       "answer",
					"session_id": sessionCode,
					"peer_id":    connID,
					"sdp":        msg.SDP,
				})

			case "ice_candidate":
				sessionHub.ForwardToAgent(requestCtx, sessionCode, map[string]any{
					"type":       "ice_candidate",
					"session_id": sessionCode,
					"peer_id":    connID,
					"candidate":  msg.Candidate,
				})
			}
		}
	}
}

// checkRelayQuota checks if the account has exceeded their relay quota.
// Returns (exceeded, periodEnd) where exceeded is true if usage >= limit.
func checkRelayQuota(accountRecord *core.Record) (bool, time.Time) {
	limitGB := accountRecord.GetFloat("relay_quota_gb")
	usedGB := accountRecord.GetFloat("current_period_usage_gb")
	periodEnd := accountRecord.GetDateTime("quota_period_end").Time()
	return usedGB >= limitGB, periodEnd
}
func generateConnID() string {
	randomBytes := make([]byte, 16)
	rand.Read(randomBytes)
	return hex.EncodeToString(randomBytes)
}
