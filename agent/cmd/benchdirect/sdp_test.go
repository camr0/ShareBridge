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

func TestRewriteHostCandidatePort(t *testing.T) {
	got, orig, err := RewriteHostCandidatePort(sampleSDP, 5001)
	if err != nil {
		t.Fatal(err)
	}
	if orig != 49439 {
		t.Fatalf("orig port = %d, want 49439", orig)
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

func TestRewriteNoHostCandidate(t *testing.T) {
	_, _, err := RewriteHostCandidatePort("v=0\r\n", 5001)
	if err == nil {
		t.Fatal("expected error for missing host candidate")
	}
}

func TestRewriteIgnoresNonHost127001Candidate(t *testing.T) {
	sdp := "v=0\r\n" +
		"a=candidate:1 1 udp 2130706431 127.0.0.1 6000 typ srflx generation 0\r\n" +
		"a=candidate:2 1 udp 2130706431 127.0.0.1 7000 typ host generation 0\r\n"

	got, orig, err := RewriteHostCandidatePort(sdp, 5001)
	if err != nil {
		t.Fatal(err)
	}
	if orig != 7000 {
		t.Fatalf("orig port = %d, want 7000", orig)
	}
	if !strings.Contains(got, "127.0.0.1 6000 typ srflx generation 0") {
		t.Fatalf("non-host candidate was rewritten:\n%s", got)
	}
	if !strings.Contains(got, "127.0.0.1 5001 typ host generation 0") {
		t.Fatalf("host candidate missing rewrite:\n%s", got)
	}
	if strings.Contains(got, "127.0.0.1 7000 typ host generation 0") {
		t.Fatalf("original host port still present:\n%s", got)
	}
}
