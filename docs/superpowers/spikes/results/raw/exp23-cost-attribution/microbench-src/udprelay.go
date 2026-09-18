// exp23 microbenchmark: UDP datagram relay cost, replicating the harness shim's
// hot path (ReadFromUDP -> copy -> optional per-datagram time.Timer -> optional
// token bucket -> WriteToUDP) so the shim's share of go_cpu_seconds can be
// attributed by measurement instead of a code-read estimate.
//
// Two processes: "sink" (like the browser receiver) and "sendrelay" (like
// benchdirect: pion sender + in-process shim).  Only the sendrelay process'
// CPU is reported, because in benchdirect only that process' CPU is charged to
// go_cpu_seconds.
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"time"
)

func cpuSeconds() float64 {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 +
		float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
}

func main() {
	mode := flag.String("mode", "sendrelay", "sink|sendrelay|sendonly")
	port := flag.Int("port", 9201, "sink port")
	relayPort := flag.Int("relayport", 9200, "relay bind port")
	total := flag.Int64("total", 64<<20, "payload bytes")
	dg := flag.Int("dg", 1200, "datagram payload bytes")
	delayUs := flag.Int("delayus", 0, "per-datagram timer wait in microseconds (like the shim)")
	rateMB := flag.Int64("rate", 0, "token-bucket bytes/sec cap (0=off)")
	sendPaceUs := flag.Int("sendpaceus", 0, "sleep in the *sender* goroutine per datagram (models a flow-controlled sender)")
	flag.Parse()

	if *mode == "sink" {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: *port})
		if err != nil {
			panic(err)
		}
		_ = conn.SetReadBuffer(16 << 20)
		buf := make([]byte, 65536)
		var got int64
		for got < *total {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				panic(err)
			}
			got += int64(n)
		}
		fmt.Fprintf(os.Stderr, "sink got %d bytes\n", got)
		return
	}

	// sender socket (models pion's UDP send side)
	sendConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		panic(err)
	}
	defer sendConn.Close()
	sinkAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: *port}

	c0 := cpuSeconds()
	t0 := time.Now()
	payload := make([]byte, *dg)

	if *mode == "sendonly" {
		var sent int64
		for sent < *total {
			n := *dg
			if rem := *total - sent; rem < int64(n) {
				n = int(rem)
			}
			if _, err := sendConn.WriteToUDP(payload[:n], sinkAddr); err != nil {
				panic(err)
			}
			sent += int64(n)
		}
		report(*mode, t0, c0, *total, 0, *delayUs, *rateMB, *dg)
		return
	}

	// relay socket = the shim's address that the sender writes to
	relayConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: *relayPort})
	if err != nil {
		panic(err)
	}
	defer relayConn.Close()
	_ = relayConn.SetReadBuffer(16 << 20)
	_ = relayConn.SetWriteBuffer(16 << 20)
	relayAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: *relayPort}

	var fwd, drop int64
	var tokens float64
	burst := float64(*rateMB) * 0.1
	tokens = burst
	last := time.Now()

	var sentAtomic int64
	go func() {
		var sent int64
		for sent < *total {
			n := *dg
			if rem := *total - sent; rem < int64(n) {
				n = int(rem)
			}
			for {
				if _, err := sendConn.WriteToUDP(payload[:n], relayAddr); err == nil {
					break
				}
			}
			sent += int64(n)
			atomic.StoreInt64(&sentAtomic, sent)
			if *sendPaceUs > 0 {
				time.Sleep(time.Duration(*sendPaceUs) * time.Microsecond)
			}
		}
	}()

	buf := make([]byte, 65536)
	for fwd < *total {
		n, _, err := relayConn.ReadFromUDP(buf)
		if err != nil {
			panic(err)
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		if *delayUs > 0 {
			timer := time.NewTimer(time.Duration(*delayUs) * time.Microsecond)
			<-timer.C
		}
		if *rateMB > 0 {
			for {
				now := time.Now()
				tokens += now.Sub(last).Seconds() * float64(*rateMB)
				last = now
				if tokens > burst {
					tokens = burst
				}
				if tokens >= float64(len(data)) {
					tokens -= float64(len(data))
					break
				}
				time.Sleep(time.Duration((float64(len(data))-tokens)/float64(*rateMB)*1e9) * time.Nanosecond)
			}
		}
		if _, err := relayConn.WriteToUDP(data, sinkAddr); err != nil {
			drop++
			continue
		}
		fwd += int64(len(data))
	}
	_ = atomic.LoadInt64(&sentAtomic)
	report(*mode, t0, c0, fwd, drop, *delayUs, *rateMB, *dg)
}

func report(mode string, t0 time.Time, c0 float64, bytes, drop int64, delayUs int, rate int64, dg int) {
	wall := time.Since(t0)
	cpu := cpuSeconds() - c0
	gb := float64(bytes) / 1e9
	fmt.Fprintf(os.Stderr, "mode=%s bytes=%d wall=%.3fs cpu=%.3fs drop=%d\n", mode, bytes, wall.Seconds(), cpu, drop)
	fmt.Printf("{\"bench\":\"udprelay\",\"mode\":%q,\"dg\":%d,\"delay_us\":%d,\"rate_bps\":%d,\"bytes\":%d,\"wall_s\":%.4f,\"cpu_s\":%.4f,\"cpu_s_per_gb\":%.3f,\"us_per_datagram\":%.4f}\n",
		mode, dg, delayUs, rate, bytes, wall.Seconds(), cpu, cpu/gb, cpu/(float64(bytes)/float64(dg))*1e6)
}
