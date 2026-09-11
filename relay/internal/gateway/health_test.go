package gateway

import (
	"testing"
	"time"
)

// TestFRPSHealthDownTransitionOnExpiredFreshnessWindow proves the frps
// process truth falls to unhealthy when the freshness window expires. The
// frps↔gateway plugin channel is per-operation HTTP, so liveness can only be
// proven by fresh authenticated traffic; an expired window must render
// unhealthy rather than staying stale-true.
func TestFRPSHealthDownTransitionOnExpiredFreshnessWindow(t *testing.T) {
	const window = 30 * time.Second
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	health := NewHealthWithFRPSFreshness(window, func() time.Time { return now })

	if health.FRPSProcessHealthy() {
		t.Fatal("frps health must read unhealthy before any authenticated frps fact (fail closed)")
	}
	health.SetFRPSHealthy(true)
	if !health.FRPSProcessHealthy() {
		t.Fatal("a fresh authenticated frps observation must read healthy")
	}
	// A window boundary that has not elapsed yet is still fresh.
	now = now.Add(window)
	if !health.FRPSProcessHealthy() {
		t.Fatal("frps health must stay healthy while the freshness window has not expired")
	}
	// One instant past the window is the down transition.
	now = now.Add(time.Nanosecond)
	if health.FRPSProcessHealthy() {
		t.Fatal("frps health must fall to unhealthy once the freshness window expires, never stay stale-true")
	}
}

// TestFRPSHealthRecoversOnNewAuthenticatedObservation proves a new
// authenticated frps fact restores healthy after the window expired.
func TestFRPSHealthRecoversOnNewAuthenticatedObservation(t *testing.T) {
	const window = 30 * time.Second
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	health := NewHealthWithFRPSFreshness(window, func() time.Time { return now })

	health.SetFRPSHealthy(true)
	now = now.Add(window + time.Second)
	if health.FRPSProcessHealthy() {
		t.Fatal("the freshness window had expired; frps must read unhealthy")
	}
	health.SetFRPSHealthy(true)
	if !health.FRPSProcessHealthy() {
		t.Fatal("a new authenticated frps fact must restore frps health")
	}
}

// TestFRPSHealthExplicitDownIsImmediate proves an explicit supervisor down
// transition is honoured immediately, not subject to the window, and that a
// later authenticated fact recovers.
func TestFRPSHealthExplicitDownIsImmediate(t *testing.T) {
	const window = 30 * time.Second
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	health := NewHealthWithFRPSFreshness(window, func() time.Time { return now })

	health.SetFRPSHealthy(true)
	health.SetFRPSHealthy(false)
	if health.FRPSProcessHealthy() {
		t.Fatal("an explicit frps down transition must be immediate")
	}
	health.SetFRPSHealthy(true)
	if !health.FRPSProcessHealthy() {
		t.Fatal("a new authenticated frps fact must restore frps health")
	}
}

// TestFRPSHealthNeverSeenIsUnhealthy proves the production constructor
// reports a never-seen frps as unhealthy (fail closed), never true.
func TestFRPSHealthNeverSeenIsUnhealthy(t *testing.T) {
	health := NewHealth()
	if health.FRPSProcessHealthy() {
		t.Fatal("a never-seen frps must read unhealthy (fail closed), never true")
	}
	if health.frpsHealthFreshness != DefaultFRPSFreshnessWindow {
		t.Fatalf("production freshness window = %v, want the named constant %v",
			health.frpsHealthFreshness, DefaultFRPSFreshnessWindow)
	}
	if DefaultFRPSFreshnessWindow <= 0 {
		t.Fatal("the production freshness window must be a positive bounded interval")
	}
}

// TestFRPSAndRouteReadyTruthsAreIndependent proves the two §17.1 truths never
// gate each other in either direction: route-ready without frps, frps healthy
// with route readiness withdrawn, and a window expiry that leaves route
// readiness untouched.
func TestFRPSAndRouteReadyTruthsAreIndependent(t *testing.T) {
	const window = 30 * time.Second
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	health := NewHealthWithFRPSFreshness(window, func() time.Time { return now })

	// Route-ready while frps is unknown/down.
	health.SetSnapshotReady(true)
	health.SetControlSynced(true)
	if !health.RouteReady() {
		t.Fatal("route readiness must be true from a snapshot plus healthy sync alone")
	}
	if health.FRPSProcessHealthy() {
		t.Fatal("frps must read unhealthy before any authenticated fact")
	}

	// frps healthy while route readiness is withdrawn.
	health.SetFRPSHealthy(true)
	health.SetControlSynced(false)
	if health.RouteReady() {
		t.Fatal("route readiness must fall when control sync is unhealthy")
	}
	if !health.FRPSProcessHealthy() {
		t.Fatal("control-sync loss must not affect the independent frps truth")
	}

	// A freshness-window expiry is frps-only: route readiness is unchanged.
	health.SetControlSynced(true)
	if !health.RouteReady() {
		t.Fatal("route readiness must recover with healthy sync")
	}
	now = now.Add(window + time.Second)
	if health.FRPSProcessHealthy() {
		t.Fatal("the freshness window expired; frps must read unhealthy")
	}
	if !health.RouteReady() {
		t.Fatal("the frps freshness expiry must never withdraw route readiness")
	}
}
