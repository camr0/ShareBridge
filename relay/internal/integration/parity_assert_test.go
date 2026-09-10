package integration

import (
	"bytes"
	"strings"
	"testing"
)

// paritySample is a deterministic non-empty capture with a TLS-1.2-shaped
// record header so the record counter has something meaningful to count.
func paritySample() []byte {
	sample := []byte{0x16, 0x03, 0x03, 0x00, 0x08}
	sample = append(sample, []byte{0x01, 0x00, 0x00, 0x04, 0xaa, 0xbb, 0xcc, 0xdd}...)
	return sample
}

// TestParityComparatorRequiresExactLengthAndNonEmptyCaptures is the regression
// for the reviewed defect: the old assertion required `agent >= browser` plus
// an equal prefix, so an injected trailing byte passed and an empty agent tap
// read as "zero extra bytes". compareParity is a hard assertion.
func TestParityComparatorRequiresExactLengthAndNonEmptyCaptures(t *testing.T) {
	browser := paritySample()

	t.Run("identical non-empty captures produce equal digests and zero extra bytes", func(t *testing.T) {
		digest, err := compareParity(browser, append([]byte(nil), browser...))
		if err != nil {
			t.Fatalf("identical captures rejected: %v", err)
		}
		if digest.ExtraAtAgent != 0 {
			t.Fatalf("ExtraAtAgent = %d, want 0", digest.ExtraAtAgent)
		}
		if digest.Bytes != len(browser) || digest.AgentBytes != len(browser) {
			t.Fatalf("byte counts = browser %d agent %d, want %d/%d", digest.Bytes, digest.AgentBytes, len(browser), len(browser))
		}
		if digest.BrowserSHA256 == "" || digest.BrowserSHA256 != digest.AgentSHA256 {
			t.Fatalf("digests = browser %q agent %q, want equal non-empty", digest.BrowserSHA256, digest.AgentSHA256)
		}
	})

	t.Run("an injected trailing byte fails hard", func(t *testing.T) {
		injected := append(append([]byte(nil), browser...), 0x41)
		digest, err := compareParity(browser, injected)
		if err == nil {
			t.Fatal("an injected trailing byte passed the parity assertion")
		}
		if digest.ExtraAtAgent != 1 {
			t.Fatalf("ExtraAtAgent = %d, want 1 for the injected byte", digest.ExtraAtAgent)
		}
		if !strings.Contains(err.Error(), "extra") {
			t.Errorf("injected-byte error does not describe the extra byte: %v", err)
		}
	})

	t.Run("a truncated agent capture fails hard", func(t *testing.T) {
		if _, err := compareParity(browser, browser[:len(browser)-1]); err == nil {
			t.Fatal("a truncated agent capture passed the parity assertion")
		}
	})

	t.Run("an empty browser capture fails hard", func(t *testing.T) {
		if _, err := compareParity(nil, nil); err == nil {
			t.Fatal("an empty browser capture passed the parity assertion")
		}
	})

	t.Run("an empty agent capture fails hard", func(t *testing.T) {
		if _, err := compareParity(browser, nil); err == nil {
			t.Fatal("an empty agent capture passed the parity assertion (read as zero extra bytes)")
		}
	})

	t.Run("a same-length content flip fails hard", func(t *testing.T) {
		flipped := append([]byte(nil), browser...)
		flipped[len(flipped)-1] ^= 0xff
		if _, err := compareParity(browser, flipped); err == nil {
			t.Fatal("a same-length content flip passed the parity assertion")
		}
	})
}

// TestFragmentationVerdictRequiresExactRecordCount is the regression for the
// reviewed overclaim: the gate accepted `>= 3` records. The record-level
// fragmentation claim is only sound with the exact requested count.
func TestFragmentationVerdictRequiresExactRecordCount(t *testing.T) {
	if err := checkFragmentation(5, 5); err != nil {
		t.Fatalf("exact record count rejected: %v", err)
	}
	if err := checkFragmentation(2, 2); err != nil {
		t.Fatalf("exact two-record count rejected: %v", err)
	}
	for _, got := range []int{0, 1, 3, 4, 6, 7} {
		if err := checkFragmentation(got, 5); err == nil {
			t.Errorf("record count %d accepted where exactly 5 is required", got)
		}
	}
	if err := checkFragmentation(1, 1); err == nil {
		t.Error("a single-record ClientHello was accepted as a fragmentation proof")
	}
}

// TestDialTargetAssertionDetectsNonRelayTarget is the non-vacuity regression
// for the strengthened relay-path privacy test: the assertion must fail on an
// empty dial record (nothing observed) and on any non-relay dial target.
func TestDialTargetAssertionDetectsNonRelayTarget(t *testing.T) {
	const relayPort = 20000
	relayTarget := "127.0.0.1:20000"

	if err := checkDialTargets(nil, relayPort); err == nil {
		t.Error("an empty dial record was accepted; a vacuous privacy proof must fail")
	}
	if err := checkDialTargets([]string{relayTarget}, relayPort); err != nil {
		t.Errorf("a relay-only dial record was rejected: %v", err)
	}
	if err := checkDialTargets([]string{relayTarget, relayTarget}, relayPort); err != nil {
		t.Errorf("a repeated relay-only dial record was rejected: %v", err)
	}
	for _, target := range []string{
		"127.0.0.1:20001",
		"127.0.0.1:80",
		"127.0.0.1:443",
		"example.com:443",
		"10.0.0.5:20000",
		"127.0.0.1",
	} {
		if err := checkDialTargets([]string{relayTarget, target}, relayPort); err == nil {
			t.Errorf("non-relay dial target %q was accepted", target)
		}
	}
}

// TestCompareParityDigestReflectsAgentExtraBytes pins the digest's evidence
// fields to the actual captures (the gate emits them as §23.3 evidence).
func TestCompareParityDigestReflectsAgentExtraBytes(t *testing.T) {
	browser := paritySample()
	agent := append(append([]byte(nil), browser...), 0x00, 0x01)
	digest, err := compareParity(browser, agent)
	if err == nil {
		t.Fatal("expected the injected bytes to fail")
	}
	if digest.Bytes != len(browser) || digest.AgentBytes != len(agent) || digest.ExtraAtAgent != 2 {
		t.Fatalf("digest = %+v, want Bytes=%d AgentBytes=%d ExtraAtAgent=2", digest, len(browser), len(agent))
	}
	if bytes.Equal([]byte(digest.BrowserSHA256), []byte(digest.AgentSHA256)) {
		t.Fatal("digests of unequal captures must not be equal")
	}
}
