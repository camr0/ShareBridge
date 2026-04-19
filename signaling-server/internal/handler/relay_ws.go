package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/relay"
)

// helloTimeout is the maximum time to wait for a client to send its hello message.
// This prevents slowloris attacks where malicious clients connect but never send data.
var helloTimeout = 10 * time.Second

// SetHelloTimeout sets the hello timeout (for testing).
func SetHelloTimeout(d time.Duration) {
	helloTimeout = d
}

// HelloTimeout returns the current hello timeout.
func HelloTimeout() time.Duration {
	return helloTimeout
}

type relayHello struct {
	Token string `json:"token"`
}

func RelayWS(app core.App, reg *relay.Registry, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()

		// Use context with deadline for hello to prevent slowloris attacks
		ctx, cancel := context.WithTimeout(r.Context(), helloTimeout)
		defer cancel()

		_, payload, err := conn.Read(ctx)
		if err != nil {
			log.Printf("relay_ws: hello read error: %v", err)
			conn.Close(websocket.StatusPolicyViolation, "hello timeout")
			return
		}

		var hello relayHello
		if json.Unmarshal(payload, &hello) != nil {
			log.Printf("relay_ws: invalid hello JSON")
			conn.Close(websocket.StatusPolicyViolation, "invalid relay hello")
			return
		}

		now := time.Now()

		// Try agent JWT first
		if claims, err := relay.VerifyAgentRelayJWT(cfg.RelayJWTSecret, hello.Token, now); err == nil {
			peer, state, bindErr := reg.BindAgentSocket(claims.SID, claims.AgentID, conn, now)
			if bindErr != nil {
				log.Printf("relay_ws: agent bind failed for sid=%s: %v", claims.SID, bindErr)
				conn.Close(websocket.StatusPolicyViolation, bindErr.Error())
				return
			}
			log.Printf("relay_ws: agent connected sid=%s agent_id=%s", claims.SID, claims.AgentID)
			if state == relay.StatePendingAgent {
				peer, bindErr = reg.WaitForBrowser(claims.SID, now)
				if bindErr != nil {
					log.Printf("relay_ws: agent wait for browser failed sid=%s: %v", claims.SID, bindErr)
					conn.Close(websocket.StatusPolicyViolation, bindErr.Error())
					return
				}
			}
			if peer != nil {
				log.Printf("relay_ws: relay pair connected sid=%s", claims.SID)
				proxyRelayPair(ctx, app, reg, claims.SID, conn, peer)
			}
			return
		}

		// Try browser JWT
		if claims, err := relay.VerifyBrowserPolicyJWT(cfg.RelayJWTSecret, hello.Token, now); err == nil {
			peer, state, bindErr := reg.BindBrowserSocket(claims.SID, claims.RegisteredClaims.ID, conn, now)
			if bindErr != nil {
				log.Printf("relay_ws: browser bind failed for sid=%s: %v", claims.SID, bindErr)
				conn.Close(websocket.StatusPolicyViolation, bindErr.Error())
				return
			}
			log.Printf("relay_ws: browser connected sid=%s jti=%s", claims.SID, claims.RegisteredClaims.ID)
			if state == relay.StatePendingBrowser {
				peer, bindErr = reg.WaitForAgent(claims.SID, now)
				if bindErr != nil {
					log.Printf("relay_ws: browser wait for agent failed sid=%s: %v", claims.SID, bindErr)
					conn.Close(websocket.StatusPolicyViolation, bindErr.Error())
					return
				}
			}
			if peer != nil {
				log.Printf("relay_ws: relay pair connected sid=%s", claims.SID)
				proxyRelayPair(ctx, app, reg, claims.SID, conn, peer)
			}
			return
		}

		log.Printf("relay_ws: invalid relay token")
		conn.Close(websocket.StatusPolicyViolation, "invalid relay token")
	}
}

func proxyRelayPair(ctx context.Context, app core.App, reg *relay.Registry, sid string, left, right *websocket.Conn) {
	var once sync.Once
	closeAndFlush := func() {
		once.Do(func() {
			left.Close(websocket.StatusNormalClosure, "")
			right.Close(websocket.StatusNormalClosure, "")
			accountID, bytes, err := reg.CloseSession(sid)
			if err == nil && bytes > 0 && app != nil {
				relay.ApplyRelayBytes(app, accountID, bytes, time.Now().UTC())
			}
			log.Printf("relay_ws: relay session closed sid=%s bytes=%d", sid, bytes)
		})
	}

	go forwardRelayFrames(ctx, reg, sid, right, left, closeAndFlush)
	forwardRelayFrames(ctx, reg, sid, left, right, closeAndFlush)
}

func forwardRelayFrames(ctx context.Context, reg *relay.Registry, sid string, src, dst *websocket.Conn, onClose func()) {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			onClose()
			return
		}
		reg.AddForwardedBytes(sid, int64(len(data)))
		if dst.Write(ctx, typ, data) != nil {
			onClose()
			return
		}
	}
}