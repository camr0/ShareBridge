// Package limits enforces the spec §14 resource bounds before the gateway
// opens an FRP user connection (spec §14: "Limits are enforced before opening
// an FRP user connection"). It owns the concurrency ceilings (global, source
// IP, agent, exact origin), the per-agent byte and active-stream counters the
// operator uses as saturation signals, and the timeout/lifetime default
// values the gateway data plane applies.
//
// Admission is two-phase because the ceiling keys become known at different
// moments. A public connection's global and source-IP slots are acquired at
// accept time — before the accept loop spawns a handler goroutine and before
// the 64 KiB ClientHello parse allocates its scratch buffer (audit finding
// I7) — and its agent and exact-origin slots are acquired after the exact
// route lookup and before the loopback dial. Each phase is all-or-nothing
// under the limiter's single mutex, and releases run in reverse: origin,
// agent, source IP, global.
//
// The Phase 4a product-tier bandwidth throttle is deliberately absent (§14):
// the pinned FRP release's only bandwidth input is a client-declared proxy
// option the fail-closed plugin rejects, so per-agent byte counters and
// saturation alerts are shipped instead of a cap. The cap stays deferred to
// Phase 4b.
package limits

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"sharebridge/relay/internal/clienthello"
)

// §14 MVP defaults. Every one is configuration: tests override them through
// Config and production operators may tune them from telemetry without a
// protocol change.
const (
	DefaultMaxStreamsPerSourceIP = 16
	DefaultMaxStreamsPerOrigin   = 32
	DefaultMaxStreamsPerAgent    = 64
	DefaultMaxStreamsGlobal      = 8192
	DefaultProxiesPerAgent       = 1
	DefaultMaxHelloBytes         = 64 << 10
	DefaultHelloTimeout          = 5 * time.Second
	DefaultDialTimeout           = 2 * time.Second
	DefaultIdleTimeout           = 5 * time.Minute
	DefaultAbsoluteLifetime      = 24 * time.Hour
	// DefaultMaxTrackedAgents bounds the persistent per-agent byte map (the
	// one map that outlives individual streams). Control's Task 11 snapshot
	// validator bounds a gateway's route set at 4096; an agent ceiling of the
	// same size keeps per-agent accounting from growing without bound while
	// admitting every agent a healthy deployment can hold. It is also the
	// HARD CEILING for the environment input: because this map persists for
	// the process lifetime, the operator surface must be finite, so
	// SHAREBRIDGE_GATEWAY_MAX_TRACKED_AGENTS may only lower it, never raise
	// it (the same tighten-only pattern as MaxStreamsGlobal). See
	// ConfigFromEnvironment; the documented memory cost is roughly two map
	// entries per agent (an identity key of at most 64 bytes, per
	// controlsync.MaxIdentityBytes, plus one uint64 and one bool), i.e. on the
	// order of a few hundred bytes per agent and about 1 MiB at this ceiling —
	// negligible against the runbook's 512 MiB gateway MemoryMax.
	DefaultMaxTrackedAgents = 4096
)

// §14 operator configuration variable names. Every documented tunable bound
// has one production input; an unset variable keeps its documented default.
// main.go resolves them at startup and fails closed on an invalid value
// rather than running with a silently wrong bound.
const (
	// EnvMaxStreamsPerSourceIP caps concurrent connections from one source IP.
	EnvMaxStreamsPerSourceIP = "SHAREBRIDGE_GATEWAY_MAX_STREAMS_PER_SOURCE_IP"
	// EnvMaxStreamsPerOrigin caps concurrent streams on one exact origin.
	EnvMaxStreamsPerOrigin = "SHAREBRIDGE_GATEWAY_MAX_STREAMS_PER_ORIGIN"
	// EnvMaxStreamsPerAgent caps concurrent streams across one agent's routes.
	EnvMaxStreamsPerAgent = "SHAREBRIDGE_GATEWAY_MAX_STREAMS_PER_AGENT"
	// EnvMaxStreamsGlobal caps concurrent streams for the whole process; an
	// operator sets it to the host file-descriptor budget when that is lower
	// than the §14 ceiling.
	EnvMaxStreamsGlobal = "SHAREBRIDGE_GATEWAY_MAX_STREAMS_GLOBAL"
	// EnvMaxHelloBytes caps the inspected ClientHello prefix.
	EnvMaxHelloBytes = "SHAREBRIDGE_GATEWAY_MAX_HELLO_BYTES"
	// EnvHelloTimeout is the ClientHello read deadline.
	EnvHelloTimeout = "SHAREBRIDGE_GATEWAY_HELLO_TIMEOUT"
	// EnvDialTimeout is the loopback connect budget.
	EnvDialTimeout = "SHAREBRIDGE_GATEWAY_DIAL_TIMEOUT"
	// EnvIdleTimeout is the no-byte stream idle timeout.
	EnvIdleTimeout = "SHAREBRIDGE_GATEWAY_IDLE_TIMEOUT"
	// EnvAbsoluteLifetime is the hard connection lifetime.
	EnvAbsoluteLifetime = "SHAREBRIDGE_GATEWAY_ABSOLUTE_LIFETIME"
	// EnvMaxTrackedAgents bounds the persistent per-agent byte map. It is a
	// HARD-CEILING input: a value above DefaultMaxTrackedAgents refuses
	// startup rather than letting the persistent map grow without bound.
	EnvMaxTrackedAgents = "SHAREBRIDGE_GATEWAY_MAX_TRACKED_AGENTS"
)

// EnvironmentLookup resolves one configuration variable and reports whether
// it was set. os.LookupEnv is the production implementation; tests supply a
// deterministic map.
type EnvironmentLookup func(name string) (string, bool)

// Saturation alert kinds. Operators alert on these; the gateway logs them
// with metadata only (never a credential, share code, header, or body).
const (
	SaturationGlobal     = "global"
	SaturationSourceIP   = "source_ip"
	SaturationAgent      = "agent"
	SaturationOrigin     = "origin"
	SaturationAgentBytes = "agent_bytes"
)

// Failure classes. The gateway closes public connections generically on any
// of them (spec §8); the distinctions exist for logs, metrics, and tests.
var (
	// ErrGlobalLimit covers the per-process connection ceiling.
	ErrGlobalLimit = errors.New("limits: global connection ceiling reached")
	// ErrSourceIPLimit covers the per-source-IP connection ceiling.
	ErrSourceIPLimit = errors.New("limits: per-source-IP connection ceiling reached")
	// ErrAgentLimit covers the per-agent active-stream ceiling.
	ErrAgentLimit = errors.New("limits: per-agent stream ceiling reached")
	// ErrOriginLimit covers the per-exact-origin active-stream ceiling.
	ErrOriginLimit = errors.New("limits: per-origin stream ceiling reached")
	// ErrAgentCapacity covers the persistent tracked-agent byte map ceiling.
	// It fails closed rather than evicting, because eviction would silently
	// corrupt long-lived operator accounting.
	ErrAgentCapacity = errors.New("limits: tracked-agent ceiling reached")
	// ErrLeaseInvalid covers a stream admission attempted with a nil, foreign,
	// or already-released connection lease.
	ErrLeaseInvalid = errors.New("limits: connection lease is not held by this limiter")
)

// Config carries every §14 bound. The zero value is not usable directly; use
// DefaultConfig and override the fields under test (NewLimiter fills any
// zero or negative value with the matching default, so a partially-built
// Config is still safe).
type Config struct {
	// MaxStreamsPerSourceIP caps concurrent connections from one source IP.
	MaxStreamsPerSourceIP int
	// MaxStreamsPerOrigin caps concurrent streams on one exact origin.
	MaxStreamsPerOrigin int
	// MaxStreamsPerAgent caps concurrent streams across one agent's routes.
	MaxStreamsPerAgent int
	// MaxStreamsGlobal caps concurrent connections for the whole process; the
	// operator sets it to the host file-descriptor budget when that is lower
	// than the §14 default.
	MaxStreamsGlobal int
	// ProxiesPerAgent is the §14 "exactly 1" proxy rule. The gateway does not
	// create FRP proxies; the value is carried here so the bound is declared
	// and pinned in one place, and Task 7's fail-closed plugin enforces it at
	// the FRP boundary.
	ProxiesPerAgent int
	// MaxHelloBytes is the §14 ClientHello inspection ceiling. Enforcement
	// lives in the clienthello parser; the gateway pins the two together via
	// this value.
	MaxHelloBytes int
	// HelloTimeout is the §14 ClientHello read deadline (clienthello parser).
	HelloTimeout time.Duration
	// DialTimeout is the §14 loopback connect budget.
	DialTimeout time.Duration
	// IdleTimeout is the §14 no-byte stream idle timeout. Activity in either
	// direction resets it.
	IdleTimeout time.Duration
	// AbsoluteLifetime is the §14 hard close; continuous activity cannot
	// extend it.
	AbsoluteLifetime time.Duration
	// MaxTrackedAgents bounds the persistent per-agent byte map. A new agent
	// beyond the ceiling is refused (ErrAgentCapacity).
	MaxTrackedAgents int
	// AgentBytesAlertThreshold emits one SaturationAgentBytes alert per agent
	// when its cumulative relayed bytes first reach the threshold. Zero (the
	// default) disables the byte alert; the per-ceiling alerts still fire.
	AgentBytesAlertThreshold uint64
	// OnSaturation, when set, receives every saturation alert. It is invoked
	// without the limiter mutex held and must not call back into the limiter.
	OnSaturation func(Saturation)
}

// DefaultConfig returns the §14 MVP defaults.
func DefaultConfig() Config {
	return Config{
		MaxStreamsPerSourceIP: DefaultMaxStreamsPerSourceIP,
		MaxStreamsPerOrigin:   DefaultMaxStreamsPerOrigin,
		MaxStreamsPerAgent:    DefaultMaxStreamsPerAgent,
		MaxStreamsGlobal:      DefaultMaxStreamsGlobal,
		ProxiesPerAgent:       DefaultProxiesPerAgent,
		MaxHelloBytes:         DefaultMaxHelloBytes,
		HelloTimeout:          DefaultHelloTimeout,
		DialTimeout:           DefaultDialTimeout,
		IdleTimeout:           DefaultIdleTimeout,
		AbsoluteLifetime:      DefaultAbsoluteLifetime,
		MaxTrackedAgents:      DefaultMaxTrackedAgents,
	}
}

// ConfigFromEnvironment builds the production §14 configuration from an
// environment lookup. An unset (or empty) variable keeps its documented
// default; a set variable must be a positive integer or Go duration, and the
// hard ceilings are enforced: MaxHelloBytes may not exceed the parser's
// inspection budget, MaxStreamsGlobal may not exceed the §14 global
// ceiling (the documentation defines it as 8,192 or a LOWER host
// file-descriptor budget), and MaxTrackedAgents may not exceed its ceiling
// (the persistent per-agent map is not bounded by current concurrency over
// the process lifetime, so its configuration surface must be finite). Any
// invalid value fails closed: the zero Config is returned with a descriptive
// error so main.go refuses startup instead of silently using a wrong bound.
func ConfigFromEnvironment(lookup EnvironmentLookup) (Config, error) {
	config := DefaultConfig()
	integerBounds := []struct {
		name   string
		target *int
	}{
		{EnvMaxStreamsPerSourceIP, &config.MaxStreamsPerSourceIP},
		{EnvMaxStreamsPerOrigin, &config.MaxStreamsPerOrigin},
		{EnvMaxStreamsPerAgent, &config.MaxStreamsPerAgent},
		{EnvMaxStreamsGlobal, &config.MaxStreamsGlobal},
		{EnvMaxHelloBytes, &config.MaxHelloBytes},
		{EnvMaxTrackedAgents, &config.MaxTrackedAgents},
	}
	for _, bound := range integerBounds {
		if err := applyIntegerEnvironment(bound.target, lookup, bound.name); err != nil {
			return Config{}, err
		}
	}
	durationBounds := []struct {
		name   string
		target *time.Duration
	}{
		{EnvHelloTimeout, &config.HelloTimeout},
		{EnvDialTimeout, &config.DialTimeout},
		{EnvIdleTimeout, &config.IdleTimeout},
		{EnvAbsoluteLifetime, &config.AbsoluteLifetime},
	}
	for _, bound := range durationBounds {
		if err := applyDurationEnvironment(bound.target, lookup, bound.name); err != nil {
			return Config{}, err
		}
	}
	if config.MaxHelloBytes > clienthello.MaxBufferedBytes {
		return Config{}, fmt.Errorf("limits: %s = %d exceeds the ClientHello inspection budget %d",
			EnvMaxHelloBytes, config.MaxHelloBytes, clienthello.MaxBufferedBytes)
	}
	if config.MaxStreamsGlobal > DefaultMaxStreamsGlobal {
		return Config{}, fmt.Errorf("limits: %s = %d exceeds the §14 global ceiling %d",
			EnvMaxStreamsGlobal, config.MaxStreamsGlobal, DefaultMaxStreamsGlobal)
	}
	if config.MaxTrackedAgents > DefaultMaxTrackedAgents {
		return Config{}, fmt.Errorf("limits: %s = %d exceeds the tracked-agent ceiling %d",
			EnvMaxTrackedAgents, config.MaxTrackedAgents, DefaultMaxTrackedAgents)
	}
	return config, nil
}

// applyIntegerEnvironment overrides target from one environment variable. An
// unset or empty variable is left at its default; a set value must be a
// positive integer or the whole configuration is rejected.
func applyIntegerEnvironment(target *int, lookup EnvironmentLookup, name string) error {
	raw, ok := lookup(name)
	if !ok || raw == "" {
		return nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fmt.Errorf("limits: %s must be a positive integer, got %q", name, raw)
	}
	*target = value
	return nil
}

// applyDurationEnvironment overrides target from one environment variable. An
// unset or empty variable is left at its default; a set value must be a
// positive Go duration or the whole configuration is rejected.
func applyDurationEnvironment(target *time.Duration, lookup EnvironmentLookup, name string) error {
	raw, ok := lookup(name)
	if !ok || raw == "" {
		return nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return fmt.Errorf("limits: %s must be a positive duration, got %q", name, raw)
	}
	*target = value
	return nil
}

// withDefaults replaces any unset (zero or negative) bound with its §14
// default so a partially-specified Config can never disable a ceiling
// accidentally.
func (config Config) withDefaults() Config {
	defaults := DefaultConfig()
	if config.MaxStreamsPerSourceIP <= 0 {
		config.MaxStreamsPerSourceIP = defaults.MaxStreamsPerSourceIP
	}
	if config.MaxStreamsPerOrigin <= 0 {
		config.MaxStreamsPerOrigin = defaults.MaxStreamsPerOrigin
	}
	if config.MaxStreamsPerAgent <= 0 {
		config.MaxStreamsPerAgent = defaults.MaxStreamsPerAgent
	}
	if config.MaxStreamsGlobal <= 0 {
		config.MaxStreamsGlobal = defaults.MaxStreamsGlobal
	}
	if config.ProxiesPerAgent <= 0 {
		config.ProxiesPerAgent = defaults.ProxiesPerAgent
	}
	if config.MaxHelloBytes <= 0 {
		config.MaxHelloBytes = defaults.MaxHelloBytes
	}
	if config.HelloTimeout <= 0 {
		config.HelloTimeout = defaults.HelloTimeout
	}
	if config.DialTimeout <= 0 {
		config.DialTimeout = defaults.DialTimeout
	}
	if config.IdleTimeout <= 0 {
		config.IdleTimeout = defaults.IdleTimeout
	}
	if config.AbsoluteLifetime <= 0 {
		config.AbsoluteLifetime = defaults.AbsoluteLifetime
	}
	if config.MaxTrackedAgents <= 0 {
		config.MaxTrackedAgents = defaults.MaxTrackedAgents
	}
	return config
}

// Saturation reports one counter reaching (or a rejection against) a §14
// ceiling. Key is the exact source IP, agent record ID, or origin hostname
// where the kind carries one; byte alerts use Bytes/Threshold instead of
// Active/Limit. Values are metadata only.
type Saturation struct {
	Kind      string
	Key       string
	Active    int
	Limit     int
	Bytes     uint64
	Threshold uint64
}

// StreamRequest describes one stream admission after the exact route lookup.
// The route ceilings are control-distributed and may only tighten the
// process defaults: a zero route value means "no override" (the process
// ceiling stays authoritative) and a looser route value is ignored. This is
// the same tighten-only rule for the route global as for origin and agent.
type StreamRequest struct {
	Hostname            string
	AgentRecordID       string
	MaxStreamsPerOrigin int
	MaxStreamsPerAgent  int
	// MaxStreamsGlobal is the route-distributed global ceiling. It may only
	// tighten the process ceiling; effectiveLimit resolves the minimum.
	MaxStreamsGlobal int
}

// Limiter is the concurrency/accounting authority for one gateway process.
// Safe for concurrent use. All admission and release mutations happen under
// one mutex so each phase is atomic and no counter can be double-counted.
type Limiter struct {
	config Config

	mu                sync.Mutex
	global            int
	activePerSourceIP map[string]int
	activePerAgent    map[string]int
	activePerOrigin   map[string]int
	agentBytes        map[string]uint64
	agentBytesAlerted map[string]bool
}

// NewLimiter builds a limiter from config, filling unset bounds with §14
// defaults.
func NewLimiter(config Config) *Limiter {
	return &Limiter{
		config:            config.withDefaults(),
		activePerSourceIP: make(map[string]int),
		activePerAgent:    make(map[string]int),
		activePerOrigin:   make(map[string]int),
		agentBytes:        make(map[string]uint64),
		agentBytesAlerted: make(map[string]bool),
	}
}

// ConnectionLease is the pre-parse phase of one public connection: the global
// and source-IP slots. Release must run on every exit path, including parse,
// lookup, admission, dial, replay, and close errors.
type ConnectionLease struct {
	limiter  *Limiter
	sourceIP string
	release  sync.Once
	released atomic.Bool
}

// StreamLease is the post-route phase of one public stream: the agent and
// exact-origin slots plus the per-agent byte accounting handle. Release must
// run on every exit path, after the connection lease by reverse order.
type StreamLease struct {
	limiter       *Limiter
	agentRecordID string
	origin        string
	release       sync.Once
}

// Config returns the effective configuration (defaults applied).
func (limiter *Limiter) Config() Config {
	return limiter.config
}

// DialTimeout is the §14 loopback connect budget.
func (limiter *Limiter) DialTimeout() time.Duration { return limiter.config.DialTimeout }

// IdleTimeout is the §14 no-byte stream idle timeout.
func (limiter *Limiter) IdleTimeout() time.Duration { return limiter.config.IdleTimeout }

// AbsoluteLifetime is the §14 hard connection lifetime.
func (limiter *Limiter) AbsoluteLifetime() time.Duration { return limiter.config.AbsoluteLifetime }

// AdmitConnection acquires the global slot and then the source-IP slot for a
// newly accepted public connection. It is called before the handler goroutine
// is spawned and before the ClientHello parse, so the accept loop can reject
// an over-limit peer without spawning work or allocating the parser scratch
// (audit I7). The acquisition is all-or-nothing.
func (limiter *Limiter) AdmitConnection(remote net.Addr) (*ConnectionLease, error) {
	sourceIP := sourceIPKey(remote)

	limiter.mu.Lock()
	var alert *Saturation
	var failure error
	switch {
	case limiter.global >= limiter.config.MaxStreamsGlobal:
		alert = &Saturation{Kind: SaturationGlobal, Active: limiter.global, Limit: limiter.config.MaxStreamsGlobal}
		failure = fmt.Errorf("%w: %d active connections against the global ceiling %d",
			ErrGlobalLimit, limiter.global, limiter.config.MaxStreamsGlobal)
	case limiter.activePerSourceIP[sourceIP] >= limiter.config.MaxStreamsPerSourceIP:
		alert = &Saturation{Kind: SaturationSourceIP, Key: sourceIP, Active: limiter.activePerSourceIP[sourceIP], Limit: limiter.config.MaxStreamsPerSourceIP}
		failure = fmt.Errorf("%w: source %s holds %d connections against the ceiling %d",
			ErrSourceIPLimit, sourceIP, limiter.activePerSourceIP[sourceIP], limiter.config.MaxStreamsPerSourceIP)
	default:
		limiter.global++
		limiter.activePerSourceIP[sourceIP]++
	}
	limiter.mu.Unlock()

	if alert != nil {
		limiter.emit(*alert)
	}
	if failure != nil {
		return nil, failure
	}
	return &ConnectionLease{limiter: limiter, sourceIP: sourceIP}, nil
}

// AdmitStream acquires the agent slot and then the exact-origin slot for a
// connection whose exact route is known, and registers the agent's byte
// counter. It is called before the loopback dial. A failure at either ceiling
// acquires nothing (no partial agent slot). conn must be a live lease from
// this limiter.
func (limiter *Limiter) AdmitStream(conn *ConnectionLease, request StreamRequest) (*StreamLease, error) {
	if conn == nil || conn.limiter != limiter || conn.released.Load() {
		return nil, fmt.Errorf("%w: stream admission requires a live lease from this limiter", ErrLeaseInvalid)
	}

	agentKey := request.AgentRecordID
	if agentKey == "" {
		agentKey = "<unknown-agent>"
	}
	originKey := request.Hostname
	if originKey == "" {
		originKey = "<unknown-origin>"
	}
	agentLimit := effectiveLimit(limiter.config.MaxStreamsPerAgent, request.MaxStreamsPerAgent)
	originLimit := effectiveLimit(limiter.config.MaxStreamsPerOrigin, request.MaxStreamsPerOrigin)
	globalLimit := effectiveLimit(limiter.config.MaxStreamsGlobal, request.MaxStreamsGlobal)

	limiter.mu.Lock()
	var alert *Saturation
	var failure error
	switch {
	case limiter.global > globalLimit:
		// The global slot was taken at AdmitConnection against the process
		// ceiling; a route-level tightening must be re-checked here, at the
		// same atomic admission point, before any agent or origin slot is
		// acquired. The check only reads the already-held counter, so it
		// acquires nothing and release semantics are unchanged.
		alert = &Saturation{Kind: SaturationGlobal, Active: limiter.global, Limit: globalLimit}
		failure = fmt.Errorf("%w: %d active streams against the route-tightened global ceiling %d",
			ErrGlobalLimit, limiter.global, globalLimit)
	case limiter.activePerAgent[agentKey] >= agentLimit:
		alert = &Saturation{Kind: SaturationAgent, Key: agentKey, Active: limiter.activePerAgent[agentKey], Limit: agentLimit}
		failure = fmt.Errorf("%w: agent %q holds %d streams against the ceiling %d",
			ErrAgentLimit, agentKey, limiter.activePerAgent[agentKey], agentLimit)
	case limiter.activePerOrigin[originKey] >= originLimit:
		alert = &Saturation{Kind: SaturationOrigin, Key: originKey, Active: limiter.activePerOrigin[originKey], Limit: originLimit}
		failure = fmt.Errorf("%w: origin %q holds %d streams against the ceiling %d",
			ErrOriginLimit, originKey, limiter.activePerOrigin[originKey], originLimit)
	case !limiter.agentTrackedLocked(agentKey) && len(limiter.agentBytes) >= limiter.config.MaxTrackedAgents:
		failure = fmt.Errorf("%w: %d agents tracked against the ceiling %d",
			ErrAgentCapacity, len(limiter.agentBytes), limiter.config.MaxTrackedAgents)
	default:
		limiter.activePerAgent[agentKey]++
		limiter.activePerOrigin[originKey]++
		if !limiter.agentTrackedLocked(agentKey) {
			limiter.agentBytes[agentKey] = 0
		}
	}
	limiter.mu.Unlock()

	if alert != nil {
		limiter.emit(*alert)
	}
	if failure != nil {
		return nil, failure
	}
	return &StreamLease{limiter: limiter, agentRecordID: agentKey, origin: originKey}, nil
}

// agentTrackedLocked reports whether agentKey already has a persistent byte
// counter. Caller holds limiter.mu.
func (limiter *Limiter) agentTrackedLocked(agentKey string) bool {
	_, ok := limiter.agentBytes[agentKey]
	return ok
}

// Release releases the source-IP and global slots. Idempotent: repeated calls
// change nothing.
func (lease *ConnectionLease) Release() {
	if lease == nil {
		return
	}
	lease.release.Do(func() {
		lease.released.Store(true)
		limiter := lease.limiter
		limiter.mu.Lock()
		defer limiter.mu.Unlock()
		if count := limiter.activePerSourceIP[lease.sourceIP]; count > 1 {
			limiter.activePerSourceIP[lease.sourceIP] = count - 1
		} else {
			delete(limiter.activePerSourceIP, lease.sourceIP)
		}
		if limiter.global > 0 {
			limiter.global--
		}
	})
}

// Release releases the exact-origin and agent slots. Idempotent: repeated
// calls change nothing. The persistent per-agent byte counter is intentionally
// not deleted; it is an operator gauge that outlives individual streams.
func (lease *StreamLease) Release() {
	if lease == nil {
		return
	}
	lease.release.Do(func() {
		limiter := lease.limiter
		limiter.mu.Lock()
		defer limiter.mu.Unlock()
		if count := limiter.activePerOrigin[lease.origin]; count > 1 {
			limiter.activePerOrigin[lease.origin] = count - 1
		} else {
			delete(limiter.activePerOrigin, lease.origin)
		}
		if count := limiter.activePerAgent[lease.agentRecordID]; count > 1 {
			limiter.activePerAgent[lease.agentRecordID] = count - 1
		} else {
			delete(limiter.activePerAgent, lease.agentRecordID)
		}
	})
}

// AddBytes adds relayed payload bytes to the stream's agent counter and
// emits at most one SaturationAgentBytes alert per agent when the configured
// threshold is first reached. Non-positive counts are ignored. It is called
// from the data plane after each copied chunk, so it never buffers payload.
func (lease *StreamLease) AddBytes(count int) {
	if lease == nil || count <= 0 {
		return
	}
	limiter := lease.limiter
	limiter.mu.Lock()
	limiter.agentBytes[lease.agentRecordID] += uint64(count)
	var alert *Saturation
	if threshold := limiter.config.AgentBytesAlertThreshold; threshold > 0 &&
		limiter.agentBytes[lease.agentRecordID] >= threshold &&
		!limiter.agentBytesAlerted[lease.agentRecordID] {
		limiter.agentBytesAlerted[lease.agentRecordID] = true
		alert = &Saturation{
			Kind:      SaturationAgentBytes,
			Key:       lease.agentRecordID,
			Bytes:     limiter.agentBytes[lease.agentRecordID],
			Threshold: threshold,
		}
	}
	limiter.mu.Unlock()
	if alert != nil {
		limiter.emit(*alert)
	}
}

// ActiveConnections reports the number of admitted (including still-parsing)
// public connections.
func (limiter *Limiter) ActiveConnections() int {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	return limiter.global
}

// ActiveStreams reports the number of admitted streams across all agents.
func (limiter *Limiter) ActiveStreams() int {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	total := 0
	for _, count := range limiter.activePerAgent {
		total += count
	}
	return total
}

// ActiveStreamsForAgent reports one agent's admitted stream count.
func (limiter *Limiter) ActiveStreamsForAgent(agentRecordID string) int {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	return limiter.activePerAgent[agentRecordID]
}

// BytesForAgent reports one agent's cumulative relayed payload bytes.
func (limiter *Limiter) BytesForAgent(agentRecordID string) uint64 {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	return limiter.agentBytes[agentRecordID]
}

// TrackedSourceIPs reports how many distinct source IPs currently hold a
// connection slot. Bounded by the global ceiling.
func (limiter *Limiter) TrackedSourceIPs() int {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	return len(limiter.activePerSourceIP)
}

// TrackedOrigins reports how many distinct exact origins currently hold a
// stream slot. Bounded by the global ceiling.
func (limiter *Limiter) TrackedOrigins() int {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	return len(limiter.activePerOrigin)
}

// TrackedAgents reports how many agents have a persistent byte counter.
// Bounded by MaxTrackedAgents.
func (limiter *Limiter) TrackedAgents() int {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	return len(limiter.agentBytes)
}

// emit invokes the saturation hook outside the limiter mutex.
func (limiter *Limiter) emit(saturation Saturation) {
	if limiter.config.OnSaturation != nil {
		limiter.config.OnSaturation(saturation)
	}
}

// effectiveLimit resolves a §14 ceiling. The process value is the safety
// ceiling; a positive control-distributed route value may only tighten it,
// and a zero route value means "no override".
func effectiveLimit(configured, routeValue int) int {
	if routeValue > 0 && routeValue < configured {
		return routeValue
	}
	return configured
}

// sourceIPKey normalizes a public peer address to a stable source-IP key:
// the port is dropped and IPv4-mapped IPv6 addresses are unmapped so the same
// host cannot evade the ceiling by address family.
func sourceIPKey(remote net.Addr) string {
	if remote == nil {
		return "<unknown-source>"
	}
	if tcpAddr, ok := remote.(*net.TCPAddr); ok {
		if addr, valid := netip.AddrFromSlice(tcpAddr.IP); valid {
			return addr.Unmap().String()
		}
	}
	host, _, err := net.SplitHostPort(remote.String())
	if err == nil && host != "" {
		if addr, parseErr := netip.ParseAddr(host); parseErr == nil {
			return addr.Unmap().String()
		}
		return host
	}
	return remote.String()
}
