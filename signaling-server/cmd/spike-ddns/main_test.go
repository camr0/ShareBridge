package main

import (
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// startDNSServer starts an in-process DNS server on a random loopback UDP port
// and returns its dial address. It serves both the recursive NS discovery and
// the authoritative A queries for the zone/probe under test.
func startDNSServer(t *testing.T, handler dns.Handler) (string, func()) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: handler}
	go func() { _ = srv.ActivateAndServe() }()
	return pc.LocalAddr().String(), func() { _ = srv.Shutdown() }
}

// withDNSServerOverrides points the package-level DNS hooks at addr for the
// duration of the test and restores them afterwards.
func withDNSServerOverrides(t *testing.T, addr string) {
	t.Helper()
	origResolver, origAddr, origPoll := recursiveResolver, authoritativeAddr, pollInterval
	recursiveResolver = addr
	authoritativeAddr = func(string) string { return addr }
	pollInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		recursiveResolver = origResolver
		authoritativeAddr = origAddr
		pollInterval = origPoll
	})
}

func TestResolveAuthoritative_Confirms(t *testing.T) {
	const wantIP = "192.0.2.10"
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		if len(r.Question) == 0 {
			_ = w.WriteMsg(m)
			return
		}
		q := r.Question[0]
		switch {
		case q.Qtype == dns.TypeNS && q.Name == "zone.test.":
			rr, _ := dns.NewRR("zone.test. 300 IN NS ns.test.")
			m.Answer = append(m.Answer, rr)
		case q.Qtype == dns.TypeA && q.Name == "probe.zone.test.":
			rr, _ := dns.NewRR(fmt.Sprintf("probe.zone.test. 300 IN A %s", wantIP))
			m.Answer = append(m.Answer, rr)
		}
		_ = w.WriteMsg(m)
	})
	addr, stop := startDNSServer(t, handler)
	defer stop()
	withDNSServerOverrides(t, addr)

	elapsed, err := resolveAuthoritative("zone.test", "probe.zone.test", wantIP, time.Second)
	if err != nil {
		t.Fatalf("resolveAuthoritative = %v", err)
	}
	if elapsed <= 0 {
		t.Fatalf("elapsed = %v, want > 0", elapsed)
	}
}

func TestResolveAuthoritative_PollsUntilItAppears(t *testing.T) {
	const wantIP = "192.0.2.10"
	var aQueries int32
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		if len(r.Question) == 0 {
			_ = w.WriteMsg(m)
			return
		}
		q := r.Question[0]
		switch {
		case q.Qtype == dns.TypeNS && q.Name == "zone.test.":
			rr, _ := dns.NewRR("zone.test. 300 IN NS ns.test.")
			m.Answer = append(m.Answer, rr)
		case q.Qtype == dns.TypeA && q.Name == "probe.zone.test.":
			ip := "192.0.2.99" // wrong until the second probe
			if atomic.AddInt32(&aQueries, 1) > 1 {
				ip = wantIP
			}
			rr, _ := dns.NewRR(fmt.Sprintf("probe.zone.test. 300 IN A %s", ip))
			m.Answer = append(m.Answer, rr)
		}
		_ = w.WriteMsg(m)
	})
	addr, stop := startDNSServer(t, handler)
	defer stop()
	withDNSServerOverrides(t, addr)

	if _, err := resolveAuthoritative("zone.test", "probe.zone.test", wantIP, time.Second); err != nil {
		t.Fatalf("resolveAuthoritative did not observe the eventual IP: %v", err)
	}
	if atomic.LoadInt32(&aQueries) < 2 {
		t.Fatalf("aQueries = %d, want >= 2 (polling must re-query)", aQueries)
	}
}

func TestResolveAuthoritative_TimesOut(t *testing.T) {
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		if len(r.Question) == 0 {
			_ = w.WriteMsg(m)
			return
		}
		q := r.Question[0]
		switch {
		case q.Qtype == dns.TypeNS && q.Name == "zone.test.":
			rr, _ := dns.NewRR("zone.test. 300 IN NS ns.test.")
			m.Answer = append(m.Answer, rr)
		case q.Qtype == dns.TypeA && q.Name == "probe.zone.test.":
			rr, _ := dns.NewRR("probe.zone.test. 300 IN A 192.0.2.99") // never the wanted IP
			m.Answer = append(m.Answer, rr)
		}
		_ = w.WriteMsg(m)
	})
	addr, stop := startDNSServer(t, handler)
	defer stop()
	withDNSServerOverrides(t, addr)

	if _, err := resolveAuthoritative("zone.test", "probe.zone.test", "192.0.2.10", 60*time.Millisecond); err == nil {
		t.Fatalf("resolveAuthoritative should time out when the authoritative servers never serve the wanted IP")
	}
}
