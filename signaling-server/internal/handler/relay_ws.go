package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/relay"
)

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

		ctx := r.Context()
		_, payload, err := conn.Read(ctx)
		if err != nil {
			return
		}

		var hello relayHello
		if json.Unmarshal(payload, &hello) != nil {
			conn.Close(websocket.StatusPolicyViolation, "invalid relay hello")
			return
		}

		now := time.Now()

		// Try agent JWT first
		if claims, err := relay.VerifyAgentRelayJWT(cfg.RelayJWTSecret, hello.Token, now); err == nil {
			peer, state, bindErr := reg.BindAgentSocket(claims.SID, claims.AgentID, conn, now)
			if bindErr != nil {
				conn.Close(websocket.StatusPolicyViolation, bindErr.Error())
				return
			}
			if state == relay.StatePendingAgent {
				peer, bindErr = reg.WaitForBrowser(claims.SID, now)
				if bindErr != nil {
					conn.Close(websocket.StatusPolicyViolation, bindErr.Error())
					return
				}
			}
			if peer != nil {
				proxyRelayPair(ctx, app, reg, claims.SID, conn, peer)
			}
			return
		}

		// Try browser JWT
		if claims, err := relay.VerifyBrowserPolicyJWT(cfg.RelayJWTSecret, hello.Token, now); err == nil {
			peer, state, bindErr := reg.BindBrowserSocket(claims.SID, claims.RegisteredClaims.ID, conn, now)
			if bindErr != nil {
				conn.Close(websocket.StatusPolicyViolation, bindErr.Error())
				return
			}
			if state == relay.StatePendingBrowser {
				peer, bindErr = reg.WaitForAgent(claims.SID, now)
				if bindErr != nil {
					conn.Close(websocket.StatusPolicyViolation, bindErr.Error())
					return
				}
			}
			if peer != nil {
				proxyRelayPair(ctx, app, reg, claims.SID, conn, peer)
			}
			return
		}

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