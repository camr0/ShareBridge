// Package metrics implements the relay's §17.3 metadata-only metric surface
// and the private operator endpoint that exposes it. It is deliberately
// dependency-free (stdlib only): the relay module carries no Prometheus
// client, and the §17.3 signal set is small and fully enumerated.
//
// Two invariants drive the whole package:
//
//  1. Bounded cardinality. Every label value is a compile-time constant from
//     this file. A caller can only ever address a pre-registered series; an
//     unknown metric name or an unknown label value is dropped without
//     allocating a series (see Registry.Inc/Add/Set/Observe). Nothing derived
//     from a share code, hostname, source IP, credential, JTI, or issued-at is
//     ever a label, so the label space cannot grow with traffic (spec §16.6,
//     §17.3).
//
//  2. No plaintext. The metric set carries counts, durations, and bounded
//     policy labels only — no route lists, no payload, no credential material,
//     no buffered ClientHello bytes.
//
// The package also provides the private-only HTTP guard (Handler), the
// truthful two-truth health surface contract (HealthReporter), and a bounded
// log-rate handler (NewRateLimitedHandler) implementing the §17.1 bounded
// log-rate expectation.
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Kind is the §17.3 metric kind.
type Kind uint8

const (
	// Counter is a monotonic total.
	Counter Kind = iota
	// Gauge is a last-value signal (settable or scraped from a source func).
	Gauge
	// Histogram is a bucketed distribution of seconds.
	Histogram
)

// Bounded label values. Every value below is a constant; no caller-supplied
// string is ever accepted as a new series.
const (
	// Tunnel lifecycle (§17.3 bullet 1).
	StateOnline        = "online"
	StateOffline       = "offline"
	TunnelReconnecting = "reconnecting"

	// Connection outcome and rejection reasons (§17.3 bullet 3).
	OutcomeAccepted = "accepted"
	OutcomeRejected = "rejected"

	ReasonLimits      = "limits"
	ReasonClientHello = "clienthello"
	ReasonRoute       = "route"
	ReasonDial        = "dial"
	ReasonAdmission   = "admission"
	ReasonReplay      = "replay"

	// ClientHello parse classes (§17.3 bullet 6).
	HelloTimeout   = "timeout"
	HelloOversize  = "oversize"
	HelloMalformed = "malformed"

	// Bounded accounting scopes (§17.3 bullets 4 and 5). Scope selects the
	// aggregation dimension; the values are never identifiers.
	ScopeGlobal   = "global"
	ScopeSourceIP = "source_ip"
	ScopeOrigin   = "origin"
	ScopeAgent    = "agent"

	// Direct preparation outcome (§17.3 bullet 8).
	PrepareDirect      = "direct"
	PrepareRelay       = "relay"
	PrepareUnavailable = "unavailable"

	// STUN exchange outcome (§17.3 bullet 9).
	STUNMatch    = "match"
	STUNMismatch = "mismatch"
	STUNTimeout  = "timeout"
)

// Process identifies which binary owns a §17.3 signal. Relay signals are
// emitted by the gateway process; Control signals by control. They are
// declared together so the inventory is one reviewable list, but each process
// registers only its own subset.
type Process uint8

const (
	// Relay signals are emitted by the gateway.
	Relay Process = iota
	// Control signals are emitted by control's route-selection path.
	Control
)

// Signal is one canonical §17.3 signal definition. Label is empty for an
// unlabeled signal; otherwise Values is the complete, enumerated label-value
// set (bounded cardinality by construction).
type Signal struct {
	Name   string
	Kind   Kind
	Help   string
	Label  string
	Values []string
	Spec   string
	Owner  Process
}

// RequiredSignals is the exact §17.3 counters/gauges/histograms inventory.
// Each entry maps to one §17.3 bullet (recorded in Spec). The TestMetrics
// test asserts this list covers every bullet and that every labeled signal
// carries an enumerated, non-empty value set.
var RequiredSignals = []Signal{
	// Bullet 1: tunnels online, offline, reconnecting, presence-lease
	// expirations.
	{Name: "sharebridge_relay_tunnel_state", Kind: Gauge,
		Help:  "Current tunnel presence state (truthful presence-registry transitions).",
		Label: "state", Values: []string{StateOnline, StateOffline}, Spec: "17.3#1", Owner: Relay},
	{Name: "sharebridge_relay_tunnel_reconnects_total", Kind: Counter,
		Help: "Tunnel sessions established after a prior session for the same agent (reconnects).",
		Spec: "17.3#1", Owner: Relay},
	{Name: "sharebridge_relay_presence_lease_expirations_total", Kind: Counter,
		Help: "Presence leases that expired without an explicit close or renewal.",
		Spec: "17.3#1", Owner: Relay},

	// Bullet 2: route snapshot/delta revision and propagation lag.
	{Name: "sharebridge_relay_route_snapshot_revision", Kind: Gauge,
		Help: "Revision of the most recently applied route snapshot.", Spec: "17.3#2", Owner: Relay},
	{Name: "sharebridge_relay_route_delta_revision", Kind: Gauge,
		Help: "Revision of the most recently applied delta page.", Spec: "17.3#2", Owner: Relay},
	{Name: "sharebridge_relay_route_propagation_lag_seconds", Kind: Histogram,
		Help: "Control-to-gateway route revision propagation lag.", Spec: "17.3#2", Owner: Relay},

	// Bullet 3: public connections accepted/rejected by reason.
	{Name: "sharebridge_relay_public_connections_total", Kind: Counter,
		Help:  "Public connections admitted or generically closed.",
		Label: "outcome", Values: []string{OutcomeAccepted, OutcomeRejected}, Spec: "17.3#3", Owner: Relay},
	{Name: "sharebridge_relay_public_connection_rejections_total", Kind: Counter,
		Help:  "Public connections closed generically, by bounded reason class.",
		Label: "reason", Values: []string{ReasonLimits, ReasonClientHello, ReasonRoute, ReasonDial, ReasonAdmission, ReasonReplay}, Spec: "17.3#3", Owner: Relay},

	// Bullet 4: concurrent connections. The per-key breakdown is expressed as
	// bounded aggregate scopes, never as per-IP/per-hostname labels.
	{Name: "sharebridge_relay_concurrent_connections", Kind: Gauge,
		Help:  "Concurrent public connections (scope=global) and distinct tracked limit entities by scope.",
		Label: "scope", Values: []string{ScopeGlobal, ScopeSourceIP, ScopeOrigin, ScopeAgent}, Spec: "17.3#4", Owner: Relay},

	// Bullet 5: bytes by agent/origin and gateway NIC saturation.
	{Name: "sharebridge_relay_relayed_bytes_total", Kind: Counter,
		Help:  "Relayed bytes under the selected bounded accounting scope.",
		Label: "scope", Values: []string{ScopeGlobal, ScopeAgent, ScopeOrigin}, Spec: "17.3#5", Owner: Relay},
	{Name: "sharebridge_relay_gateway_nic_saturation_ratio", Kind: Gauge,
		Help: "Gateway NIC saturation ratio in [0,1].", Spec: "17.3#5", Owner: Relay},

	// Bullet 6: ClientHello parse timeout/oversize/malformed counts.
	{Name: "sharebridge_relay_clienthello_total", Kind: Counter,
		Help:  "ClientHello parse outcomes by bounded class.",
		Label: "reason", Values: []string{HelloTimeout, HelloOversize, HelloMalformed}, Spec: "17.3#6", Owner: Relay},

	// Bullet 7: gateway-to-FRP connect latency/failure.
	{Name: "sharebridge_relay_frp_connect_seconds", Kind: Histogram,
		Help: "Gateway-to-FRP loopback connect latency.", Spec: "17.3#7", Owner: Relay},
	{Name: "sharebridge_relay_frp_connect_failures_total", Kind: Counter,
		Help: "Gateway-to-FRP loopback connect failures.", Spec: "17.3#7", Owner: Relay},

	// Bullet 8: direct-preparation and browser-fallback rates.
	{Name: "sharebridge_relay_direct_preparation_total", Kind: Counter,
		Help:  "Direct-preparation outcomes by bounded result class.",
		Label: "outcome", Values: []string{PrepareDirect, PrepareRelay, PrepareUnavailable}, Spec: "17.3#8", Owner: Control},
	{Name: "sharebridge_relay_browser_fallback_total", Kind: Counter,
		Help: "Preparations that resolved to the relay origin (browser fallback decision).", Spec: "17.3#8", Owner: Control},

	// Bullet 9: STUN match/mismatch/timeout counts.
	{Name: "sharebridge_relay_stun_total", Kind: Counter,
		Help:  "STUN observation outcomes by bounded class.",
		Label: "outcome", Values: []string{STUNMatch, STUNMismatch, STUNTimeout}, Spec: "17.3#9", Owner: Control},

	// Bullet 10: time from gateway/frps restart to tunnel restoration.
	{Name: "sharebridge_relay_tunnel_restoration_seconds", Kind: Histogram,
		Help: "Time from a gateway or frps restart to tunnel restoration.", Spec: "17.3#10", Owner: Relay},

	// Bullet 11: route revocation to active-stream close latency.
	{Name: "sharebridge_relay_revocation_close_seconds", Kind: Histogram,
		Help: "Latency from route revocation to active-stream close.", Spec: "17.3#11", Owner: Relay},
}

// histogramBuckets are the fixed latency buckets (seconds). Fixed buckets keep
// the series count bounded regardless of observed values.
var histogramBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

type histogram struct {
	buckets []atomic.Int64
	sum     atomic.Int64 // microseconds, to keep integer atomics
	count   atomic.Int64
}

func newHistogram() *histogram {
	return &histogram{buckets: make([]atomic.Int64, len(histogramBuckets))}
}

func (h *histogram) observe(seconds float64) {
	if seconds < 0 {
		seconds = 0
	}
	h.count.Add(1)
	h.sum.Add(int64(seconds * 1e6))
	for i, bound := range histogramBuckets {
		if seconds <= bound {
			h.buckets[i].Add(1)
			return
		}
	}
}

type series struct {
	def        Signal
	scalar     atomic.Int64
	labeled    map[string]*atomic.Int64
	collectors map[string]func() int64
	// ratios holds scrape-time floating-point collectors for ratio gauges
	// (NIC saturation). A ratio collector reports ok=false when the host
	// exposes no usable statistics; Render then emits NaN ("unavailable")
	// rather than a fabricated 0 or 1.
	ratios    map[string]func() (float64, bool)
	hist      *histogram
	histLabel map[string]*histogram
}

// Registry holds the bounded §17.3 signal set. It is safe for concurrent use
// on the datapath: increments are lock-free once a series exists, and the
// only lock is the render-time snapshot.
type Registry struct {
	mu      sync.Mutex
	signals map[string]*series
	order   []string
}

// NewRegistry returns a registry pre-populated with every §17.3 signal owned
// by owner. Relay registries are built by the gateway process; control builds
// its own registry with owner=Control (the control module cannot import this
// package, so directctl exposes an equivalent bounded counter set).
func NewRegistry(owner Process) *Registry {
	registry := &Registry{signals: make(map[string]*series)}
	for _, def := range RequiredSignals {
		if def.Owner != owner {
			continue
		}
		registry.register(def)
	}
	return registry
}

func (r *Registry) register(def Signal) {
	entry := &series{def: def, collectors: make(map[string]func() int64), ratios: make(map[string]func() (float64, bool))}
	if def.Kind == Histogram {
		if def.Label == "" {
			entry.hist = newHistogram()
		} else {
			entry.histLabel = make(map[string]*histogram)
			for _, value := range def.Values {
				entry.histLabel[value] = newHistogram()
			}
		}
	} else if def.Label != "" {
		entry.labeled = make(map[string]*atomic.Int64, len(def.Values))
		for _, value := range def.Values {
			entry.labeled[value] = new(atomic.Int64)
		}
	}
	r.signals[def.Name] = entry
	r.order = append(r.order, def.Name)
}

// series resolves a bounded label value to its pre-registered series. The
// second result is false for an unknown metric name, an unknown label value,
// or a wrong label arity; callers drop the observation (never allocate).
func (r *Registry) series(name string, label []string) (*series, *atomic.Int64, bool) {
	entry, ok := r.signals[name]
	if !ok {
		return nil, nil, false
	}
	if entry.def.Label == "" {
		if len(label) != 0 {
			return nil, nil, false
		}
		return entry, &entry.scalar, true
	}
	if len(label) != 1 {
		return nil, nil, false
	}
	cell, ok := entry.labeled[label[0]]
	if !ok {
		return nil, nil, false
	}
	return entry, cell, true
}

// Inc adds one to a counter or gauge series.
func (r *Registry) Inc(name string, label ...string) { r.Add(name, 1, label...) }

// Add adds delta to a counter or gauge series.
func (r *Registry) Add(name string, delta int64, label ...string) {
	if _, cell, ok := r.series(name, label); ok && cell != nil {
		cell.Add(delta)
	}
}

// Set stores value on a gauge series.
func (r *Registry) Set(name string, value int64, label ...string) {
	if _, cell, ok := r.series(name, label); ok && cell != nil {
		cell.Store(value)
	}
}

// SetFunc installs a scrape-time collector for a gauge series. The collector
// runs on render; a nil collector clears the source.
func (r *Registry) SetFunc(name string, fn func() int64, label ...string) {
	entry, _, ok := r.series(name, label)
	if !ok {
		return
	}
	key := ""
	if entry.def.Label != "" {
		key = label[0]
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if fn == nil {
		delete(entry.collectors, key)
		return
	}
	entry.collectors[key] = fn
}

// SetRatioFunc installs a scrape-time floating-point collector for a ratio
// gauge series (a value in [0,1]). The collector runs on render; a nil
// collector clears the source. When the collector reports ok=false the series
// renders NaN, the conventional "no data" value — a ratio gauge must report
// unavailable rather than fabricate a plausible-looking 0 (spec §17.3 bullet 5:
// NIC saturation is only meaningful when the host actually exposes stats).
func (r *Registry) SetRatioFunc(name string, fn func() (float64, bool), label ...string) {
	entry, _, ok := r.series(name, label)
	if !ok {
		return
	}
	key := ""
	if entry.def.Label != "" {
		key = label[0]
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if fn == nil {
		delete(entry.ratios, key)
		return
	}
	entry.ratios[key] = fn
}

// Ratio reads a ratio gauge's current collector value without rendering. The
// second result is false when the series carries no ratio collector or the
// source reports the value unavailable. Used by tests and internal
// assertions.
func (r *Registry) Ratio(name string, label ...string) (float64, bool) {
	entry, _, ok := r.series(name, label)
	if !ok {
		return 0, false
	}
	key := ""
	if entry.def.Label != "" {
		key = label[0]
	}
	r.mu.Lock()
	fn := entry.ratios[key]
	r.mu.Unlock()
	if fn == nil {
		return 0, false
	}
	return fn()
}

// Observe records one seconds value in a histogram series.
func (r *Registry) Observe(name string, seconds float64, label ...string) {
	entry, _, ok := r.series(name, label)
	if !ok {
		return
	}
	if entry.def.Kind != Histogram {
		return
	}
	if entry.def.Label == "" {
		if entry.hist != nil {
			entry.hist.observe(seconds)
		}
		return
	}
	if h := entry.histLabel[label[0]]; h != nil {
		h.observe(seconds)
	}
}

// Value reads a series' current value (0 for unknown series). Gauge collectors
// are not invoked here; used by tests and internal assertions.
func (r *Registry) Value(name string, label ...string) int64 {
	_, cell, ok := r.series(name, label)
	if !ok || cell == nil {
		return 0
	}
	return cell.Load()
}

// SeriesCount reports the number of registered metric names. It is the
// cardinality witness: the value is fixed at registry construction and cannot
// grow with traffic.
func (r *Registry) SeriesCount() int { return len(r.signals) }

// LabelValues returns the enumerated values for a labeled signal (nil for an
// unlabeled or unknown signal).
func (r *Registry) LabelValues(name string) []string {
	entry, ok := r.signals[name]
	if !ok {
		return nil
	}
	return append([]string(nil), entry.def.Values...)
}

// Render writes the Prometheus text exposition of the registry. All labels are
// bounded enum values; no plaintext is emitted.
func (r *Registry) Render() string {
	// SetFunc mutates the collector map; serialize rendering against it. The
	// datapath counters remain lock-free atomics, so scrape locking is cheap
	// and infrequent.
	r.mu.Lock()
	defer r.mu.Unlock()
	var builder strings.Builder
	for _, name := range r.order {
		entry := r.signals[name]
		fmt.Fprintf(&builder, "# HELP %s %s\n", name, entry.def.Help)
		fmt.Fprintf(&builder, "# TYPE %s %s\n", name, kindName(entry.def.Kind))
		switch {
		case entry.def.Kind == Histogram && entry.def.Label == "":
			renderHistogram(&builder, name, entry.def.Label, "", entry.hist)
		case entry.def.Kind == Histogram:
			for _, value := range entry.def.Values {
				renderHistogram(&builder, name, entry.def.Label, value, entry.histLabel[value])
			}
		case entry.def.Label == "":
			if fn := entry.ratios[""]; fn != nil {
				fmt.Fprintf(&builder, "%s %s\n", name, renderRatio(fn))
				break
			}
			value := entry.scalar.Load()
			if fn := entry.collectors[""]; fn != nil {
				value = fn()
			}
			fmt.Fprintf(&builder, "%s %d\n", name, value)
		default:
			for _, value := range entry.def.Values {
				if fn := entry.ratios[value]; fn != nil {
					fmt.Fprintf(&builder, "%s{%s=%q} %s\n", name, entry.def.Label, value, renderRatio(fn))
					continue
				}
				cell := entry.labeled[value]
				val := cell.Load()
				if fn := entry.collectors[value]; fn != nil {
					val = fn()
				}
				fmt.Fprintf(&builder, "%s{%s=%q} %d\n", name, entry.def.Label, value, val)
			}
		}
	}
	return builder.String()
}

// renderRatio formats one ratio-collector reading for the Prometheus text
// exposition. An unavailable reading renders as NaN ("no data") rather than
// a fabricated number.
func renderRatio(fn func() (float64, bool)) string {
	value, ok := fn()
	if !ok {
		return "NaN"
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func kindName(kind Kind) string {
	switch kind {
	case Counter:
		return "counter"
	case Gauge:
		return "gauge"
	case Histogram:
		return "histogram"
	default:
		return "untyped"
	}
}

func renderHistogram(builder *strings.Builder, name, labelName, labelValue string, h *histogram) {
	if h == nil {
		return
	}
	labels := func(extra ...string) string {
		parts := make([]string, 0, 2)
		if labelName != "" {
			parts = append(parts, fmt.Sprintf("%s=%q", labelName, labelValue))
		}
		parts = append(parts, extra...)
		return strings.Join(parts, ",")
	}
	cumulative := int64(0)
	for i, bound := range histogramBuckets {
		cumulative += h.buckets[i].Load()
		fmt.Fprintf(builder, "%s_bucket{%s} %d\n", name, labels("le="+strconv.Quote(strconv.FormatFloat(bound, 'g', -1, 64))), cumulative)
	}
	fmt.Fprintf(builder, "%s_bucket{%s} %d\n", name, labels("le=\"+Inf\""), h.count.Load())
	fmt.Fprintf(builder, "%s_sum{%s} %g\n", name, labels(), float64(h.sum.Load())/1e6)
	fmt.Fprintf(builder, "%s_count{%s} %d\n", name, labels(), h.count.Load())
}

// HealthReporter is the truthful health contract (§17.1): route readiness
// requires BOTH an applied route snapshot and a healthy control sync, and
// frps process health is a separate, independent truth. The two are never
// inferred from each other.
type HealthReporter interface {
	RouteReady() bool
	FRPSProcessHealthy() bool
}

// Handler returns the private operator endpoint: /metrics (Prometheus text)
// and /healthz (bounded JSON). It is private-only by construction: every
// request must arrive from a loopback peer AND carry a loopback Host. Public
// Hosts/interfaces are rejected with a generic 403 that exposes no route
// list, source-IP detail, or diagnostic. The process must additionally bind
// the listener to a loopback address (see bindLoopback); the guard is the
// second, independent enforcement.
func (r *Registry) Handler(health HealthReporter) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writer.Header().Set("Allow", http.MethodGet)
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeNoStore(writer)
		writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = io.WriteString(writer, r.Render())
	})
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writer.Header().Set("Allow", http.MethodGet)
			http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeNoStore(writer)
		writer.Header().Set("Content-Type", "application/json")
		routeReady := health != nil && health.RouteReady()
		frpsHealthy := health != nil && health.FRPSProcessHealthy()
		_ = json.NewEncoder(writer).Encode(map[string]bool{
			"route_ready":          routeReady,
			"frps_process_healthy": frpsHealthy,
		})
	})
	return privateOnly(mux)
}

// privateOnly wraps next with the private-monitoring guard.
func privateOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !isLoopbackPeer(request.RemoteAddr) || !isLoopbackHost(request.Host) {
			writeNoStore(writer)
			http.Error(writer, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func writeNoStore(writer http.ResponseWriter) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
}

func isLoopbackPeer(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func isLoopbackHost(hostport string) bool {
	if hostport == "" {
		return false
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// BindLoopback validates that address is a numeric loopback host:port and
// returns it. Startup fails closed rather than binding a public interface.
func BindLoopback(address string) (string, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("metrics: address must be host:port: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("metrics: address %q is not a numeric loopback address", address)
	}
	return address, nil
}

// ---------------------------------------------------------------------------
// Bounded log-rate handler (§17.1 "bounded retention/rate expectations")
// ---------------------------------------------------------------------------

// NewRateLimitedHandler wraps next with a bounded per-second log budget. A
// burst above the budget is dropped and counted; every dropSummary-th dropped
// record emits one summary line so operators see sustained flapping instead of
// silently lost output. Retention itself is delegated to journald's bounded
// SystemMaxUse/MaxRetentionSec (Task 35 deployment units); this handler bounds
// the gateway's own write rate so a public-connection flood cannot fill the
// journal.
func NewRateLimitedHandler(next slog.Handler, perSecond int, burst int) slog.Handler {
	if perSecond < 1 {
		perSecond = 1
	}
	if burst < 1 {
		burst = 1
	}
	return &rateLimitedHandler{
		next:   next,
		bucket: &tokenBucket{perSecond: perSecond, burst: burst, tokens: float64(burst), dropSummary: 100},
	}
}

type rateLimitedHandler struct {
	next   slog.Handler
	bucket *tokenBucket
}

// tokenBucket is the shared, mutex-guarded budget. WithAttrs/WithGroup keep
// the same bucket so a logger clone cannot mint a fresh budget.
type tokenBucket struct {
	mu          sync.Mutex
	perSecond   int
	burst       int
	tokens      float64
	last        int64 // unix nanos
	dropped     int64
	dropSummary int64
}

func (h *rateLimitedHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *rateLimitedHandler) Handle(ctx context.Context, record slog.Record) error {
	now := time.Now().UnixNano()
	bucket := h.bucket
	bucket.mu.Lock()
	if bucket.last == 0 {
		bucket.last = now
	}
	elapsed := float64(now-bucket.last) / float64(time.Second)
	bucket.last = now
	bucket.tokens += elapsed * float64(bucket.perSecond)
	if bucket.tokens > float64(bucket.burst) {
		bucket.tokens = float64(bucket.burst)
	}
	if bucket.tokens < 1 {
		bucket.dropped++
		summarize := bucket.dropped%bucket.dropSummary == 0
		dropped := bucket.dropped
		bucket.mu.Unlock()
		if !summarize {
			return nil
		}
		summary := slog.NewRecord(record.Time, slog.LevelWarn, "logs rate limited", 0)
		summary.AddAttrs(slog.Int64("dropped_total", dropped))
		return h.next.Handle(ctx, summary)
	}
	bucket.tokens--
	bucket.mu.Unlock()
	return h.next.Handle(ctx, record)
}

func (h *rateLimitedHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &rateLimitedHandler{next: h.next.WithAttrs(attrs), bucket: h.bucket}
}

func (h *rateLimitedHandler) WithGroup(name string) slog.Handler {
	return &rateLimitedHandler{next: h.next.WithGroup(name), bucket: h.bucket}
}

// Dropped reports how many records the rate limiter has dropped.
func (h *rateLimitedHandler) Dropped() int64 {
	h.bucket.mu.Lock()
	defer h.bucket.mu.Unlock()
	return h.bucket.dropped
}
