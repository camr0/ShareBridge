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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"sharebridge/relay/internal/frpplugin"
	"sharebridge/relay/internal/gateway"
	"sharebridge/relay/internal/limits"
	"sharebridge/relay/internal/presence"
	"sharebridge/relay/internal/routes"
)

const (
	envListenAddress       = "SHAREBRIDGE_GATEWAY_LISTEN_ADDR"
	envPluginListenAddress = "SHAREBRIDGE_FRP_PLUGIN_LISTEN_ADDR"
	envPluginSharedSecret  = "SHAREBRIDGE_FRP_PLUGIN_SHARED_SECRET"
	envControlPublicKey    = "SHAREBRIDGE_CONTROL_RELAY_PUBLIC_KEY"
	envRelayPortMin        = "SHAREBRIDGE_RELAY_PORT_MIN"
	envRelayPortMax        = "SHAREBRIDGE_RELAY_PORT_MAX"
	envRelayDataDir        = "SHAREBRIDGE_RELAY_DATA_DIR"

	defaultPluginListenAddress = "127.0.0.1:9001"
	defaultRelayPortMin        = 10000
	defaultRelayPortMax        = 10099
	defaultDataDirName         = ".sharebridge-relay"

	// admissionStateFilename is the FRP plugin's persisted admission replay
	// state inside the relay data directory: the burned replay-JTI set and
	// the per-agent issued-at/generation high-water, so a burned-but-
	// unexpired credential stays rejected across a gateway restart (spec
	// §7.2 ten-minute credential horizon).
	admissionStateFilename = "admission-state.json"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Resolve every §14 operator bound before anything binds a socket: an
	// invalid or over-ceiling value refuses startup instead of running with a
	// silently wrong limit. The configuration drives both the resource
	// limiter and the ClientHello parser bounds.
	limitsConfig, err := configuredLimits()
	if err != nil {
		logger.Error("gateway: §14 limits configuration rejected", "error", err)
		os.Exit(1)
	}

	// The gateway streams registry is both the connection accounting used by
	// the resource limiter and the §15.2 drain seam for the presence registry:
	// an frps session reset clears presence and then closes the affected
	// agent's established streams through it.
	streams := gateway.NewStreams()
	presenceRegistry, err := newPresenceRegistry(streams)
	if err != nil {
		logger.Error("gateway: presence registry configuration rejected", "error", err)
		os.Exit(1)
	}

	pluginServer, pluginListenAddress, err := configuredPluginServer(presenceRegistry)
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

	// The route table joins tunnel presence through the registry the plugin
	// feeds: a route with no probe-confirmed online presence fails closed, and
	// a control snapshot has not yet been wired, so the table stays empty and
	// every public lookup still fails closed.
	routeTable := routes.NewTable(presenceRegistry)
	server := gateway.NewServer(routeTable, streams,
		gateway.WithLogger(logger), gateway.WithLimitsConfig(limitsConfig))
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

// configuredLimits resolves the §14 operator bounds from the process
// environment. It is the gateway binary's single production configuration
// entry point; an invalid or over-ceiling value is returned as an error so
// main can refuse to start.
func configuredLimits() (limits.Config, error) {
	return limits.ConfigFromEnvironment(os.LookupEnv)
}

// newPresenceRegistry builds the production presence registry the FRP plugin
// feeds and the route table joins against. The boot identity is a fresh
// 128-bit hex value per process: §15.1 requires a gateway restart to change
// the boot ID so control discards stale presence events. The streams registry
// is the §15.2 drain seam. The control-sync Sink is not wired yet, so presence
// events are currently process-local; route snapshots are likewise not yet
// wired, so the route table stays empty and the gateway fails closed.
func newPresenceRegistry(drainer presence.AgentDrainer) (*presence.Registry, error) {
	var bootID [16]byte
	if _, err := rand.Read(bootID[:]); err != nil {
		return nil, fmt.Errorf("generate gateway boot id: %w", err)
	}
	return presence.NewRegistry(presence.Config{
		BootID:  hex.EncodeToString(bootID[:]),
		Drainer: drainer,
	})
}

func configuredPluginServer(presenceEvents frpplugin.PresenceEvents) (*frpplugin.Server, string, error) {
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
	statePath, err := resolveAdmissionStatePath()
	if err != nil {
		return nil, "", err
	}

	pluginServer, err := frpplugin.NewServer(frpplugin.Config{
		ControlPublicKey:   ed25519.PublicKey(publicKeyBytes),
		PluginSharedSecret: sharedSecret,
		RelayPortMin:       relayPortMin,
		RelayPortMax:       relayPortMax,
		StatePath:          statePath,
		PresenceEvents:     presenceEvents,
	})
	if err != nil {
		return nil, "", err
	}
	return pluginServer, pluginListenAddress, nil
}

// resolveAdmissionStatePath resolves the FRP plugin's persisted admission
// state path from SHAREBRIDGE_RELAY_DATA_DIR (default ~/.sharebridge-relay,
// mirroring the agent's SHAREBRIDGE_DATA_DIR convention) and creates the
// directory owner-only when missing. Startup fails closed when the data
// directory is unavailable: the persisted replay state is what keeps a
// burned credential rejected across restarts, so the gateway must not run
// without it.
func resolveAdmissionStatePath() (string, error) {
	dataDir := os.Getenv(envRelayDataDir)
	if dataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", errors.New("relay data directory unavailable: set " + envRelayDataDir)
		}
		dataDir = filepath.Join(home, defaultDataDirName)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("relay data directory %q unavailable: %w", dataDir, err)
	}
	return filepath.Join(dataDir, admissionStateFilename), nil
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
