package handler

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/coder/websocket"
	"sharebridge/server/internal/config"
)

// DebugWS provides a minimal websocket endpoint to isolate handshake/runtime
// issues from the share-session signaling flow.
func DebugWS(cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cfg.DebugWS {
			log.Printf("debug_ws: incoming method=%s path=%s remote=%s host=%s origin=%q upgrade=%q connection=%q version=%q key_present=%t extensions=%q ua=%q",
				r.Method,
				r.URL.Path,
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

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			log.Printf("debug_ws accept: %v", err)
			return
		}
		if cfg.DebugWS {
			log.Printf("debug_ws: accepted remote=%s subprotocol=%q", r.RemoteAddr, conn.Subprotocol())
		}
		defer conn.CloseNow()

		ctx := r.Context()
		if err := sendDebug(ctx, conn, map[string]any{"type": "hello", "path": r.URL.Path}); err != nil {
			if cfg.DebugWS {
				log.Printf("debug_ws: initial send failed err=%v", err)
			}
			return
		}

		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				if cfg.DebugWS {
					log.Printf("debug_ws: read ended err=%v", err)
				}
				return
			}
			if cfg.DebugWS {
				log.Printf("debug_ws: recv type=%d bytes=%d", typ, len(data))
			}

			if typ == websocket.MessageText {
				var msg any
				if err := json.Unmarshal(data, &msg); err == nil {
					if err := sendDebug(ctx, conn, map[string]any{"type": "echo", "message": msg}); err != nil {
						if cfg.DebugWS {
							log.Printf("debug_ws: echo send failed err=%v", err)
						}
						return
					}
					continue
				}
			}

			if err := conn.Write(ctx, typ, data); err != nil {
				if cfg.DebugWS {
					log.Printf("debug_ws: raw echo failed err=%v", err)
				}
				return
			}
		}
	}
}

func sendDebug(ctx context.Context, conn *websocket.Conn, msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}
