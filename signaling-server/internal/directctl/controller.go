package directctl

import (
	"context"

	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/certcoordinator"
	"sharebridge/server/internal/ddns"
	"sharebridge/server/internal/hub"
)

// Config holds controller configuration. Field set is finalized by Task 10.
type Config struct {
	BaseDomain string
}

// Controller is the direct-mode control-plane handler.
//
// NOTE: this is a minimal skeleton created by Task 8 so the shared test
// fixtures compile. Task 10 fills in the full field set (epochs, waiters,
// seq, verified, ackTimeout, ddnsFn, sendToAgentFn, ...) and the real
// NewController wiring.
type Controller struct {
	app   core.App
	hub   *hub.Hub
	coord *certcoordinator.Coordinator
	ddns  *ddns.Cloudflare
	cfg   Config

	sendFn       func(ctx context.Context, conn *websocket.Conn, msg any) error
	allowPrivate bool
}

// NewController constructs a Controller. It is a stub for now; Task 10 fills
// in the remaining defaults.
func NewController(app core.App, h *hub.Hub, coord *certcoordinator.Coordinator, dnsClient *ddns.Cloudflare, cfg Config) *Controller {
	return &Controller{
		app:   app,
		hub:   h,
		coord: coord,
		ddns:  dnsClient,
		cfg:   cfg,
		sendFn: func(ctx context.Context, conn *websocket.Conn, msg any) error {
			return hub.SendDirect(ctx, conn, msg)
		},
	}
}
