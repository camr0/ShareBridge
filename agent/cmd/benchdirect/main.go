package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
)

func main() {
	mode := flag.String("mode", "raw", "raw|prod")
	rtt := flag.Int("rtt", 0, "added RTT in ms")
	loss := flag.Float64("loss", 0, "packet loss fraction 0..1")
	size := flag.String("size", "512MiB", "total bytes to send (e.g. 8MiB)")
	chunk := flag.String("chunk", "16KiB", "bytes per send (e.g. 16KiB)")
	backpressure := flag.String("backpressure", "event", "event|poll (mode A only)")
	out := flag.String("out", "-", "JSON output path (default stdout)")
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

	cfg := runConfig{
		mode:         *mode,
		rttMs:        *rtt,
		loss:         *loss,
		size:         sizeBytes,
		chunk:        int(chunkBytes),
		backpressure: *backpressure,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
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
		_ = writeJSON(os.Stdout, res)
		return
	}
	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open output:", err)
		os.Exit(1)
	}
	defer f.Close()
	_ = writeJSON(f, res)
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
			return n * unit.mult, nil
		}
	}
	return strconv.ParseInt(s, 10, 64)
}
