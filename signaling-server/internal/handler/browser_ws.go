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

		// Look up the API key to get the account for quota checking
		apiKeyID := sessionRecord.GetString("api_key_id")
		apiKeyRecord, err := app.FindRecordById("api_keys", apiKeyID)
		if err != nil {
			log.Printf("browser_ws: error looking up api_key %s: %v", apiKeyID, err)
			hub.SendDirect(requestCtx, browserConn, map[string]string{"type": "error", "message": "internal error"})
			browserConn.Close(websocket.StatusInternalError, "internal error")
			return
		}

		accountID := apiKeyRecord.GetString("account_id")
		if accountID == "" {
			log.Printf("browser_ws: api_key %s has empty account_id", apiKeyID)
			hub.SendDirect(requestCtx, browserConn, map[string]string{"type": "error", "message": "internal error"})
			browserConn.Close(websocket.StatusInternalError, "internal error")
			return
		}

		// Look up the account for quota checking
		accountRecord, err := app.FindRecordById("users", accountID)
		if err != nil {
			log.Printf("browser_ws: error looking up account %s: %v", accountID, err)
			hub.SendDirect(requestCtx, browserConn, map[string]string{"type": "error", "message": "internal error"})
			browserConn.Close(websocket.StatusInternalError, "internal error")
			return
		}

		// Check quota
		quotaExceeded, periodEnd := checkRelayQuota(accountRecord)

		// Send ICE config - only include TURN if quota not exceeded
		// Use account-scoped TURN username to bound Prometheus label cardinality.
		var turnCreds *turn.Credentials
		if cfg.HasTurn() && !quotaExceeded {
			turnExpiry := time.Now().Add(24 * time.Hour)
			generatedCreds := turn.GenerateCredentials(cfg.TurnSecret, accountID, turnExpiry)
			turnCreds = &generatedCreds
		}
		iceServers := turn.BuildICEConfig(&turn.ICEConfigRequest{
			STUNURL:     cfg.STUNURL,
			TurnURL:     cfg.TurnURL(),
			Credentials: turnCreds,
		})

		if quotaExceeded {
			hub.SendDirect(requestCtx, browserConn, map[string]any{
				"type":                "ice_config",
				"ice_servers":         iceServers,
				"relay_quota_exceeded": true,
				"quota_period_end":    periodEnd.Format(time.RFC3339),
			})
		} else {
			hub.SendDirect(requestCtx, browserConn, map[string]any{
				"type":        "ice_config",
				"ice_servers": iceServers,
			})
		}


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
