package main

import (
	"strings"
	"testing"
)

const sampleSDP = "v=0\r\n" +
	"o=- 1 1 IN IP4 127.0.0.1\r\n" +
	"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n" +
	"c=IN IP4 0.0.0.0\r\n" +
	"a=candidate:842163049 1 udp 2130706431 127.0.0.1 49439 typ host generation 0\r\n" +
	"a=ice-ufrag:abc\r\n" +
	"a=ice-pwd:def\r\n"

func TestRewriteHostCandidate(t *testing.T) {
	got, ip, port, err := RewriteHostCandidate(sampleSDP, 5001)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "127.0.0.1" || port != 49439 {
		t.Fatalf("orig = %s:%d, want 127.0.0.1:49439", ip, port)
	}
	if !strings.Contains(got, "127.0.0.1 5001 typ host") {
		t.Fatalf("rewritten candidate missing:\n%s", got)
	}
	if strings.Contains(got, "127.0.0.1 49439 typ host") {
		t.Fatalf("original port still present:\n%s", got)
	}
	if !strings.Contains(got, "a=ice-ufrag:abc") {
		t.Fatal("unrelated line corrupted")
	}
}

func TestRewriteHostCandidateRewritesNonLoopbackIP(t *testing.T) {
	sdp := "v=0\r\n" +
		"a=candidate:2935132940 1 udp 2113937151 192.168.1.224 51357 typ host generation 0\r\n"
	got, ip, port, err := RewriteHostCandidate(sdp, 5001)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "192.168.1.224" || port != 51357 {
		t.Fatalf("orig = %s:%d, want 192.168.1.224:51357", ip, port)
	}
	if !strings.Contains(got, "127.0.0.1 5001 typ host") {
		t.Fatalf("IP+port not rewritten to loopback:\n%s", got)
	}
}

func TestRewriteHostCandidateStripsOtherCandidates(t *testing.T) {
	sdp := "v=0\r\n" +
		"a=candidate:111 1 udp 2113937151 192.168.1.224 51357 typ host generation 0\r\n" +
		"a=candidate:222 1 udp 2113939711 2600:4040::1 63045 typ host generation 0\r\n" +
		"a=candidate:333 1 udp 2113937151 10.0.0.5 7000 typ host generation 0\r\n"
	got, _, _, err := RewriteHostCandidate(sdp, 5001)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(got, "a=candidate:") != 1 {
		t.Fatalf("expected exactly one candidate, got:\n%s", got)
	}
	if !strings.Contains(got, "127.0.0.1 5001 typ host") {
		t.Fatalf("missing rewritten candidate:\n%s", got)
	}
}

func TestRewriteNoHostCandidate(t *testing.T) {
	_, _, _, err := RewriteHostCandidate("v=0\r\n", 5001)
	if err == nil {
		t.Fatal("expected error for missing host candidate")
	}
}
