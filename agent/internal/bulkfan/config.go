// Package bulkfan implements the flag-gated bulk-transfer levers for the v1
// agent, all three of which are candidates from
// docs/superpowers/spikes/2026-10-02-v1-direct-candidate-experiments-review.md:
//
//   - A1 (SB_BULK_CONNS)     — N independent PeerConnections per single transfer.
//   - N5 (SB_BULK_LANES)     — K ordered bulk DataChannels on the one association.
//   - A3 (SB_BULK_PACE_MBPS) — application-level token-bucket pacing.
//
// Every lever is off by default. With the SB_BULK_* variables unset the transfer
// path keeps emitting v1 chunk envelopes on a single bulk lane, byte-identical
// to the behaviour before this package existed. Flag parsing follows the
// SB_SCTP_* convention in internal/peer (parse, log and ignore invalid values).
package bulkfan

import (
	"log"
	"os"
	"strconv"
)

// Environment variable names. Each is read once per manager construction.
const (
	EnvConns    = "SB_BULK_CONNS"     // A1
	EnvLanes    = "SB_BULK_LANES"     // N5
	EnvPaceMbps = "SB_BULK_PACE_MBPS" // A3
)

// Bounds keep an operator typo from allocating hundreds of SCTP associations or
// DataChannels. Values above the bound are clamped (and logged).
const (
	MinConns    = 1
	MaxConns    = 4
	MinLanes    = 1
	MaxLanes    = 8
	MinPaceMbps = 0
	MaxPaceMbps = 10_000
)

// Config is the resolved set of bulk-transfer levers.
type Config struct {
	// Conns is the number of independent PeerConnections carrying one transfer
	// (A1). 1 disables fan-out.
	Conns int
	// Lanes is the number of ordered bulk DataChannels created on each
	// association (N5). 1 disables striping.
	Lanes int
	// PaceMbps is the token-bucket rate for bulk sends in megabits per second
	// (A3). 0 disables pacing.
	PaceMbps float64
}

// Default returns the offline configuration: every lever disabled.
func Default() Config {
	return Config{Conns: 1, Lanes: 1, PaceMbps: 0}
}

// FanoutEnabled reports whether A1 is active.
func (c Config) FanoutEnabled() bool { return c.Conns > 1 }

// StripeEnabled reports whether N5 is active.
func (c Config) StripeEnabled() bool { return c.Lanes > 1 }

// PacingEnabled reports whether A3 is active.
func (c Config) PacingEnabled() bool { return c.PaceMbps > 0 }

// Enabled reports whether any lever is active. When false the caller must take
// the untouched single-lane path.
func (c Config) Enabled() bool {
	return c.FanoutEnabled() || c.StripeEnabled() || c.PacingEnabled()
}

// PaceBytesPerSecond converts the configured rate for the token bucket.
func (c Config) PaceBytesPerSecond() float64 { return c.PaceMbps * 1_000_000 / 8 }

// FromEnv resolves the configuration from the supplied lookup. Production
// passes os.Getenv; tests pass a stub so they do not depend on the ambient
// environment (same pattern as peer.applySCTPTuning).
func FromEnv(getenv func(string) string) Config {
	cfg := Default()
	cfg.Conns = envInt(getenv, EnvConns, cfg.Conns, MinConns, MaxConns)
	cfg.Lanes = envInt(getenv, EnvLanes, cfg.Lanes, MinLanes, MaxLanes)
	cfg.PaceMbps = envFloat(getenv, EnvPaceMbps, cfg.PaceMbps, MinPaceMbps, MaxPaceMbps)
	return cfg
}

// FromOSEnv resolves the configuration from the process environment.
func FromOSEnv() Config { return FromEnv(os.Getenv) }

func envInt(getenv func(string) string, name string, def, min, max int) int {
	raw := getenv(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("bulkfan: ignoring %s=%q: %v", name, raw, err)
		return def
	}
	if n < min {
		log.Printf("bulkfan: ignoring %s=%q: below minimum %d", name, raw, min)
		return def
	}
	if n > max {
		log.Printf("bulkfan: clamping %s=%q to %d", name, raw, max)
		return max
	}
	return n
}

func envFloat(getenv func(string) string, name string, def, min, max float64) float64 {
	raw := getenv(name)
	if raw == "" {
		return def
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		log.Printf("bulkfan: ignoring %s=%q: %v", name, raw, err)
		return def
	}
	if f < min {
		log.Printf("bulkfan: ignoring %s=%q: below minimum %v", name, raw, min)
		return def
	}
	if f > max {
		log.Printf("bulkfan: clamping %s=%q to %v", name, raw, max)
		return max
	}
	return f
}
