// Command gateway runs the ShareBridge public L4 relay gateway: it accepts
// browser TLS connections without terminating TLS, routes by exact
// ClientHello SNI against control-distributed in-memory routes, and splices
// the streams onto the agents' loopback FRP proxy ports (spec §4.1, §8).
//
// Task 4 wires the data plane only. The route table starts empty and without
// a presence registry, so every public connection is rejected fail-closed
// until the control sync and FRP authorization plugin tasks arrive — the
// gateway serves nothing rather than guessing (spec §8).
package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"sharebridge/relay/internal/gateway"
	"sharebridge/relay/internal/routes"
)

// envListenAddr overrides the public listen address. Deployment
// configuration arrives with the systemd units; until then this one
// environment variable is enough for development.
const envListenAddr = "SHAREBRIDGE_GATEWAY_LISTEN_ADDR"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	listenAddress := os.Getenv(envListenAddr)
	if listenAddress == "" {
		listenAddress = ":443"
	}

	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		logger.Error("gateway: listen failed", "addr", listenAddress, "error", err)
		os.Exit(1)
	}

	// The route table joins tunnel presence through the Task 14 registry;
	// until it exists a nil presence fails every lookup closed.
	routeTable := routes.NewTable(nil)
	server := gateway.NewServer(routeTable, gateway.NewStreams(), gateway.WithLogger(logger))

	serveContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- server.Serve(listener)
	}()

	logger.Info("gateway: listening", "addr", listenAddress)
	select {
	case err := <-serveErrors:
		if err != nil {
			logger.Error("gateway: accept loop failed", "error", err)
			os.Exit(1)
		}
	case <-serveContext.Done():
		logger.Info("gateway: shutting down")
		server.Close()
		server.Wait()
		logger.Info("gateway: stopped")
	}
}
