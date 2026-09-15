package main

import (
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// chromeCPUSeconds returns cumulative CPU seconds across all Google Chrome
// processes, or 0 if it cannot be determined.
func chromeCPUSeconds() float64 {
	out, err := exec.Command("/bin/ps", "-Ao", "cputime=,comm=").Output()
	if err != nil {
		return 0
	}
	var total float64
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, " ", 2)
		if len(fields) != 2 {
			continue
		}
		if !strings.Contains(fields[1], "Google Chrome") && !strings.Contains(fields[1], "Chromium") {
			continue
		}
		total += parseCPUTime(fields[0])
	}
	return total
}

// parseCPUTime parses ps cputime formats: SS.ss, MM:SS.ss or HH:MM:SS.ss.
func parseCPUTime(s string) float64 {
	var total float64
	for _, part := range strings.Split(strings.TrimSpace(s), ":") {
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			return 0
		}
		total = total*60 + v
	}
	return total
}

type cpuSampler struct {
	stop     chan struct{}
	done     chan struct{}
	start    time.Time
	startCPU float64
	mu       sync.Mutex
	lastCPU  float64
}

// startCPUSampler polls Chrome's cumulative CPU time while the transfer runs, so
// a throughput plateau at high connection counts can be blamed on CPU rather
// than on the network model.
func startCPUSampler() *cpuSampler {
	c := &cpuSampler{stop: make(chan struct{}), done: make(chan struct{}), start: time.Now()}
	c.startCPU = chromeCPUSeconds()
	c.lastCPU = c.startCPU
	go func() {
		defer close(c.done)
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-c.stop:
				v := chromeCPUSeconds()
				c.mu.Lock()
				c.lastCPU = v
				c.mu.Unlock()
				return
			case <-t.C:
				v := chromeCPUSeconds()
				c.mu.Lock()
				c.lastCPU = v
				c.mu.Unlock()
			}
		}
	}()
	return c
}

// Stop returns CPU seconds consumed during the sampling window and the average
// number of cores kept busy.
func (c *cpuSampler) Stop() (seconds, cores float64) {
	close(c.stop)
	<-c.done
	c.mu.Lock()
	defer c.mu.Unlock()
	seconds = c.lastCPU - c.startCPU
	if elapsed := time.Since(c.start).Seconds(); elapsed > 0 {
		cores = seconds / elapsed
	}
	if seconds < 0 {
		return 0, 0
	}
	return seconds, cores
}

func timevalSeconds(t syscall.Timeval) float64 {
	return float64(t.Sec) + float64(t.Usec)/1e6
}

// selfCPUSeconds returns cumulative CPU seconds consumed by this process.
func selfCPUSeconds() float64 {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0
	}
	return timevalSeconds(ru.Utime) + timevalSeconds(ru.Stime)
}
