package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	mode := flag.String("mode", "raw", "raw|prod|https|fetch")
	rtt := flag.Int("rtt", 0, "added RTT in ms")
	loss := flag.Float64("loss", 0, "packet loss fraction 0..1")
	size := flag.String("size", "512MiB", "total bytes to send (e.g. 8MiB)")
	chunk := flag.String("chunk", "16KiB", "bytes per send (e.g. 16KiB)")
	backpressure := flag.String("backpressure", "event", "event|poll (mode A only)")
	deadline := flag.Int("deadline", 30, "receive deadline in seconds")
	window := flag.String("window", "5MiB", "backpressure window (raw mode)")
	mincwnd := flag.String("mincwnd", "0", "minimum SCTP congestion window (e.g. 2MiB), 0 = default")
	jitter := flag.Int("jitter", 0, "per-packet delay jitter in ms (uniform +/-)")
	bandwidth := flag.String("bandwidth", "0", "link bandwidth cap (e.g. 8MB), 0 = unlimited")
	out := flag.String("out", "-", "JSON output path (default stdout)")
	url := flag.String("url", "", "fetch target URL (fetch mode)")
	reps := flag.Int("reps", 12, "number of fetch reps (fetch mode)")
	flag.Parse()

	sizeBytes, sizeErr := parseByteSize(*size)
	if sizeErr != nil {
		fmt.Fprintln(os.Stderr, "invalid -size:", sizeErr)
		os.Exit(2)
	}
	chunkBytes, chunkErr := parseByteSize(*chunk)
	if chunkErr != nil {
		fmt.Fprintln(os.Stderr, "invalid -chunk:", chunkErr)
		os.Exit(2)
	}
	if !chunkFitsInt(chunkBytes) {
		fmt.Fprintln(os.Stderr, "invalid -chunk: value exceeds platform int range")
		os.Exit(2)
	}
	windowBytes, windowErr := parseByteSize(*window)
	if windowErr != nil {
		fmt.Fprintln(os.Stderr, "invalid -window:", windowErr)
		os.Exit(2)
	}
	minCwndBytes, minCwndErr := parseByteSize(*mincwnd)
	if minCwndErr != nil {
		fmt.Fprintln(os.Stderr, "invalid -mincwnd:", minCwndErr)
		os.Exit(2)
	}
	bandwidthBytes, bandwidthErr := parseByteSize(*bandwidth)
	if bandwidthErr != nil {
		fmt.Fprintln(os.Stderr, "invalid -bandwidth:", bandwidthErr)
		os.Exit(2)
	}

	cfg := runConfig{
		mode:         *mode,
		rttMs:        *rtt,
		loss:         *loss,
		size:         sizeBytes,
		chunk:        int(chunkBytes),
		backpressure: *backpressure,
		deadline:     time.Duration(*deadline) * time.Second,
		window:       windowBytes,
		minCwnd:      minCwndBytes,
		jitter:       time.Duration(*jitter) * time.Millisecond,
		bandwidth:    bandwidthBytes,
	}
	if err := validateRunConfig(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if cfg.mode == "fetch" {
		if *url == "" {
			fmt.Fprintln(os.Stderr, "invalid -url: must be non-empty for fetch mode")
			os.Exit(2)
		}
		if *reps < 1 {
			fmt.Fprintln(os.Stderr, "invalid -reps: must be >= 1")
			os.Exit(2)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var (
		res rawResult
		err error
	)
	switch cfg.mode {
	case "raw":
		res, err = runRaw(ctx, cfg)
	case "prod":
		res, err = runProd(ctx, cfg)
	case "https":
		if err := runHTTPServer(ctx, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "bench failed:", err)
			os.Exit(1)
		}
		return
	case "fetch":
		results, ferr := fetch(ctx, *url, cfg.size, cfg.rttMs, *reps)
		if ferr != nil {
			fmt.Fprintln(os.Stderr, "bench failed:", ferr)
			os.Exit(1)
		}
		med, mn, mx, p95 := summarize(results)
		fmt.Fprintf(os.Stderr, "fetch median=%.2f min=%.2f max=%.2f p95=%.2f Mbps\n", med, mn, mx, p95)
		if err := writeFetchResult(*out, results); err != nil {
			fmt.Fprintln(os.Stderr, "write output:", err)
			os.Exit(1)
		}
		return
	default:
		fmt.Fprintln(os.Stderr, "invalid -mode:", cfg.mode)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench failed:", err)
		os.Exit(1)
	}

	fmt.Fprintln(os.Stderr, humanSummary(res))
	if *out == "-" {
		if err := writeJSON(os.Stdout, res); err != nil {
			fmt.Fprintln(os.Stderr, "write output:", err)
			os.Exit(1)
		}
		return
	}
	if err := writeResult(*out, res); err != nil {
		fmt.Fprintln(os.Stderr, "write output:", err)
		os.Exit(1)
	}
}

func validateRunConfig(cfg runConfig) error {
	if cfg.mode != "raw" && cfg.mode != "prod" && cfg.mode != "https" && cfg.mode != "fetch" {
		return fmt.Errorf("invalid -mode %q: must be raw, prod, https, or fetch", cfg.mode)
	}
	if cfg.rttMs < 0 {
		return fmt.Errorf("invalid -rtt %d: must be >= 0", cfg.rttMs)
	}
	if cfg.size <= 0 {
		return fmt.Errorf("invalid -size %d: must be > 0", cfg.size)
	}
	if cfg.chunk <= 0 {
		return fmt.Errorf("invalid -chunk %d: must be > 0", cfg.chunk)
	}
	if cfg.window <= 0 {
		return fmt.Errorf("invalid -window %d: must be > 0", cfg.window)
	}
	if cfg.minCwnd < 0 {
		return fmt.Errorf("invalid -mincwnd %d: must be >= 0", cfg.minCwnd)
	}
	if cfg.bandwidth < 0 {
		return fmt.Errorf("invalid -bandwidth %d: must be >= 0", cfg.bandwidth)
	}
	if cfg.deadline <= 0 {
		return fmt.Errorf("invalid -deadline %v: must be > 0", cfg.deadline)
	}
	if math.IsNaN(cfg.loss) || math.IsInf(cfg.loss, 0) {
		return fmt.Errorf("invalid -loss %v: must be finite", cfg.loss)
	}
	if cfg.loss < 0 || cfg.loss > 1 {
		return fmt.Errorf("invalid -loss %v: must be between 0 and 1", cfg.loss)
	}
	if cfg.mode == "raw" {
		switch cfg.backpressure {
		case "event", "poll":
		default:
			return fmt.Errorf("invalid -backpressure %q: must be event or poll", cfg.backpressure)
		}
	}
	return nil
}

// chunkFitsInt reports whether b can be stored in a platform int without
// truncation.
func chunkFitsInt(b int64) bool {
	return b <= int64(int(^uint(0)>>1))
}

func writeResult(out string, res rawResult) (err error) {
	if out == "-" {
		return writeJSON(os.Stdout, res)
	}
	f, err := os.Create(out)
	if err != nil {
		return fmt.Errorf("open output: %w", err)
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("close output: %w", cerr)
		}
	}()
	if err := writeJSON(f, res); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

// writeFetchResult writes the []fetchResult array as JSON, mirroring the
// rawResult path while leaving the raw/prod JSON shape untouched.
func writeFetchResult(out string, res []fetchResult) (err error) {
	if out == "-" {
		return json.NewEncoder(os.Stdout).Encode(res)
	}
	f, err := os.Create(out)
	if err != nil {
		return fmt.Errorf("open output: %w", err)
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("close output: %w", cerr)
		}
	}()
	if err := json.NewEncoder(f).Encode(res); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	upper := strings.ToUpper(s)
	for _, unit := range []struct {
		suffix string
		mult   int64
	}{
		{suffix: "KIB", mult: 1 << 10},
		{suffix: "MIB", mult: 1 << 20},
		{suffix: "GIB", mult: 1 << 30},
		{suffix: "KB", mult: 1000},
		{suffix: "MB", mult: 1000 * 1000},
		{suffix: "GB", mult: 1000 * 1000 * 1000},
		{suffix: "B", mult: 1},
	} {
		if strings.HasSuffix(upper, unit.suffix) {
			v := strings.TrimSpace(s[:len(s)-len(unit.suffix)])
			if v == "" {
				return 0, fmt.Errorf("missing numeric value in %q", s)
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return 0, err
			}
			if n > math.MaxInt64/unit.mult {
				return 0, fmt.Errorf("size %q overflows int64", s)
			}
			return n * unit.mult, nil
		}
	}
	return strconv.ParseInt(s, 10, 64)
}
