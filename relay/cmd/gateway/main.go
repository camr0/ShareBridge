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
	"strings"
	"sync"
	"syscall"
	"time"

	"sharebridge/relay/internal/controlsync"
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
	// Control↔gateway §11.3 sync configuration. All five values are
	// required together; a partial configuration refuses startup. When none
	// is set the gateway runs without control sync (the dark/enable-relay
	// posture) and route readiness stays fail-closed false.
	envControlSyncURL      = "SHAREBRIDGE_CONTROL_SYNC_URL"
	envControlSyncSAN      = "SHAREBRIDGE_CONTROL_SYNC_SAN"
	envControlSyncCAFile   = "SHAREBRIDGE_CONTROL_SYNC_CA_FILE"
	envGatewaySyncCertFile = "SHAREBRIDGE_GATEWAY_SYNC_CERT_FILE"
	envGatewaySyncKeyFile  = "SHAREBRIDGE_GATEWAY_SYNC_KEY_FILE"
	// envGatewayNamespace is this gateway's §6 namespace ("sb" plus eight
	// lowercase hex characters); required with the sync configuration.
	envGatewayNamespace = "SHAREBRIDGE_GATEWAY_NAMESPACE"
	// NIC saturation collector configuration (§17.3 bullet 5). The ratio is
	// only reported when both the interface and a positive capacity are set;
	// otherwise the gauge renders NaN (unavailable) rather than a fake 0.
	envNICInterface       = "SHAREBRIDGE_GATEWAY_NIC_INTERFACE"
	envNICCapacityBytes   = "SHAREBRIDGE_GATEWAY_NIC_CAPACITY_BYTES_PER_SEC"
	procNetDevPath        = "/proc/net/dev"
	defaultNICSampleEvery = 15 * time.Second

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
	// tunnelRestoration anchors an frps restart (the §15.2 SessionReset
	// lifecycle fact) until the affected tunnel is confirm-online again, so
	// the §17.3 tunnel-restoration histogram measures a real
	// restart→restored interval rather than an offline→online guess.
	restoration := newTunnelRestorationTracker(time.Now)
	presenceRegistry, err := newPresenceRegistry(streams, registry, restoration)
	if err != nil {
		logger.Error("gateway: presence registry configuration rejected", "error", err)
		os.Exit(1)
	}
	// The route table joins tunnel presence through the registry the plugin
	// feeds: a route with no probe-confirmed online presence fails closed.
	// It is built before the plugin so the control-sync applier can own its
	// writes once sync is configured.
	routeTable := routes.NewTable(presenceRegistry)

	// frps process health is an independent §17.1 truth: any authenticated
	// frps plugin call proves frps is driving the plugin boundary, and a
	// §15.2 SessionReset proves frps restarted. The truth never gates route
	// readiness.
	pluginPresenceEvents := frpsLifecycleEvents{next: presenceRegistry, health: health, restoration: restoration}
	pluginServer, pluginListenAddress, err := configuredPluginServer(pluginPresenceEvents, registry)
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

	// Control sync loop (§17.1 snapshot/control-sync truth). It is built
	// fail-closed from the operator environment and, when configured, is the
	// only production caller of gateway.Health's snapshot/control-sync
	// setters. Without it the gateway correctly reports route_ready=false.
	syncLoop, syncEnabled, err := configuredSyncLoop(health, registry, routeTable, streams, presenceRegistry.BootID(), logger)
	if err != nil {
		_ = pluginListener.Close()
		_ = listener.Close()
		_ = metricsListener.Close()
		logger.Error("gateway: control sync configuration rejected", "error", err)
		os.Exit(1)
	}
	if syncEnabled {
		go syncLoop.Run(serveContext)
		logger.Info("gateway: control sync loop started")
	} else {
		logger.Warn("gateway: control sync not configured; route readiness stays fail-closed")
	}

	// Bounded NIC saturation collector (§17.3 bullet 5): exactly one sampler
	// goroutine, no per-connection work. On a host without usable stats the
	// gauge reports unavailable (NaN).
	nicSampler, err := metrics.NewSampler(registry, "sharebridge_relay_gateway_nic_saturation_ratio", configuredNICSource(), defaultNICSampleEvery)
	if err != nil {
		logger.Error("gateway: NIC sampler configuration rejected", "error", err)
		os.Exit(1)
	}
	go nicSampler.Run(serveContext)

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

// configuredSyncLoop builds the §11.3 control-sync loop from the operator
// environment. It returns enabled=false when NO sync variable is set (the
// dark/enable-relay posture: the gateway runs, route readiness is fail-closed
// false, and the operator sees a warning). A PARTIAL configuration is a
// startup error — a gateway must never silently run with a half-configured
// control channel. The loop is the production writer of the §17.1
// snapshot/control-sync health truths.
func configuredSyncLoop(health *gateway.Health, registry *metrics.Registry, table *routes.Table, streams *gateway.Streams, bootID string, logger *slog.Logger) (*controlsync.Loop, bool, error) {
	values := []string{
		os.Getenv(envControlSyncURL),
		os.Getenv(envControlSyncSAN),
		os.Getenv(envControlSyncCAFile),
		os.Getenv(envGatewaySyncCertFile),
		os.Getenv(envGatewaySyncKeyFile),
		os.Getenv(envGatewayNamespace),
	}
	configured := false
	for _, value := range values {
		if value != "" {
			configured = true
			break
		}
	}
	if !configured {
		return nil, false, nil
	}
	for _, value := range values {
		if value == "" {
			return nil, false, fmt.Errorf("control sync requires %s, %s, %s, %s, %s and %s together",
				envControlSyncURL, envControlSyncSAN, envControlSyncCAFile, envGatewaySyncCertFile, envGatewaySyncKeyFile, envGatewayNamespace)
		}
	}
	serverCAPEM, err := os.ReadFile(os.Getenv(envControlSyncCAFile))
	if err != nil {
		return nil, false, fmt.Errorf("control sync CA file: %w", err)
	}
	clientCertPEM, err := os.ReadFile(os.Getenv(envGatewaySyncCertFile))
	if err != nil {
		return nil, false, fmt.Errorf("gateway sync certificate file: %w", err)
	}
	clientKeyPEM, err := os.ReadFile(os.Getenv(envGatewaySyncKeyFile))
	if err != nil {
		return nil, false, fmt.Errorf("gateway sync key file: %w", err)
	}
	client, err := controlsync.NewClient(controlsync.ClientConfig{
		BaseURL:           os.Getenv(envControlSyncURL),
		ExpectedServerSAN: os.Getenv(envControlSyncSAN),
		ServerCAPEM:       serverCAPEM,
		ClientCertPEM:     clientCertPEM,
		ClientKeyPEM:      clientKeyPEM,
		Metrics:           registry,
	})
	if err != nil {
		return nil, false, err
	}
	applier, err := controlsync.NewApplier(controlsync.ApplierConfig{
		Client:    client,
		Table:     table,
		Streams:   streams,
		Namespace: os.Getenv(envGatewayNamespace),
		BootID:    bootID,
		Logger:    logger,
		Metrics:   registry,
		Clock:     time.Now,
	})
	if err != nil {
		return nil, false, err
	}
	loop, err := controlsync.NewLoop(controlsync.LoopConfig{
		Applier: applier,
		Health:  health,
		Logger:  logger,
	})
	if err != nil {
		return nil, false, err
	}
	return loop, true, nil
}

// configuredNICSource builds the §17.3 bullet 5 NIC saturation source from the
// operator environment. Without a configured interface and a positive link
// capacity the source reports the value unavailable (the gauge renders NaN);
// it never fabricates a ratio.
func configuredNICSource() metrics.NICSource {
	capacity, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(envNICCapacityBytes)), 64)
	if err != nil {
		capacity = 0
	}
	return metrics.ProcNetDevSource(procNetDevPath, os.Getenv(envNICInterface), capacity, time.Now)
}

// newPresenceRegistry builds the production presence registry the FRP plugin
// feeds and the route table joins against. The boot identity is a fresh
// 128-bit hex value per process: §15.1 requires a gateway restart to change
// the boot ID so control discards stale presence events. The streams registry
// is the §15.2 drain seam, and the §17.3 metrics sink observes the presence
// transitions (online/offline counts, lease expirations, and restart→restored
// latency). The control-sync Sink is wired separately once sync is configured.
func newPresenceRegistry(drainer presence.AgentDrainer, registry *metrics.Registry, restoration *tunnelRestorationTracker) (*presence.Registry, error) {
	var bootID [16]byte
	if _, err := rand.Read(bootID[:]); err != nil {
		return nil, fmt.Errorf("generate gateway boot id: %w", err)
	}
	return presence.NewRegistry(presence.Config{
		BootID:  hex.EncodeToString(bootID[:]),
		Drainer: drainer,
		Sink:    &presenceMetricsSink{registry: registry, restoration: restoration, now: time.Now},
	})
}

// tunnelRestorationTracker anchors the frps-restart moment for an agent (the
// §15.2 SessionReset lifecycle fact) until that agent's tunnel is
// confirm-online again. It is the production input to §17.3 bullet 10 (time
// from frps restart to tunnel restoration): an anchor is only ever created by
// a real restart fact, so an ordinary offline→online transition is never
// misreported as a restart restoration.
type tunnelRestorationTracker struct {
	mu         sync.Mutex
	anchoredAt map[string]time.Time
	now        func() time.Time
}

func newTunnelRestorationTracker(now func() time.Time) *tunnelRestorationTracker {
	if now == nil {
		now = time.Now
	}
	return &tunnelRestorationTracker{anchoredAt: make(map[string]time.Time), now: now}
}

// noteRestart records (or refreshes) the restart anchor for an agent.
func (tracker *tunnelRestorationTracker) noteRestart(agentRecordID string) {
	if tracker == nil || agentRecordID == "" {
		return
	}
	tracker.mu.Lock()
	tracker.anchoredAt[agentRecordID] = tracker.now()
	tracker.mu.Unlock()
}

// takeElapsed consumes an agent's restart anchor and returns the seconds from
// restart to now. ok is false when the agent has no pending restart anchor, so
// nothing is recorded for a first-ever or ordinary online transition.
func (tracker *tunnelRestorationTracker) takeElapsed(agentRecordID string) (float64, bool) {
	if tracker == nil {
		return 0, false
	}
	tracker.mu.Lock()
	anchoredAt, ok := tracker.anchoredAt[agentRecordID]
	if ok {
		delete(tracker.anchoredAt, agentRecordID)
	}
	tracker.mu.Unlock()
	if !ok {
		return 0, false
	}
	seconds := tracker.now().Sub(anchoredAt).Seconds()
	if seconds < 0 {
		seconds = 0
	}
	return seconds, true
}

// frpsLifecycleEvents is the production frps/plugin lifecycle view. Any
// authenticated plugin fact proves frps is driving the plugin boundary, so it
// marks the independent §17.1 frps process truth healthy; a §15.2 SessionReset
// (a burned one-use credential re-presented) additionally proves frps
// restarted and anchors the §17.3 restoration latency. It never touches route
// readiness.
type frpsLifecycleEvents struct {
	next        frpplugin.PresenceEvents
	health      *gateway.Health
	restoration *tunnelRestorationTracker
}

func (events frpsLifecycleEvents) ObserveFRPEvent(fact frpplugin.PresenceFact) {
	if events.health != nil {
		events.health.SetFRPSHealthy(true)
	}
	if fact.Operation == frpplugin.OperationSessionReset && events.restoration != nil {
		events.restoration.noteRestart(fact.AgentRecordID)
	}
	if events.next != nil {
		events.next.ObserveFRPEvent(fact)
	}
}

// presenceMetricsSink projects presence transitions onto the bounded §17.3
// tunnel signals. It runs on the presence registry's emission path (which
// holds its state lock), so it MUST NOT block: every operation is an atomic
// counter/gauge update.
type presenceMetricsSink struct {
	registry    *metrics.Registry
	restoration *tunnelRestorationTracker
	now         func() time.Time
	mu          sync.Mutex
	online      int64
	offline     int64
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
	if event.State == presence.StateOffline && !sink.now().Before(event.LeaseExpiresAt) {
		sink.registry.Inc("sharebridge_relay_presence_lease_expirations_total")
	}
	// Restart→restored latency is recorded only when a real restart anchor
	// exists for this agent; an ordinary online transition records nothing.
	if event.State == presence.StateOnline {
		if seconds, ok := sink.restoration.takeElapsed(event.AgentRecordID); ok {
			sink.registry.Observe("sharebridge_relay_tunnel_restoration_seconds", seconds)
		}
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
