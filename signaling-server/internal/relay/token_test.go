package relay

import (
	"testing"
	"time"
)

func TestIssueAndValidate_roundTrip(t *testing.T) {
	issuer := NewIssuer([]byte("test-secret-do-not-use-in-prod-abcd1234"), 5*time.Minute)

	claims := Claims{
		ShareCode:     "abc12345",
		BrowserPeerID: "12D3KooWBrowser",
		RelayAllowed:  true,
		DCUtRAllowed:  true,
	}
	tok, err := issuer.Issue(claims)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	got, err := issuer.Validate(tok)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got.ShareCode != claims.ShareCode || got.BrowserPeerID != claims.BrowserPeerID ||
		got.RelayAllowed != claims.RelayAllowed || got.DCUtRAllowed != claims.DCUtRAllowed {
		t.Fatalf("claims mismatch: got %+v want %+v", got, claims)
	}
	if got.JTI == "" {
		t.Fatal("JTI empty")
	}
}

func TestValidate_rejectsTamperedClaim(t *testing.T) {
	issuer := NewIssuer([]byte("test-secret-do-not-use-in-prod-abcd1234"), time.Minute)
	tok, _ := issuer.Issue(Claims{ShareCode: "abc12345", RelayAllowed: true})
	parts := []byte(tok)
	for i, c := range parts {
		if c == '.' {
			parts[i+5] = parts[i+5] ^ 0x01
			break
		}
	}
	if _, err := issuer.Validate(string(parts)); err == nil {
		t.Fatal("expected signature error")
	}
}

func TestValidate_rejectsExpired(t *testing.T) {
	issuer := NewIssuer([]byte("test-secret-do-not-use-in-prod-abcd1234"), time.Millisecond)
	tok, _ := issuer.Issue(Claims{ShareCode: "abc12345"})
	time.Sleep(10 * time.Millisecond)
	if _, err := issuer.Validate(tok); err == nil {
		t.Fatal("expected expiry error")
	}
}

func TestJTIStore_firstUseAcceptedSecondRejected(t *testing.T) {
	store := NewJTIStore(5 * time.Minute)
	defer store.Close()
	if !store.ConsumeOnce("jti-A") {
		t.Fatal("first use should be accepted")
	}
	if store.ConsumeOnce("jti-A") {
		t.Fatal("second use should be rejected")
	}
}

func TestJTIStore_expiredEntriesPruned(t *testing.T) {
	store := &JTIStore{
		entries: make(map[string]time.Time),
		ttl:     20 * time.Millisecond,
		stop:    make(chan struct{}),
	}
	store.ConsumeOnce("jti-B")
	time.Sleep(40 * time.Millisecond)
	store.prune()
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.entries["jti-B"]; ok {
		t.Fatal("expected jti-B to be pruned")
	}
}