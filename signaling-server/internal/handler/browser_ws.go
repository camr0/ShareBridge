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

func BrowserWS(app core.App, sessionHub *hub.Hub, cfg *config.Config) http.HandlerFunc {
	return func(responseWriter http.ResponseWriter, request *http.Request) {
		sessionCode := request.URL.Query().Get("session")

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
			http.Error(responseWriter, `{"error":"session not found or expired"}`, http.StatusNotFound)
			return
		}
		sessionRecord := records[0]

		expiresAt := sessionRecord.GetDateTime("expires_at")
		if !expiresAt.IsZero() && time.Now().After(expiresAt.Time()) {
			http.Error(responseWriter, `{"error":"session not found or expired"}`, http.StatusNotFound)
			return
		}

		browserConn, err := websocket.Accept(responseWriter, request, nil)
		if err != nil {
			log.Printf("browser_ws accept: %v", err)
			return
		}
		defer browserConn.CloseNow()

		requestCtx := request.Context()

		_, agentConnected := sessionHub.GetAgentConn(sessionCode)
		if !agentConnected {
			hub.SendDirect(requestCtx, browserConn, map[string]string{"type": "error", "message": "agent not connected"})
			browserConn.Close(websocket.StatusNormalClosure, "agent not connected")
			return
		}

		if err := sessionHub.PairSession(sessionCode, browserConn); err != nil {
			hub.SendDirect(requestCtx, browserConn, map[string]string{"type": "error", "message": "agent not connected"})
			browserConn.Close(websocket.StatusNormalClosure, "agent not connected")
			return
		}
		defer sessionHub.UnpairSession(sessionCode)

		connID := generateConnID()
		sessionHub.RegisterBrowserConn(connID, browserConn)
		defer sessionHub.UnregisterBrowserConn(connID)

		log.Printf("browser connected to session %s (conn %s)", sessionCode, connID)

		// Send ICE config immediately so the browser can set up RTCPeerConnection
		// while the knock/nonce round-trip happens in parallel.
		var turnCreds *turn.Credentials
		if cfg.HasTurn() {
			turnExpiry := time.Now().Add(24 * time.Hour)
			// Use account-scoped TURN username to bound Prometheus label cardinality.
			apiKeyRecord, err := app.FindRecordById("api_keys", sessionRecord.GetString("api_key_id"))
			if err != nil {
				http.Error(responseWriter, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}
			accountID := apiKeyRecord.GetString("account_id")
			if accountID == "" {
				log.Printf("browser_ws: api_key %s has empty account_id", sessionRecord.GetString("api_key_id"))
				http.Error(responseWriter, `{"error":"internal error"}`, http.StatusInternalServerError)
				return
			}

			generatedCreds := turn.GenerateCredentials(cfg.TurnSecret, accountID, turnExpiry)
			turnCreds = &generatedCreds
		}
		iceServers := turn.BuildICEConfig(&turn.ICEConfigRequest{
			STUNURL:     cfg.STUNURL,
			TurnURL:     cfg.TurnURL(),
			Credentials: turnCreds,
		})
		hub.SendDirect(requestCtx, browserConn, map[string]any{
			"type":        "ice_config",
			"ice_servers": iceServers,
		})

		apiKeyID := sessionRecord.GetString("api_key_id")

		for {
			_, data, err := browserConn.Read(requestCtx)
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

// generateConnID returns a random 32-char hex string for use as a connection ID.
func generateConnID() string {
	randomBytes := make([]byte, 16)
	rand.Read(randomBytes)
	return hex.EncodeToString(randomBytes)
}
