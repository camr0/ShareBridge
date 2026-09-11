package metrics

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// NIC saturation (§17.3 bullet 5). This file provides the bounded collector
// for `sharebridge_relay_gateway_nic_saturation_ratio`. Two constraints shape
// it:
//
//  1. Bounded cost. There is exactly ONE sampler goroutine per process and it
//     only reads a counter source on a fixed interval. No public-connection
//     path touches it, and there is never a goroutine per connection.
//
//  2. Truthful availability. The saturation ratio is only meaningful when the
//     host actually exposes byte counters AND a capacity to divide them by. On
//     a platform without /proc/net/dev (macOS, BSD, a container without procfs)
//     or an unconfigured interface/capacity the sampler reports the value as
//     UNAVAILABLE — the gauge renders NaN — rather than fabricating a 0 that a
//     dashboard would read as "healthy and idle".
type NICSource func() (ratio float64, ok bool)

// Sampler periodically samples a NICSource into the named ratio gauge. Safe
// for concurrent use; the counter read and the metric read are the only
// operations.
type Sampler struct {
	registry *Registry
	name     string
	source   NICSource
	interval time.Duration

	mu    sync.Mutex
	ratio float64
	ok    bool
}

// NewSampler builds a bounded sampler and installs its scrape-time collector
// on registry. A nil registry or source yields a collector that always reports
// unavailable. interval <= 0 selects the 15-second default.
func NewSampler(registry *Registry, name string, source NICSource, interval time.Duration) (*Sampler, error) {
	if name == "" {
		return nil, errors.New("metrics: NIC sampler requires a metric name")
	}
	if interval < 0 {
		return nil, errors.New("metrics: NIC sampler interval must not be negative")
	}
	if interval == 0 {
		interval = 15 * time.Second
	}
	sampler := &Sampler{registry: registry, name: name, source: source, interval: interval}
	if registry != nil {
		registry.SetRatioFunc(name, sampler.current)
	}
	return sampler, nil
}

// SampleOnce performs exactly one sample. Run calls it on a ticker; tests call
// it directly to drive the real sampler path without waiting on wall time.
func (sampler *Sampler) SampleOnce() {
	ratio, ok := 0.0, false
	if sampler.source != nil {
		ratio, ok = sampler.source()
	}
	sampler.mu.Lock()
	sampler.ratio, sampler.ok = ratio, ok
	sampler.mu.Unlock()
}

// Run samples immediately and then on the configured interval until ctx is
// cancelled. It is the single periodic NIC reader for the process.
func (sampler *Sampler) Run(ctx context.Context) {
	sampler.SampleOnce()
	ticker := time.NewTicker(sampler.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sampler.SampleOnce()
		}
	}
}

// current is the scrape-time collector behind the ratio gauge.
func (sampler *Sampler) current() (float64, bool) {
	sampler.mu.Lock()
	defer sampler.mu.Unlock()
	return sampler.ratio, sampler.ok
}

// parseProcNetDev extracts one interface's cumulative rx+tx byte counters from
// /proc/net/dev. The format is two header lines then one line per interface:
//
//	eth0: rx_bytes rx_packets ... rx_multicast tx_bytes tx_packets ...
//
// An absent interface or a malformed line reports ok=false.
func parseProcNetDev(data []byte, iface string) (rxBytes uint64, txBytes uint64, ok bool) {
	if iface == "" {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		if strings.TrimSpace(line[:colon]) != iface {
			continue
		}
		fields := strings.Fields(line[colon+1:])
		if len(fields) < 9 {
			return 0, 0, false
		}
		rx, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		tx, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		return rx, tx, true
	}
	return 0, 0, false
}

// ProcNetDevSource returns a NICSource that reads cumulative interface byte
// counters from a /proc/net/dev-shaped file and reports saturation as the
// observed bytes-per-second divided by capacityBytesPerSecond. Production
// passes "/proc/net/dev". The source holds the previous reading, so its first
// call (and every call after a counter reset, an unreadable file, or an absent
// interface) reports unavailable rather than a fabricated ratio. A capacity
// that is unset or non-positive also reports unavailable: without a link
// capacity there is no honest denominator.
func ProcNetDevSource(path string, iface string, capacityBytesPerSecond float64, now func() time.Time) NICSource {
	if now == nil {
		now = time.Now
	}
	var (
		mu            sync.Mutex
		previousBytes uint64
		previousTime  time.Time
		initialized   bool
	)
	return func() (float64, bool) {
		if capacityBytesPerSecond <= 0 || iface == "" {
			return 0, false
		}
		mu.Lock()
		defer mu.Unlock()
		data, err := os.ReadFile(path)
		if err != nil {
			return 0, false
		}
		rx, tx, ok := parseProcNetDev(data, iface)
		if !ok {
			return 0, false
		}
		nowTime := now()
		bytes := rx + tx
		if !initialized || bytes < previousBytes {
			previousBytes = bytes
			previousTime = nowTime
			initialized = true
			return 0, false
		}
		elapsed := nowTime.Sub(previousTime).Seconds()
		if elapsed <= 0 {
			return 0, false
		}
		delta := bytes - previousBytes
		previousBytes = bytes
		previousTime = nowTime
		ratio := (float64(delta) / elapsed) / capacityBytesPerSecond
		if ratio < 0 {
			ratio = 0
		}
		if ratio > 1 {
			ratio = 1
		}
		return ratio, true
	}
}
