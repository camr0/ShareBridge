package metrics_test

// §17.3 bullet 5 NIC saturation coverage: the sampler is bounded (one
// injectable source, one sampler goroutine) and truthful (unavailable renders
// NaN, never a fabricated ratio). The tests drive the real sampler path.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sharebridge/relay/internal/metrics"
)

const nicMetricName = "sharebridge_relay_gateway_nic_saturation_ratio"

func TestNICSamplerReportsRealRatioAndUnavailableNeverZero(t *testing.T) {
	registry := metrics.NewRegistry(metrics.Relay)
	sampler, err := metrics.NewSampler(registry, nicMetricName, func() (float64, bool) { return 0.42, true }, time.Hour)
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	sampler.SampleOnce()
	if ratio, ok := registry.Ratio(nicMetricName); !ok || ratio != 0.42 {
		t.Fatalf("Ratio = %v, ok=%v, want 0.42,true", ratio, ok)
	}
	if rendered := registry.Render(); !strings.Contains(rendered, nicMetricName+" 0.42") {
		t.Fatalf("registry did not render the real ratio:\n%s", rendered)
	}

	// An unavailable source renders NaN (no data), never a fabricated 0.
	unavailable := metrics.NewRegistry(metrics.Relay)
	unavailableSampler, err := metrics.NewSampler(unavailable, nicMetricName, func() (float64, bool) { return 0, false }, time.Hour)
	if err != nil {
		t.Fatalf("NewSampler(unavailable): %v", err)
	}
	unavailableSampler.SampleOnce()
	if _, ok := unavailable.Ratio(nicMetricName); ok {
		t.Fatal("an unavailable source reported ok=true")
	}
	if rendered := unavailable.Render(); !strings.Contains(rendered, nicMetricName+" NaN") {
		t.Fatalf("unavailable NIC stats did not render NaN:\n%s", rendered)
	}
}

func TestNICSamplerRunSamplesPeriodically(t *testing.T) {
	registry := metrics.NewRegistry(metrics.Relay)
	sampler, err := metrics.NewSampler(registry, nicMetricName, func() (float64, bool) { return 0.5, true }, time.Millisecond)
	if err != nil {
		t.Fatalf("NewSampler: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sampler.Run(ctx)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ratio, ok := registry.Ratio(nicMetricName); ok && ratio == 0.5 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Run did not sample the injected source")
}

func TestProcNetDevSourceUsesRealCounters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dev")
	writeProcNetDev(t, path, 1000, 500)
	now := time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC)
	source := metrics.ProcNetDevSource(path, "eth0", 1000, func() time.Time { return now })

	if _, ok := source(); ok {
		t.Fatal("the first sample must be unavailable: there is no baseline delta yet")
	}
	now = now.Add(time.Second)
	writeProcNetDev(t, path, 1500, 1000) // +1000 bytes over 1s at 1000 B/s => ratio 1.0
	ratio, ok := source()
	if !ok {
		t.Fatal("a second reading with a real delta reported unavailable")
	}
	if ratio != 1 {
		t.Fatalf("ratio = %v, want 1.0 (1000 B/s of 1000 B/s capacity)", ratio)
	}

	// A capacity that is unset or non-positive has no honest denominator.
	noCapacity := metrics.ProcNetDevSource(path, "eth0", 0, func() time.Time { return now })
	if _, ok := noCapacity(); ok {
		t.Fatal("an unset capacity reported a ratio")
	}
	// A missing /proc/net/dev (non-Linux, container without procfs) is
	// unavailable, never an error that would fail the gateway.
	missing := metrics.ProcNetDevSource(filepath.Join(dir, "absent"), "eth0", 1000, func() time.Time { return now })
	if _, ok := missing(); ok {
		t.Fatal("a missing stats file reported a ratio")
	}
}

func writeProcNetDev(t *testing.T, path string, rxBytes, txBytes uint64) {
	t.Helper()
	line := "  eth0: " + itoa(int(rxBytes)) + " 7 0 0 0 0 0 0 " + itoa(int(txBytes)) + " 7 0 0 0 0 0 0\n"
	content := "Inter-|   Receive                                                |  Transmit\n" +
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n" +
		line
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write proc net dev fixture: %v", err)
	}
}
