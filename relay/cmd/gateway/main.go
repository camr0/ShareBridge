// Command gateway runs the ShareBridge public L4 relay gateway: it accepts
// browser TLS connections without terminating TLS, routes by exact
// ClientHello SNI against control-distributed in-memory routes, and splices
// the streams onto the agents' loopback FRP proxy ports (spec §4.1, §8).
//
// The same process exposes a separately bound, loopback-only FRP authorization
// plugin. Startup fails closed unless its public verification key and
// operator-only shared authentication secret are configured.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"sharebridge/relay/internal/frpplugin"
	"sharebridge/relay/internal/gateway"
	"sharebridge/relay/internal/routes"
)

const (
	envListenAddress       = "SHAREBRIDGE_GATEWAY_LISTEN_ADDR"
	envPluginListenAddress = "SHAREBRIDGE_FRP_PLUGIN_LISTEN_ADDR"
	envPluginSharedSecret  = "SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET"
	envControlPublicKey    = "SHAREBRIDGE_CONTROL_RELAY_PUBLIC_KEY"
	envRelayPortMin        = "SHAREBRIDGE_RELAY_PORT_MIN"
	envRelayPortMax        = "SHAREBRIDGE_RELAY_PORT_MAX"

	defaultPluginListenAddress = "127.0.0.1:9001"
	defaultRelayPortMin        = 10000
	defaultRelayPortMax        = 10099
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	pluginServer, pluginListenAddress, err := configuredPluginServer()
	if err != nil {
		logger.Error("gateway: FRP authorization plugin configuration rejected", "error", err)
		os.Exit(1)
	}
	pluginListener, err := net.Listen("tcp", pluginListenAddress)
	if err != nil {
		logger.Error("gateway: FRP authorization plugin listen failed", "addr", pluginListenAddress, "error", err)
		os.Exit(1)
	}

	listenAddress := os.Getenv(envListenAddress)
	if listenAddress == "" {
		listenAddress = ":443"
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		_ = pluginListener.Close()
		logger.Error("gateway: listen failed", "addr", listenAddress, "error", err)
		os.Exit(1)
	}

	// The route table joins tunnel presence through the Task 14 registry;
	// until it exists a nil presence fails every public lookup closed. The
	// plugin already emits credential-free facts through its injected seam.
	routeTable := routes.NewTable(nil)
	server := gateway.NewServer(routeTable, gateway.NewStreams(), gateway.WithLogger(logger))
	httpPluginServer := &http.Server{
		Handler:           pluginServer,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	serveContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	gatewayErrors := make(chan error, 1)
	go func() {
		gatewayErrors <- server.Serve(listener)
	}()
	pluginErrors := make(chan error, 1)
	go func() {
		pluginErrors <- httpPluginServer.Serve(pluginListener)
	}()

	logger.Info("gateway: listening", "addr", listenAddress)
	logger.Info("gateway: FRP authorization plugin listening", "addr", pluginListenAddress)
	select {
	case serveErr := <-gatewayErrors:
		if serveErr != nil {
			logger.Error("gateway: accept loop failed", "error", serveErr)
		}
	case pluginErr := <-pluginErrors:
		if pluginErr != nil && !errors.Is(pluginErr, http.ErrServerClosed) {
			logger.Error("gateway: FRP authorization plugin failed", "error", pluginErr)
		}
	case <-serveContext.Done():
		logger.Info("gateway: shutting down")
	}

	server.Close()
	server.Wait()
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	if err := httpPluginServer.Shutdown(shutdownContext); err != nil {
		logger.Error("gateway: FRP authorization plugin shutdown failed", "error", err)
	}
	logger.Info("gateway: stopped")
}

func configuredPluginServer() (*frpplugin.Server, string, error) {
	pluginListenAddress := os.Getenv(envPluginListenAddress)
	if pluginListenAddress == "" {
		pluginListenAddress = defaultPluginListenAddress
	}
	listenHost, _, err := net.SplitHostPort(pluginListenAddress)
	if err != nil {
		return nil, "", errors.New("FRP plugin listen address must be host:port")
	}
	listenIP := net.ParseIP(listenHost)
	if listenIP == nil || !listenIP.IsLoopback() {
		return nil, "", errors.New("FRP plugin listener must use a numeric loopback address")
	}

	sharedSecret := os.Getenv(envPluginSharedSecret)
	if sharedSecret == "" {
		return nil, "", errors.New("FRP plugin shared secret is required")
	}
	publicKeyBytes, err := hex.DecodeString(os.Getenv(envControlPublicKey))
	if err != nil || len(publicKeyBytes) != ed25519.PublicKeySize {
		return nil, "", errors.New("control relay public key must be 64 hexadecimal characters")
	}
	relayPortMin, err := environmentPort(envRelayPortMin, defaultRelayPortMin)
	if err != nil {
		return nil, "", err
	}
	relayPortMax, err := environmentPort(envRelayPortMax, defaultRelayPortMax)
	if err != nil {
		return nil, "", err
	}

	pluginServer, err := frpplugin.NewServer(frpplugin.Config{
		ControlPublicKey:   ed25519.PublicKey(publicKeyBytes),
		PluginSharedSecret: sharedSecret,
		RelayPortMin:       relayPortMin,
		RelayPortMax:       relayPortMax,
	})
	if err != nil {
		return nil, "", err
	}
	return pluginServer, pluginListenAddress, nil
}

func environmentPort(name string, defaultValue int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return defaultValue, nil
	}
	port, err := strconv.Atoi(value)
	if err != nil {
		return 0, errors.New(name + " must be a decimal TCP port")
	}
	return port, nil
}
