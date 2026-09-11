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
	"sync"
	"syscall"
	"time"

	"sharebridge/relay/internal/frpplugin"
	"sharebridge/relay/internal/gateway"
	"sharebridge/relay/internal/limits"
	"sharebridge/relay/internal/metrics"
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
	// envMetricsAddress is the private-only operator endpoint (health +
	// §17.3 metrics). It must be a numeric loopback host:port; startup
	// refuses anything else (spec §17.1: metrics/health are private).
	envMetricsAddress = "SHAREBRIDGE_GATEWAY_METRICS_ADDR"

	defaultPluginListenAddress = "127.0.0.1:9001"
	defaultRelayPortMin        = 10000
	defaultRelayPortMax        = 10099
	defaultDataDirName         = ".sharebridge-relay"
	defaultMetricsAddress      = "127.0.0.1:9101"
	// logRatePerSecond / logRateBurst bound the gateway's own journal write
	// rate (§17.1 bounded log-rate expectation). Retention itself is the
	// deployment's journald SystemMaxUse/MaxRetentionSec policy (Task 35).
	logRatePerSecond = 200
	logRateBurst     = 100

	// admissionStateFilename is the FRP plugin's persisted admission replay
	// state inside the relay data directory: the burned replay-JTI set and
	// the per-agent issued-at/generation high-water, so a burned-but-
	// unexpired credential stays rejected across a gateway restart (spec
	// §7.2 ten-minute credential horizon).
	admissionStateFilename = "admission-state.json"
)

func main() {
	// The gateway's own log rate is bounded so a public-connection flood
	// cannot fill the journal; journald retention is bounded separately by
	// the Task 35 unit policy. The handler is generic and carries no
	// credential material.
	logger := slog.New(metrics.NewRateLimitedHandler(slog.NewTextHandler(os.Stderr, nil), logRatePerSecond, logRateBurst))

	// §17.3 registry and the §17.1 two-truth health surface are built before
	// any socket binds so every process exposes the same bounded signal set.
	registry := metrics.NewRegistry(metrics.Relay)
	health := gateway.NewHealth()

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
	presenceRegistry, err := newPresenceRegistry(streams, registry)
	if err != nil {
		logger.Error("gateway: presence registry configuration rejected", "error", err)
		os.Exit(1)
	}

	pluginServer, pluginListenAddress, err := configuredPluginServer(presenceRegistry, registry)
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
		gateway.WithLogger(logger), gateway.WithLimitsConfig(limitsConfig), gateway.WithMetrics(registry))
	httpPluginServer := &http.Server{
		Handler:           pluginServer,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	// Private-only operator endpoint (§17.1): loopback bind (validated) plus
	// the handler's independent loopback peer/Host guard. A public bind is
	// refused at startup rather than silently exposed.
	metricsAddress, err := metrics.BindLoopback(environmentDefault(envMetricsAddress, defaultMetricsAddress))
	if err != nil {
		_ = pluginListener.Close()
		_ = listener.Close()
		logger.Error("gateway: metrics/health address rejected", "error", err)
		os.Exit(1)
	}
	metricsListener, err := net.Listen("tcp", metricsAddress)
	if err != nil {
		_ = pluginListener.Close()
		_ = listener.Close()
		logger.Error("gateway: metrics/health listen failed", "addr", metricsAddress, "error", err)
		os.Exit(1)
	}
	metricsServer := &http.Server{
		Handler:           registry.Handler(health),
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
	metricsErrors := make(chan error, 1)
	go func() {
		metricsErrors <- metricsServer.Serve(metricsListener)
	}()

	logger.Info("gateway: listening", "addr", listenAddress)
	logger.Info("gateway: FRP authorization plugin listening", "addr", pluginListenAddress)
	logger.Info("gateway: private health/metrics listening", "addr", metricsAddress)
	select {
	case serveErr := <-gatewayErrors:
		if serveErr != nil {
			logger.Error("gateway: accept loop failed", "error", serveErr)
		}
	case pluginErr := <-pluginErrors:
		if pluginErr != nil && !errors.Is(pluginErr, http.ErrServerClosed) {
			logger.Error("gateway: FRP authorization plugin failed", "error", pluginErr)
		}
	case metricsErr := <-metricsErrors:
		if metricsErr != nil && !errors.Is(metricsErr, http.ErrServerClosed) {
			logger.Error("gateway: health/metrics server failed", "error", metricsErr)
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
	if err := metricsServer.Shutdown(shutdownContext); err != nil {
		logger.Error("gateway: health/metrics shutdown failed", "error", err)
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
// is the §15.2 drain seam, and the §17.3 metrics sink observes the presence
// transitions (online/offline counts and lease expirations). The control-sync
// Sink is not wired yet, so presence events are currently process-local; route
// snapshots are likewise not yet wired, so the route table stays empty and the
// gateway fails closed.
func newPresenceRegistry(drainer presence.AgentDrainer, registry *metrics.Registry) (*presence.Registry, error) {
	var bootID [16]byte
	if _, err := rand.Read(bootID[:]); err != nil {
		return nil, fmt.Errorf("generate gateway boot id: %w", err)
	}
	return presence.NewRegistry(presence.Config{
		BootID:  hex.EncodeToString(bootID[:]),
		Drainer: drainer,
		Sink:    &presenceMetricsSink{registry: registry},
	})
}

// presenceMetricsSink projects presence transitions onto the bounded §17.3
// tunnel signals. It runs on the presence registry's emission path (which
// holds its state lock), so it MUST NOT block: every operation is an atomic
// counter/gauge update.
type presenceMetricsSink struct {
	registry *metrics.Registry
	mu       sync.Mutex
	online   int64
	offline  int64
}

func (sink *presenceMetricsSink) ObservePresenceEvent(event presence.Event) {
	sink.mu.Lock()
	switch event.State {
	case presence.StateOnline:
		sink.online++
		if sink.offline > 0 {
			sink.offline--
		}
	case presence.StateOffline:
		if sink.online > 0 {
			sink.online--
		}
		sink.offline++
	}
	online, offline := sink.online, sink.offline
	sink.mu.Unlock()
	sink.registry.Set("sharebridge_relay_tunnel_state", online, metrics.StateOnline)
	sink.registry.Set("sharebridge_relay_tunnel_state", offline, metrics.StateOffline)
	// A lease expiry is an offline transition whose lease boundary has already
	// passed; an explicit CloseProxy carries a future lease. The presence
	// registry emits synchronously with the transition, so this is the
	// truthful classification available without coupling the metric to the
	// registry internals.
	if event.State == presence.StateOffline && !time.Now().Before(event.LeaseExpiresAt) {
		sink.registry.Inc("sharebridge_relay_presence_lease_expirations_total")
	}
}

// tunnelMetricsAdapter projects the frpplugin's reconnect signal onto the
// bounded §17.3 counter.
type tunnelMetricsAdapter struct{ registry *metrics.Registry }

func (adapter tunnelMetricsAdapter) TunnelLoginReconnect() {
	adapter.registry.Inc("sharebridge_relay_tunnel_reconnects_total")
}

func configuredPluginServer(presenceEvents frpplugin.PresenceEvents, registry *metrics.Registry) (*frpplugin.Server, string, error) {
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
		Metrics:            tunnelMetricsAdapter{registry: registry},
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

// environmentDefault returns the environment value or fallback (mirrors the
// other operator knobs; empty means "use the default").
func environmentDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
