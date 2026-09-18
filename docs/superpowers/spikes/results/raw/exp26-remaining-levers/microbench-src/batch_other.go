//go:build !linux

package main

import (
	"errors"
	"net"
)

// macOS (darwin) exposes neither sendmmsg nor UDP_SEGMENT to userspace:
// golang.org/x/sys/unix@v0.47.0 has no Sendmmsg symbol at all, and the macOS
// SDK's sys/socket.h defines no UDP_SEGMENT option.  These paths therefore
// compile but refuse to run, so a darwin run reports "unsupported" rather than
// silently measuring something else.
var errUnsupported = errors.New("sendmmsg/UDP_SEGMENT not available on this platform (darwin)")

func probeBatchedSupported() error { return errUnsupported }

func enableBatching(_ *net.UDPConn, _, _ int, _ string) error { return errUnsupported }

func sendBatch(_ *net.UDPConn, _ *net.UDPAddr, _ []byte, _, _ int) error { return errUnsupported }

