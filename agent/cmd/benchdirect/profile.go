package main

// exp26 Task B instrumentation: opt-in CPU/heap profiling of the benchdirect
// sender process, plus allocation and GC deltas over the transfer window.
//
// Everything here is inert unless -cpuprofile / -memprofile is passed, so the
// default harness behaviour is unchanged.  Remove this file and the two flag
// registrations in main.go to revert.

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"time"
)

// dataDatagramsPerGB is the measured v1 wire rate (exp24): 896.6 data
// datagrams per MiB of application payload.
const dataDatagramsPerGB = 918000.0

type goProfiler struct {
	cpuFile *os.File
	mem0    runtime.MemStats
	gc0     float64
	t0      time.Time
	started bool
}

func startGoProfiling(cpuPath string) (*goProfiler, error) {
	p := &goProfiler{t0: time.Now()}
	runtime.ReadMemStats(&p.mem0)
	p.gc0 = p.mem0.GCCPUFraction
	if cpuPath != "" {
		f, err := os.Create(cpuPath)
		if err != nil {
			return nil, fmt.Errorf("cpuprofile: %w", err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("cpuprofile: %w", err)
		}
		p.cpuFile = f
	}
	p.started = true
	return p, nil
}

type profileReport struct {
	BytesReceived int64   `json:"bytes_received"`
	GoCPUSeconds  float64 `json:"go_cpu_seconds"`
	WallS         float64 `json:"wall_s"`
	AllocBytes    uint64  `json:"alloc_bytes"`
	Mallocs       uint64  `json:"mallocs"`
	Frees         uint64  `json:"frees"`
	NumGC         uint32  `json:"num_gc"`
	GCPauseNs     uint64  `json:"gc_pause_ns"`
	GCCPUFraction float64 `json:"gc_cpu_fraction_delta"`

	AllocBytesPerGB       float64 `json:"alloc_bytes_per_gb_payload"`
	AllocBytesPerDatagram float64 `json:"alloc_bytes_per_data_datagram"`
	ObjectsPerGB          float64 `json:"objects_per_gb_payload"`
	ObjectsPerDatagram    float64 `json:"objects_per_data_datagram"`
	GCPausePctOfGoCPU     float64 `json:"gc_pause_pct_of_go_cpu"`
	AllocBytesPerCPUSec   float64 `json:"alloc_bytes_per_cpu_s"`
}

func (r profileReport) print() {
	b, _ := json.Marshal(r)
	fmt.Fprintln(os.Stderr, "PROFILE "+string(b))
}

// Stop writes the profiles and returns the window deltas.
func (p *goProfiler) Stop(memPath string, goCPUSec float64, received int64) profileReport {
	var mem1 runtime.MemStats
	runtime.ReadMemStats(&mem1) // before stopping the CPU profile, so the
	// ReadMemStats work itself is attributed consistently.

	if p.cpuFile != nil {
		pprof.StopCPUProfile()
		_ = p.cpuFile.Close()
	}
	if memPath != "" {
		if f, err := os.Create(memPath); err == nil {
			runtime.GC()
			_ = pprof.WriteHeapProfile(f)
			_ = f.Close()
		}
	}

	rep := profileReport{
		BytesReceived: received,
		GoCPUSeconds:  goCPUSec,
		WallS:         time.Since(p.t0).Seconds(),
		AllocBytes:    mem1.TotalAlloc - p.mem0.TotalAlloc,
		Mallocs:       mem1.Mallocs - p.mem0.Mallocs,
		Frees:         mem1.Frees - p.mem0.Frees,
		NumGC:         mem1.NumGC - p.mem0.NumGC,
		GCPauseNs:     mem1.PauseTotalNs - p.mem0.PauseTotalNs,
		GCCPUFraction: mem1.GCCPUFraction - p.gc0,
	}
	if received > 0 {
		gb := float64(received) / 1e9
		rep.AllocBytesPerGB = float64(rep.AllocBytes) / gb
		rep.ObjectsPerGB = float64(rep.Mallocs) / gb
		if dg := gb * dataDatagramsPerGB; dg > 0 {
			rep.AllocBytesPerDatagram = float64(rep.AllocBytes) / dg
			rep.ObjectsPerDatagram = float64(rep.Mallocs) / dg
		}
	}
	if goCPUSec > 0 {
		rep.GCPausePctOfGoCPU = float64(rep.GCPauseNs) / 1e9 / goCPUSec * 100
		rep.AllocBytesPerCPUSec = float64(rep.AllocBytes) / goCPUSec
	}
	return rep
}
