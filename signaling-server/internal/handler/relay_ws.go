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

// relayWebSocketReadLimit must exceed the encrypted relay frame cap used by
// the browser and agent (8 MiB plus framing/cipher overhead).
const relayWebSocketReadLimit = 10 * 1024 * 1024

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
		conn.SetReadLimit(relayWebSocketReadLimit)

		// Only the hello read uses a short timeout. Relay sessions themselves must
		// live on the request context, not the slowloris protection deadline.
			helloCtx, cancel := context.WithTimeout(context.Background(), helloTimeout)
			defer cancel()
			sessionCtx := context.Background()

		_, payload, err := conn.Read(helloCtx)
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
				log.Printf("relay_ws: relay pair connected sid=%s", claims.SID)
				proxyRelayPair(sessionCtx, app, reg, claims.SID, conn, peer, "agent", "browser")
				return
			}
			if state == relay.StateActive {
				done, watchErr := reg.WatchSession(claims.SID)
				if watchErr != nil {
					conn.Close(websocket.StatusPolicyViolation, watchErr.Error())
					return
				}
				<-done
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
				log.Printf("relay_ws: relay pair connected sid=%s", claims.SID)
				proxyRelayPair(sessionCtx, app, reg, claims.SID, conn, peer, "browser", "agent")
				return
			}
			if state == relay.StateActive {
				done, watchErr := reg.WatchSession(claims.SID)
				if watchErr != nil {
					conn.Close(websocket.StatusPolicyViolation, watchErr.Error())
					return
				}
				<-done
			}
			return
		}

		log.Printf("relay_ws: invalid relay token")
		conn.Close(websocket.StatusPolicyViolation, "invalid relay token")
	}
}

func proxyRelayPair(ctx context.Context, app core.App, reg *relay.Registry, sid string, left, right *websocket.Conn, leftRole, rightRole string) {
	var once sync.Once
	closeAndFlush := func(trigger string) {
		once.Do(func() {
			left.Close(websocket.StatusNormalClosure, "")
			right.Close(websocket.StatusNormalClosure, "")
			accountID, bytes, err := reg.CloseSession(sid)
			if err == nil && bytes > 0 && app != nil {
				relay.ApplyRelayBytes(app, accountID, bytes, time.Now().UTC())
			}
			log.Printf("relay_ws: relay session closed sid=%s trigger=%s bytes=%d", sid, trigger, bytes)
		})
	}

	go forwardRelayFrames(ctx, reg, sid, right, left, rightRole, leftRole, closeAndFlush)
	forwardRelayFrames(ctx, reg, sid, left, right, leftRole, rightRole, closeAndFlush)
}

func forwardRelayFrames(ctx context.Context, reg *relay.Registry, sid string, src, dst *websocket.Conn, srcRole, dstRole string, onClose func(string)) {
	for {
		typ, data, err := src.Read(ctx)
		if err != nil {
			trigger := srcRole + "->" + dstRole + " read"
			log.Printf("relay_ws: forward read failed sid=%s direction=%s->%s: %v", sid, srcRole, dstRole, err)
			onClose(trigger)
			return
		}
		reg.AddForwardedBytes(sid, int64(len(data)))
		if err := dst.Write(ctx, typ, data); err != nil {
			trigger := srcRole + "->" + dstRole + " write"
			log.Printf("relay_ws: forward write failed sid=%s direction=%s->%s type=%v bytes=%d: %v", sid, srcRole, dstRole, typ, len(data), err)
			onClose(trigger)
			return
		}
	}
}
