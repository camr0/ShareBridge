// agent/internal/daemon/daemon_binder_generation_test.go
package daemon

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/direct"
	"sharebridge/agent/internal/immich"
)

// newGenerationDaemon builds a daemon with a real namespace/cert, a shared
// binder/server, an Immich-hydratable resolver, and the in-memory signaling
// mock — the fixture for the daemon-level authorization→registration fence.
func newGenerationDaemon(t *testing.T) (*Daemon, *mockSignalingClient, *directState) {
	t.Helper()
	cm, chainPEM := newBaselineCertFixture(t, t.TempDir())
	if err := cm.Install([]byte(chainPEM)); err != nil {
		t.Fatalf("install test cert: %v", err)
	}
	baseURL, host := newImmichTestServer(t)
	cfg := &config.Config{
		SignalingURL:      "ws://localhost:8080",
		APIKey:            "test-key",
		ImmichURL:         baseURL,
		ImmichAllowedHost: host,
		ImmichAPIKey:      "api",
	}
	st := newMockStore()
	sig := newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())
	sig.shareOrigin = func(code, _ string) string { return testOriginFor(strings.ToLower(code)) }
	ds := &directState{
		namespace:    testDirectNS,
		baseDomain:   testDirectBase,
		cert:         cm,
		ready:        true,
		gate:         direct.NewSignalGate(st.GetAgentID(), func(string, direct.RouteKind) bool { return true }),
		origins:      map[string]originPair{},
		relayOrigins: map[string]string{},
	}
	d := &Daemon{
		config:    cfg,
		store:     st,
		signaling: sig,
		resolver:  direct.NewResolverRegistry(),
		sessions:  make(map[string]*Session),
		direct:    ds,
	}
	d.syncDirectServe()
	t.Cleanup(func() {
		ds.mu.Lock()
		server := ds.server
		ds.mu.Unlock()
		if server != nil {
			server.CloseAllConns()
		}
	})
	return d, sig, ds
}

// TestRevokeBetweenAuthorizeAndBindingServesNothing is audit Critical #3 at the
// daemon level: a request passes Binder authorization, a revokeOrigin lands in
// the authorization→registration window, and the gated handler is then
// released. Assert (a) no stream is served, (b) the share's resolver entry is
// gone, so the revoked share cannot resolve content at all. The connection
// close itself is pinned by the direct-package fence test.
func TestRevokeBetweenAuthorizeAndBindingServesNothing(t *testing.T) {
	d, _, ds := newGenerationDaemon(t)
	const code = "IMMICHREVOKE1"
	origin := testOriginFor("immichrevoke1")
	if _, bound := d.bindOrigin(code, origin); !bound {
		t.Fatalf("bindOrigin must succeed for the test origin")
	}
	client, err := d.newImmichClient(code)
	if err != nil {
		t.Fatalf("newImmichClient: %v", err)
	}
	d.hydrateContentSession(&Session{Code: code, ShareType: "immich", immich: client})
	if d.resolver.Get(code) == nil {
		t.Fatalf("precondition: the share's resolver entry must be hydrated")
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	ds.server.SetTestHookAfterAuthorize(func() {
		close(entered)
		<-release
	})

	ts := httptest.NewUnstartedServer(ds.server.Handler())
	ts.TLS = ds.server.TLSConfig()
	ts.StartTLS()
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	req, err := http.NewRequest(http.MethodGet, "https://"+origin+"/s/"+code+"/download?size=4096", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = origin
	doReq := func() (*http.Response, error) {
		tr := &http.Transport{
			TLSClientConfig:   &tls.Config{ServerName: origin, InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}, // test-only: self-signed
			ForceAttemptHTTP2: false,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}
		return (&http.Client{Transport: tr}).Do(req)
	}
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := doReq()
		done <- result{resp, err}
	}()
	awaitRecv(t, entered, "handler entering the post-authorize window")

	d.revokeOrigin(code)
	if d.resolver.Get(code) != nil {
		t.Errorf("revokeOrigin must remove the share's resolver entry")
	}
	close(release)

	res := <-done
	if res.err == nil {
		defer res.resp.Body.Close()
		if res.resp.StatusCode == http.StatusOK {
			t.Errorf("a revoked share served a stream after revocation")
		}
	}
}

// assertNoHalfBinding asserts that a failed registration left no local or
// control-side trace.
func assertNoHalfBinding(t *testing.T, d *Daemon, ds *directState, sig *mockSignalingClient, code string) {
	t.Helper()
	if !sig.unregisteredCode(code) {
		t.Errorf("failed registration must roll back the control-side registration via unregisterShare")
	}
	label := strings.ToLower(code)
	directOrigin := testOriginFor(label)
	relayOrigin := relayOriginForLabel(label)

	ds.mu.Lock()
	_, hasPair := ds.origins[code]
	_, hasRelay := ds.relayOrigins[code]
	_, hasPending := ds.pendingRelayBinds[code]
	binder := ds.binder
	ds.mu.Unlock()
	if hasPair || hasRelay || hasPending {
		t.Errorf("failed registration left bookkeeping: pair=%v relay=%v pending=%v", hasPair, hasRelay, hasPending)
	}
	if binder != nil {
		if _, err := binder.AdmitSNI(directOrigin); err == nil {
			t.Errorf("failed registration left the direct admission installed")
		}
		if _, err := binder.AdmitSNI(relayOrigin); err == nil {
			t.Errorf("failed registration left the relay admission installed")
		}
	}
	if d.resolver.Get(code) != nil {
		t.Errorf("failed registration left resolver state installed")
	}
}

// TestFailedRegistrationRollsBackBinding is audit Important #2: a failure after
// the local admissions were installed (Immich client creation, session save)
// must roll back the admissions, bookkeeping, and resolver state — not just the
// control registration.
func TestFailedRegistrationRollsBackBinding(t *testing.T) {
	t.Run("immich_client_failure", func(t *testing.T) {
		d, sig, ds := relayOnlyTestDaemon(t, false)
		// Point the client at a disallowed host so newImmichClient fails.
		d.config.ImmichAllowedHost = "not-the-immich-host:2283"
		const code = "IMMICHROLLBACK1"
		if _, err := d.registerImmichShare(context.Background(), immich.SharedLink{Key: code, Type: "ALBUM"}, 5, time.Time{}, false); err == nil {
			t.Fatalf("expected Immich client creation to fail")
		}
		assertNoHalfBinding(t, d, ds, sig, code)
	})
	t.Run("session_save_failure", func(t *testing.T) {
		d, sig, ds := relayOnlyTestDaemon(t, true)
		st := d.store.(*mockStore)
		st.mu.Lock()
		st.saveError = errors.New("disk full")
		st.mu.Unlock()
		const code = "IMMICHROLLBACK2"
		if _, err := d.registerImmichShare(context.Background(), immich.SharedLink{Key: code, Type: "ALBUM"}, 5, time.Time{}, true); err == nil {
			t.Fatalf("expected SaveSession to fail")
		}
		assertNoHalfBinding(t, d, ds, sig, code)
	})
}

// TestRevokeRemovesPendingRelayBind is the concrete Important #1 leak: a
// relay-only share whose binder does not exist yet records a pending bind;
// revoking it must delete that pending bind, or a later binder build installs
// the revoked binding.
func TestRevokeRemovesPendingRelayBind(t *testing.T) {
	cm, _ := newBaselineCertFixture(t, t.TempDir())
	ds := &directState{
		baseDomain:   testDirectBase,
		cert:         cm,
		origins:      map[string]originPair{},
		relayOrigins: map[string]string{},
	}
	d := &Daemon{resolver: direct.NewResolverRegistry(), direct: ds}
	const code = "IMMICHPEND1"
	origin := testOriginFor("sblate9")
	if _, ok := d.bindRelayOnlyOrigin(code, origin); ok {
		t.Fatalf("no binder yet: bindRelayOnlyOrigin must record a pending bind")
	}
	ds.mu.Lock()
	pending := ds.pendingRelayBinds[code]
	ds.mu.Unlock()
	if pending != origin {
		t.Fatalf("pendingRelayBinds[%s] = %q, want %q", code, pending, origin)
	}

	d.revokeOrigin(code)
	ds.mu.Lock()
	_, still := ds.pendingRelayBinds[code]
	ds.mu.Unlock()
	if still {
		t.Fatalf("revokeOrigin must delete the pending relay-only bind")
	}

	// Build the binder now: the revoked binding must NOT be installed.
	ds.mu.Lock()
	ds.namespace = testDirectNS
	ds.mu.Unlock()
	if err := d.syncDirectServe(); err != nil {
		t.Fatalf("syncDirectServe: %v", err)
	}
	ds.mu.Lock()
	binder := ds.binder
	ds.mu.Unlock()
	if binder == nil {
		t.Fatalf("binder must exist after syncDirectServe")
	}
	if _, err := binder.AdmitSNI(relayOriginForLabel("sblate9")); err == nil {
		t.Fatalf("a revoked pending relay-only binding was installed by a later binder build")
	}
}

// TestRevokeAndExpiryRemoveResolverState is audit Important #2's cleanup half:
// normal revocation and expiry must delete the share's resolver entry.
func TestRevokeAndExpiryRemoveResolverState(t *testing.T) {
	d, _ := newTestDaemon(t)
	baseURL, host := newImmichTestServer(t)
	d.config.ImmichURL = baseURL
	d.config.ImmichAllowedHost = host
	d.config.ImmichAPIKey = "api"
	d.newImmichPoller = func() (immichPoller, error) {
		return &fakeImmichPoller{shares: []immich.SharedLink{
			{Key: "IMMICHRESOLVE1", Type: "ALBUM"},
			{Key: "IMMICHRESOLVE2", Type: "ALBUM"},
		}}, nil
	}
	if err := d.syncImmichShares(context.Background()); err != nil {
		t.Fatalf("syncImmichShares: %v", err)
	}
	if d.resolver.Get("IMMICHRESOLVE1") == nil || d.resolver.Get("IMMICHRESOLVE2") == nil {
		t.Fatalf("precondition: both shares must be hydrated")
	}

	if err := d.RevokeSession("IMMICHRESOLVE1"); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}
	if d.resolver.Get("IMMICHRESOLVE1") != nil {
		t.Errorf("RevokeSession must remove the resolver entry")
	}

	d.mu.RLock()
	session := d.sessions["IMMICHRESOLVE2"]
	d.mu.RUnlock()
	if session == nil {
		t.Fatalf("precondition: session IMMICHRESOLVE2 must exist")
	}
	session.ExpiresAt = time.Now().Add(-time.Minute)
	d.pruneExpiredSessions()
	if d.resolver.Get("IMMICHRESOLVE2") != nil {
		t.Errorf("expiry must remove the resolver entry")
	}
}

// gatedBind starts bind in a goroutine and blocks it inside the Binder
// mutation, returning the entered/release/done channels. The caller must close
// release.
func gatedBind(t *testing.T, ds *directState, bind func()) (entered, release, done chan struct{}) {
	t.Helper()
	entered = make(chan struct{})
	release = make(chan struct{})
	ds.mu.Lock()
	binder := ds.binder
	ds.mu.Unlock()
	if binder == nil {
		t.Fatalf("gatedBind requires a built binder")
	}
	binder.SetTestHookBeforeAllow(func() {
		close(entered)
		<-release
	})
	done = make(chan struct{})
	go func() {
		defer close(done)
		bind()
	}()
	awaitRecv(t, entered, "bind entering the Binder mutation")
	return entered, release, done
}

// TestBindOriginHoldsStateLockAcrossCommit pins the Important #1 serialization:
// while bindOrigin/bindRelayOnlyOrigin are inside the Binder mutation, ds.mu
// must be held, so a rebuild/revocation/lockdown cannot interleave between the
// Binder snapshot and the bookkeeping commit.
func TestBindOriginHoldsStateLockAcrossCommit(t *testing.T) {
	t.Run("direct_pair", func(t *testing.T) {
		d, _, ds := relayOnlyTestDaemon(t, false)
		const code = "IMMICHBIND1"
		origin := testOriginFor("sbbind1")
		_, release, done := gatedBind(t, ds, func() { d.bindOrigin(code, origin) })
		if ds.mu.TryLock() {
			ds.mu.Unlock()
			t.Errorf("bindOrigin released ds.mu before the Binder mutation/commit completed")
		}
		close(release)
		awaitClosed(t, done, "bindOrigin did not return")

		ds.mu.Lock()
		pair, ok := ds.origins[code]
		binder := ds.binder
		ds.mu.Unlock()
		if !ok {
			t.Fatalf("bindOrigin did not commit the origin pair")
		}
		if _, err := binder.AdmitSNI(pair.directOrigin); err != nil {
			t.Errorf("committed direct origin is not admitted: %v", err)
		}
		if _, err := binder.AdmitSNI(pair.relayOrigin); err != nil {
			t.Errorf("committed relay origin is not admitted: %v", err)
		}
	})
	t.Run("relay_only", func(t *testing.T) {
		d, _, ds := relayOnlyTestDaemon(t, false)
		const code = "IMMICHBIND2"
		origin := testOriginFor("sbbind2")
		_, release, done := gatedBind(t, ds, func() { d.bindRelayOnlyOrigin(code, origin) })
		if ds.mu.TryLock() {
			ds.mu.Unlock()
			t.Errorf("bindRelayOnlyOrigin released ds.mu before the Binder mutation/commit completed")
		}
		close(release)
		awaitClosed(t, done, "bindRelayOnlyOrigin did not return")

		ds.mu.Lock()
		relayOrigin, ok := ds.relayOrigins[code]
		binder := ds.binder
		ds.mu.Unlock()
		if !ok {
			t.Fatalf("bindRelayOnlyOrigin did not commit the relay binding")
		}
		if _, err := binder.AdmitSNI(relayOrigin); err != nil {
			t.Errorf("committed relay origin is not admitted: %v", err)
		}
		if _, err := binder.AdmitSNI(origin); err == nil {
			t.Errorf("relay-only bind must not admit the direct origin")
		}
	})
}

// TestRebuildDuringBindCommitsConsistently: a binder rebuild racing the bind
// commit must not leave a stale booking — every recorded origin must be
// admitted by the binder the server actually serves with.
func TestRebuildDuringBindCommitsConsistently(t *testing.T) {
	d, _, ds := relayOnlyTestDaemon(t, false)
	const code = "IMMICHBIND3"
	origin := testOriginFor("sbbind3")
	// Force syncDirectServe to rebuild rather than no-op.
	ds.mu.Lock()
	ds.serveNS = ""
	ds.mu.Unlock()

	_, release, done := gatedBind(t, ds, func() { d.bindOrigin(code, origin) })
	if ds.mu.TryLock() {
		ds.mu.Unlock()
		t.Errorf("bindOrigin must serialize with a binder rebuild across its commit")
		// Base behavior: land the rebuild inside the snapshot→commit window so
		// the booking is provably made against a replaced Binder.
		if err := d.syncDirectServe(); err != nil {
			t.Fatalf("rebuild inside the gap: %v", err)
		}
		close(release)
		awaitClosed(t, done, "bindOrigin did not return")
	} else {
		close(release)
		awaitClosed(t, done, "bindOrigin did not return")
		ds.mu.Lock()
		ds.serveNS = ""
		ds.mu.Unlock()
		if err := d.syncDirectServe(); err != nil {
			t.Fatalf("rebuild after commit: %v", err)
		}
	}

	ds.mu.Lock()
	pair, ok := ds.origins[code]
	binder := ds.binder
	ds.mu.Unlock()
	if !ok {
		t.Fatalf("bindOrigin did not commit the origin pair")
	}
	if _, err := binder.AdmitSNI(pair.directOrigin); err != nil {
		t.Errorf("stale booking: recorded direct origin is not admitted by the serving binder")
	}
	if _, err := binder.AdmitSNI(pair.relayOrigin); err != nil {
		t.Errorf("stale booking: recorded relay origin is not admitted by the serving binder")
	}
}

// TestRevokeDuringBindLeavesNoBinding: a revoke racing the bind commit must
// linearize, leaving no binding behind.
func TestRevokeDuringBindLeavesNoBinding(t *testing.T) {
	d, _, ds := relayOnlyTestDaemon(t, false)
	const code = "IMMICHREVOKE2"
	origin := testOriginFor("sbrevoke2")
	_, release, done := gatedBind(t, ds, func() { d.bindOrigin(code, origin) })
	revokeDone := make(chan struct{})
	if ds.mu.TryLock() {
		ds.mu.Unlock()
		t.Errorf("bindOrigin must serialize with a concurrent revokeOrigin")
		// Base behavior: the revoke lands before the bind commits and is lost.
		d.revokeOrigin(code)
		close(revokeDone)
	} else {
		go func() {
			defer close(revokeDone)
			d.revokeOrigin(code)
		}()
	}
	close(release)
	awaitClosed(t, done, "bindOrigin did not return")
	awaitClosed(t, revokeDone, "revokeOrigin did not return")

	ds.mu.Lock()
	_, hasPair := ds.origins[code]
	_, hasRelay := ds.relayOrigins[code]
	binder := ds.binder
	ds.mu.Unlock()
	if hasPair || hasRelay {
		t.Errorf("a revoke that raced the bind commit left a binding behind")
	}
	if binder != nil {
		if _, err := binder.AdmitSNI(origin); err == nil {
			t.Errorf("a revoked origin is still admitted")
		}
	}
}

// TestLockdownDuringBindWithdrawsAdmission: a lockdown racing the bind commit
// must not leave the daemon locked with an admission installed.
func TestLockdownDuringBindWithdrawsAdmission(t *testing.T) {
	d, _, ds := relayOnlyTestDaemon(t, false)
	const code = "IMMICHLOCK1"
	origin := testOriginFor("sblock1")
	_, release, done := gatedBind(t, ds, func() { d.bindOrigin(code, origin) })
	lockdownDone := make(chan struct{})
	if ds.mu.TryLock() {
		ds.mu.Unlock()
		t.Errorf("bindOrigin must serialize with a lockdown transition")
		_ = d.Lockdown()
		close(lockdownDone)
	} else {
		go func() {
			defer close(lockdownDone)
			_ = d.Lockdown()
		}()
	}
	close(release)
	awaitClosed(t, done, "bindOrigin did not return")
	awaitClosed(t, lockdownDone, "Lockdown did not return")

	ds.mu.Lock()
	locked := ds.locked
	binder := ds.binder
	ds.mu.Unlock()
	if !locked {
		t.Fatalf("precondition: the daemon must be locked after Lockdown")
	}
	if binder != nil {
		if _, err := binder.AdmitSNI(origin); err == nil {
			t.Errorf("a locked daemon kept an admission installed after a lockdown raced the bind")
		}
	}
}

// TestBinderLifecycleInterleavings hammers binder mutation, revocation,
// rebuilds, and lockdowns concurrently. It is a -race/repetition control; the
// deterministic interleavings live in the tests above.
func TestBinderLifecycleInterleavings(t *testing.T) {
	d, _, ds := relayOnlyTestDaemon(t, false)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		code := fmt.Sprintf("IMMICHINT%d", i)
		origin := testOriginFor(fmt.Sprintf("sbint%d", i))
		wg.Add(1)
		go func(code, origin string) {
			defer wg.Done()
			for n := 0; n < 40; n++ {
				d.bindOrigin(code, origin)
				d.revokeOrigin(code)
			}
		}(code, origin)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := 0; n < 25; n++ {
			ds.mu.Lock()
			ds.serveNS = ""
			ds.mu.Unlock()
			_ = d.syncDirectServe()
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := 0; n < 8; n++ {
			_ = d.Lockdown()
		}
	}()
	wg.Wait()

	// Quiesce to a known unlocked, rebuilt state and assert every recorded
	// binding is admitted by the binder actually in service.
	ds.mu.Lock()
	ds.locked = false
	ds.serveNS = ""
	ds.mu.Unlock()
	if err := d.syncDirectServe(); err != nil {
		t.Fatalf("final rebuild: %v", err)
	}
	ds.mu.Lock()
	binder := ds.binder
	pairs := make([]originPair, 0, len(ds.origins))
	for _, p := range ds.origins {
		pairs = append(pairs, p)
	}
	relays := make([]string, 0, len(ds.relayOrigins))
	for _, r := range ds.relayOrigins {
		relays = append(relays, r)
	}
	ds.mu.Unlock()
	for _, p := range pairs {
		if _, err := binder.AdmitSNI(p.directOrigin); err != nil {
			t.Errorf("recorded direct origin %s is not admitted by the serving binder", p.directOrigin)
		}
		if _, err := binder.AdmitSNI(p.relayOrigin); err != nil {
			t.Errorf("recorded relay origin %s is not admitted by the serving binder", p.relayOrigin)
		}
	}
	for _, r := range relays {
		if _, err := binder.AdmitSNI(r); err != nil {
			t.Errorf("recorded relay-only origin %s is not admitted by the serving binder", r)
		}
	}
}
