package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	var cfg runConfig
	var maxRxBufU uint

	flag.StringVar(&cfg.signalURL, "signal", "", "signaling origin or full URL, e.g. wss://share.example (required)")
	flag.StringVar(&cfg.code, "code", "", "share code to join, e.g. a1k6q46n (required)")
	flag.StringVar(&cfg.path, "path", "", "file path within the share to request; empty = pick the largest file from file_list")
	flag.StringVar(&cfg.password, "password", "", "share password (empty for password-less shares)")
	flag.DurationVar(&cfg.duration, "duration", 180*time.Second, "hard stop after this long")
	flag.BoolVar(&cfg.insecure, "insecure", false, "skip TLS verification (test signalling servers only)")
	flag.StringVar(&cfg.requestID, "request-id", "fieldrecv", "request_id used for the file_request")
	flag.BoolVar(&cfg.dumpControl, "dump-control", false, "echo every post-handshake control message to stderr")
	flag.UintVar(&maxRxBufU, "max-rx-buf", 0, "DIAGNOSTIC: override pion's 1 MiB SCTP advertised receive window (bytes); 0 = pion default")
	flag.Parse()
	cfg.maxRxBuf = uint32(maxRxBufU)

	if cfg.signalURL == "" || cfg.code == "" {
		fmt.Fprintln(os.Stderr, "usage: fieldrecv --signal wss://host --code <share> [--path file] [--duration 180s]")
		flag.PrintDefaults()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	res, err := run(ctx, cfg)
	fmt.Fprintln(os.Stderr, res.summaryLine())
	if err != nil {
		fmt.Fprintf(os.Stderr, "fieldrecv: %v\n", err)
		os.Exit(1)
	}
}
