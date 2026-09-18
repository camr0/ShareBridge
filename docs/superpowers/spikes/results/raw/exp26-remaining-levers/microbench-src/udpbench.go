// exp26 microbenchmark: how much of the sender's CPU is *syscall* cost, and
// therefore what a syscall-batching fork lever (sendmmsg / UDP GSO) could buy.
//
// This file is deliberately standalone (no repo imports) so it can be run and
// re-run without rebuilding the harness.  Two processes, like exp23's udprelay:
//
//	sink     - binds and drains, models the peer/browser receiver
//	send     - the process whose CPU is charged (models the Go/pion sender)
//
// Modes:
//
//	write      one WriteToUDP(1237 B) per datagram        -- today's pion behaviour
//	writebig   one WriteToUDP(K*1237 B) per K datagrams   -- same bytes, 1/K syscalls
//	                                                         (measured proxy for a batched syscall)
//	connwrite  connected socket + Write(1237)             -- removes the sendto addr copy
//	mmsg       real sendmmsg(batch=K)                     -- LINUX ONLY
//	gso        real UDP_SEGMENT(batch=K)                  -- LINUX ONLY
//
// On darwin `mmsg`/`gso` report unsupported: macOS exposes neither.  The
// size-sweep + `writebig` proxy is then the honest route to a syscall-cost
// number (see -sweep), and the batching delta is arithmetic on top of it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// Defaults are the real path's numbers (exp24): 1237 B on the wire per data
// datagram, ~918 k data datagrams per GB of application payload.
const (
	defaultDG            = 1237
	defaultDatagramsPerGB = 918000
)

func cpuSeconds() float64 {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 +
		float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
}

func connConnect(conn *net.UDPConn, addr *net.UDPAddr) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	ip := addr.IP.To4()
	if ip == nil {
		return fmt.Errorf("connect: non-IPv4 address")
	}
	sa := &syscall.SockaddrInet4{Port: addr.Port}
	copy(sa.Addr[:], ip)
	var serr error
	if err := raw.Control(func(fd uintptr) { serr = syscall.Connect(int(fd), sa) }); err != nil {
		return err
	}
	return serr
}

// gMode is the selected send mode, needed by the platform-specific batch impl.
var gMode string

type result struct {
	Bench          string  `json:"bench"`
	Mode           string  `json:"mode"`
	DG             int     `json:"dg"`
	Batch          int     `json:"batch"`
	Datagrams      int64   `json:"datagrams"`
	Syscalls       int64   `json:"syscalls"`
	Bytes          int64   `json:"bytes"`
	WallS          float64 `json:"wall_s"`
	CPUS          float64 `json:"cpu_s"`
	CPUSPerGB      float64 `json:"cpu_s_per_gb_wire"`
	CPUSPerGBPayload float64 `json:"cpu_s_per_gb_payload"`
	NsPerDatagram  float64 `json:"ns_per_datagram"`
	NsPerSyscall   float64 `json:"ns_per_syscall"`
	Note           string  `json:"note,omitempty"`
}

func emit(r result) {
	b, _ := json.Marshal(r)
	fmt.Println(string(b))
}

func main() {
	mode := flag.String("mode", "send", "send|sink")
	smode := flag.String("smode", "write", "write|writebig|connwrite|mmsg|gso")
	port := flag.Int("port", 9301, "sink port")
	dg := flag.Int("dg", defaultDG, "datagram payload bytes")
	batch := flag.Int("batch", 8, "datagrams per batched syscall (writebig/mmsg/gso)")
	total := flag.Int64("total", 918000, "number of datagrams to send")
	runs := flag.Int("runs", 5, "measured repeats (reported individually)")
	flag.Parse()

	if *mode == "sink" {
		runSink(*port, *dg, *batch, *total, *smode)
		return
	}
	runSender(*smode, *port, *dg, *batch, *total, *runs)
}

func runSink(port, dg, batch int, total int64, smode string) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sink listen:", err)
		os.Exit(1)
	}
	_ = conn.SetReadBuffer(16 << 20)
	// Expect the full byte volume; for writebig/gso the wire is still ~dg*batch
	// per syscall but the datagram count differs, so just drain until the
	// sender is gone (read loop until a long idle gap).
	buf := make([]byte, 256<<10)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var got, dgs int64
	for {
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			break
		}
		got += int64(n)
		dgs++
	}
	fmt.Fprintf(os.Stderr, "sink dg=%d batch=%d smode=%s bytes=%d datagrams=%d\n", dg, batch, smode, got, dgs)
}

func runSender(smode string, port, dg, batch int, total int64, runs int) {
	if smode == "mmsg" || smode == "gso" {
		if err := probeBatchedSupported(); err != nil {
			emit(result{Bench: "udpbench", Mode: smode, DG: dg, Batch: batch,
				Note: "unsupported on this platform: " + err.Error()})
			return
		}
	}

	gMode = smode
	for rep := 1; rep <= runs; rep++ {
		sinkAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			fmt.Fprintln(os.Stderr, "dial:", err)
			os.Exit(1)
		}
		_ = conn.SetWriteBuffer(16 << 20)

		payload := make([]byte, dg*batch)
		var syscalls int64

		c0 := cpuSeconds()
		t0 := time.Now()

		switch smode {
		case "write":
			syscalls = runPlainWrite(conn, sinkAddr, payload[:dg], total)
		case "writebig":
			syscalls = runPlainWrite(conn, sinkAddr, payload[:dg*batch], total/int64(batch))
		case "connwrite":
			if err := connConnect(conn, sinkAddr); err != nil {
				fmt.Fprintln(os.Stderr, "connect:", err)
				os.Exit(1)
			}
			syscalls = runPlainWrite(conn, nil, payload[:dg], total)
		default:
			syscalls = runBatched(conn, sinkAddr, payload, dg, batch, total)
		}

		wall := time.Since(t0).Seconds()
		cpu := cpuSeconds() - c0
		conn.Close()

		nDatagrams := total
		if smode == "writebig" {
			nDatagrams = total
		}
		bytes := int64(dg) * nDatagrams
		gbWire := float64(bytes) / 1e9
		gbPayload := float64(total) / float64(defaultDatagramsPerGB)

		r := result{
			Bench: "udpbench", Mode: smode, DG: dg, Batch: batch,
			Datagrams: nDatagrams, Syscalls: syscalls, Bytes: bytes,
			WallS: wall, CPUS: cpu,
			CPUSPerGB:        cpu / gbWire,
			CPUSPerGBPayload: cpu / gbPayload,
			NsPerDatagram:    cpu / float64(nDatagrams) * 1e9,
			NsPerSyscall:     cpu / float64(syscalls) * 1e9,
		}
		r.Note = fmt.Sprintf("rep=%d", rep)
		emit(r)
	}
}

func runPlainWrite(conn *net.UDPConn, addr *net.UDPAddr, payload []byte, n int64) int64 {
	var syscalls int64
	for i := int64(0); i < n; i++ {
		var err error
		if addr == nil {
			_, err = conn.Write(payload)
		} else {
			_, err = conn.WriteToUDP(payload, addr)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
		syscalls++
	}
	return syscalls
}

// runBatched writes the SAME total datagram count as runPlainWrite but in
// batched syscalls.  It is used for the size-sweep proxy (see -sweep in the
// runner); it deliberately reuses the datagram-size identity of the mode.
func runBatched(conn *net.UDPConn, addr *net.UDPAddr, payload []byte, dg, batch int, total int64) int64 {
	if err := enableBatching(conn, dg, batch, gMode); err != nil {
		fmt.Fprintln(os.Stderr, "enable batching:", err)
		os.Exit(1)
	}
	nfull := total / int64(batch)
	rem := total % int64(batch)
	var syscalls int64
	for i := int64(0); i < nfull; i++ {
		if err := sendBatch(conn, addr, payload, dg, batch); err != nil {
			fmt.Fprintln(os.Stderr, "sendbatch:", err)
			os.Exit(1)
		}
		syscalls++
	}
	if rem > 0 {
		if err := sendBatch(conn, addr, payload, dg, int(rem)); err != nil {
			fmt.Fprintln(os.Stderr, "sendbatch rem:", err)
			os.Exit(1)
		}
		syscalls++
	}
	return syscalls
}
