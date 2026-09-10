// agent/internal/direct/binder_reregistration_test.go
package direct

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
)

// This file pins the M4 remediation round B2b fix: re-registering an UNCHANGED
// admission (the routine WS-reconnect path that re-runs every persisted share
// through Allow/AllowShare) must be idempotent and must preserve the Binder
// generation. Advancing the epoch on an identical admission invalidates every
// in-flight request admitted from that entry (Revalidate compares the entry,
// epoch included) and closeState tears the connection down — killing live
// streams on a routine network blip. Generation advances are reserved for an
// admission that actually CHANGES (different code/origin/route kind) or a
// re-add after revoke/withdrawal.
//
// The controls in this file prove the fix does not weaken Critical #3: a
// changed admission, and a re-add after revocation, still advance the
// generation and still fail the stale in-flight request closed.

const (
	reregNS       = "sbdeadbeef"
	reregBase     = "example.com"
	reregCode     = "abc"
	reregOther    = "def"
	reregDirect   = "demo." + reregNS + "." + reregBase
	reregRelay    = "demo.relay." + reregNS + "." + reregBase
	reregDownload = 4096
)

// startRelayOnlyGenerationTestServer builds the generation fixture for the
// §13.3 relay-only route kind: only the relay origin is admitted, via Allow —
// exactly what bindRelayOnlyOrigin installs. The direct origin stays unbound.
func startRelayOnlyGenerationTestServer(t *testing.T) *generationTestServer {
	t.Helper()
	cert := testServerCert(t, reregNS, reregBase)
	gate := NewSignalGate("a", func(string, RouteKind) bool { return true })
	srv := NewDirectServer(reregNS, reregBase, nil, &rotatableCerts{cert}, gate, 1<<20)
	if err := srv.Binder().Allow(reregRelay, RouteRelay, reregCode); err != nil {
		t.Fatalf("Allow(relay): %v", err)
	}
	if _, err := srv.Binder().AdmitSNI(reregDirect); err == nil {
		t.Fatalf("relay-only fixture must not admit the direct origin")
	}
	res := &countingContentResolver{session: &ContentSession{Membership: map[string]struct{}{}}}
	srv.SetResolver(res)
	hs := srv.newHTTPServer()
	hs.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go hs.ServeTLS(ln, "", "")
	t.Cleanup(func() { hs.Close(); ln.Close() })
	return &generationTestServer{srv: srv, res: res, ln: ln, hs: hs, direct: reregDirect, relay: reregRelay}
}

// gatedResult is the outcome of one gated download.
type gatedResult struct {
	resp *http.Response
	err  error
}

// startGatedDownload installs the post-authorize seam, starts one download and
// returns once the request is parked in the authorization→binding window (so
// its admitted binding is already captured at the current generation).
func startGatedDownload(t *testing.T, g *generationTestServer, origin, code string) (release chan struct{}, done chan gatedResult) {
	t.Helper()
	entered := make(chan struct{})
	release = make(chan struct{})
	g.srv.SetTestHookAfterAuthorize(func() {
		close(entered)
		<-release
	})
	done = make(chan gatedResult, 1)
	go func() {
		resp, err := g.download(g.client(origin), origin, code)
		done <- gatedResult{resp, err}
	}()
	awaitSignal(t, entered, "handler entering the post-authorize window")
	return release, done
}

// assertInFlightServed asserts the parked request completed with the full
// stream on the same connection (i.e. it was not torn down).
func assertInFlightServed(t *testing.T, res gatedResult, res2 *countingContentResolver) {
	t.Helper()
	if res.err != nil {
		t.Errorf("in-flight request must complete across an identical re-registration: %v", res.err)
		return
	}
	defer res.resp.Body.Close()
	if res.resp.StatusCode != http.StatusOK {
		t.Errorf("in-flight request status = %d, want 200 (an identical re-registration must not tear the connection down)", res.resp.StatusCode)
		return
	}
	if n, _ := io.Copy(io.Discard, res.resp.Body); n != reregDownload {
		t.Errorf("in-flight request served %d bytes, want %d", n, reregDownload)
	}
	if n := res2.Calls(); n != 1 {
		t.Errorf("resolver calls = %d, want 1", n)
	}
}

// assertRefused asserts nothing was served and content was never resolved.
func assertRefused(t *testing.T, res gatedResult, res2 *countingContentResolver) {
	t.Helper()
	if res.err == nil {
		defer res.resp.Body.Close()
		if res.resp.StatusCode == http.StatusOK {
			t.Errorf("a superseded admission served a stream (status 200)")
		}
		if body, _ := io.ReadAll(res.resp.Body); len(body) == reregDownload {
			t.Errorf("a superseded admission served the full %d-byte stream", reregDownload)
		}
	}
	if n := res2.Calls(); n != 0 {
		t.Errorf("content was resolved %d time(s) for a superseded admission; it must serve nothing", n)
	}
}

// TestBinderIdenticalReregistrationPreservesGeneration is the Binder-level
// regression: re-Allow/AllowShare of the identical active admission must be a
// genuine no-op that preserves the generation, while a changed admission and a
// re-add after revocation still advance it.
func TestBinderIdenticalReregistrationPreservesGeneration(t *testing.T) {
	t.Run("allow_relay_only", func(t *testing.T) {
		b := NewBinder(reregNS, reregBase)
		if err := b.Allow(reregRelay, RouteRelay, reregCode); err != nil {
			t.Fatalf("Allow: %v", err)
		}
		bd, err := b.AdmitSNI(reregRelay)
		if err != nil {
			t.Fatalf("AdmitSNI: %v", err)
		}
		if err := b.Allow(reregRelay, RouteRelay, reregCode); err != nil {
			t.Fatalf("identical re-Allow: %v", err)
		}
		again, err := b.AdmitSNI(reregRelay)
		if err != nil {
			t.Fatalf("AdmitSNI after identical re-Allow: %v", err)
		}
		if again != bd {
			t.Errorf("an identical re-Allow advanced the generation: %+v -> %+v", bd, again)
		}
		if !b.Revalidate(bd) {
			t.Errorf("a binding admitted before an identical re-Allow must still revalidate")
		}

		if err := b.Allow(reregRelay, RouteRelay, reregOther); err != nil {
			t.Fatalf("changed Allow: %v", err)
		}
		changed, err := b.AdmitSNI(reregRelay)
		if err != nil {
			t.Fatalf("AdmitSNI after changed Allow: %v", err)
		}
		if changed == bd {
			t.Errorf("a changed admission (different share code) must advance the generation")
		}
		if b.Revalidate(bd) {
			t.Errorf("a changed admission must not revalidate the superseded binding")
		}
		if !b.Revalidate(changed) {
			t.Errorf("the changed admission must revalidate")
		}

		b.Revoke(reregRelay)
		if err := b.Allow(reregRelay, RouteRelay, reregCode); err != nil {
			t.Fatalf("re-Allow after revoke: %v", err)
		}
		readded, err := b.AdmitSNI(reregRelay)
		if err != nil {
			t.Fatalf("AdmitSNI after re-add: %v", err)
		}
		if readded == bd {
			t.Errorf("a re-add after revocation must be a new generation, not the pre-revoke one")
		}
		if b.Revalidate(bd) {
			t.Errorf("a re-add after revocation must not resurrect the revoked admission")
		}
	})

	t.Run("allow_share_pair", func(t *testing.T) {
		b := NewBinder(reregNS, reregBase)
		if err := b.AllowShare(reregDirect, reregRelay, reregCode); err != nil {
			t.Fatalf("AllowShare: %v", err)
		}
		dbd, err := b.AdmitSNI(reregDirect)
		if err != nil {
			t.Fatalf("AdmitSNI(direct): %v", err)
		}
		rbd, err := b.AdmitSNI(reregRelay)
		if err != nil {
			t.Fatalf("AdmitSNI(relay): %v", err)
		}
		if err := b.AllowShare(reregDirect, reregRelay, reregCode); err != nil {
			t.Fatalf("identical re-AllowShare: %v", err)
		}
		d2, _ := b.AdmitSNI(reregDirect)
		r2, _ := b.AdmitSNI(reregRelay)
		if d2 != dbd || r2 != rbd {
			t.Errorf("an identical re-AllowShare advanced the pair generation: %+v/%+v -> %+v/%+v", dbd, rbd, d2, r2)
		}
		if !b.Revalidate(dbd) || !b.Revalidate(rbd) {
			t.Errorf("both pair bindings admitted before an identical re-AllowShare must still revalidate")
		}

		if err := b.AllowShare(reregDirect, reregRelay, reregOther); err != nil {
			t.Fatalf("changed AllowShare: %v", err)
		}
		d3, _ := b.AdmitSNI(reregDirect)
		if d3 == dbd {
			t.Errorf("a changed pair admission must advance the generation")
		}
		if b.Revalidate(dbd) || b.Revalidate(rbd) {
			t.Errorf("a changed pair admission must not revalidate the superseded bindings")
		}

		b.RevokeShare(reregDirect, reregRelay)
		if err := b.AllowShare(reregDirect, reregRelay, reregCode); err != nil {
			t.Fatalf("re-AllowShare after revoke: %v", err)
		}
		d4, _ := b.AdmitSNI(reregDirect)
		if d4 == dbd {
			t.Errorf("a pair re-add after revocation must be a new generation")
		}
		if b.Revalidate(dbd) || b.Revalidate(rbd) {
			t.Errorf("a pair re-add after revocation must not resurrect the revoked admissions")
		}
	})
}

// TestBinderIdenticalReregistrationStableUnderRepetition hammers the identical
// re-registration path: the generation (and therefore the live connections)
// must stay stable no matter how many routine reconnects re-register the same
// admission.
func TestBinderIdenticalReregistrationStableUnderRepetition(t *testing.T) {
	b := NewBinder(reregNS, reregBase)
	if err := b.AllowShare(reregDirect, reregRelay, reregCode); err != nil {
		t.Fatalf("AllowShare: %v", err)
	}
	dbd, err := b.AdmitSNI(reregDirect)
	if err != nil {
		t.Fatalf("AdmitSNI(direct): %v", err)
	}
	rbd, err := b.AdmitSNI(reregRelay)
	if err != nil {
		t.Fatalf("AdmitSNI(relay): %v", err)
	}
	for i := 0; i < 200; i++ {
		if err := b.AllowShare(reregDirect, reregRelay, reregCode); err != nil {
			t.Fatalf("re-AllowShare %d: %v", i, err)
		}
		// The relay-only path re-admits its single §13.3 entry.
		if err := b.Allow(reregRelay, RouteRelay, reregCode); err != nil {
			t.Fatalf("re-Allow %d: %v", i, err)
		}
		if !b.Revalidate(dbd) || !b.Revalidate(rbd) {
			t.Fatalf("iteration %d: an identical re-registration invalidated a live admission", i)
		}
	}
	d2, _ := b.AdmitSNI(reregDirect)
	r2, _ := b.AdmitSNI(reregRelay)
	if d2 != dbd || r2 != rbd {
		t.Errorf("200 identical re-registrations changed the generation: %+v/%+v -> %+v/%+v", dbd, rbd, d2, r2)
	}
}

// TestReregistrationPreservesEpochAndInFlightConnection is the regression
// exactly: a request admitted at generation E is parked in the
// authorization→registration window; the routine reconnect re-registration of
// the unchanged share runs; the request must still complete on its connection.
func TestReregistrationPreservesEpochAndInFlightConnection(t *testing.T) {
	t.Run("direct_pair", func(t *testing.T) {
		g := startGenerationTestServer(t)
		before, err := g.srv.Binder().AdmitSNI(g.direct)
		if err != nil {
			t.Fatalf("precondition: %v", err)
		}
		release, done := startGatedDownload(t, g, g.direct, reregCode)

		if err := g.srv.Binder().AllowShare(g.direct, g.relay, reregCode); err != nil {
			t.Fatalf("routine re-registration: %v", err)
		}
		after, err := g.srv.Binder().AdmitSNI(g.direct)
		if err != nil {
			t.Fatalf("the re-registered share must still be admitted: %v", err)
		}
		if after != before {
			t.Errorf("an identical re-registration advanced the generation (%+v -> %+v); in-flight requests admitted before it fail Revalidate and the connection is closed", before, after)
		}
		close(release)
		assertInFlightServed(t, <-done, g.res)
	})

	t.Run("relay_only", func(t *testing.T) {
		g := startRelayOnlyGenerationTestServer(t)
		before, err := g.srv.Binder().AdmitSNI(g.relay)
		if err != nil {
			t.Fatalf("precondition: %v", err)
		}
		release, done := startGatedDownload(t, g, g.relay, reregCode)

		// bindRelayOnlyOrigin's single-entry path.
		if err := g.srv.Binder().Allow(g.relay, RouteRelay, reregCode); err != nil {
			t.Fatalf("routine re-registration: %v", err)
		}
		after, err := g.srv.Binder().AdmitSNI(g.relay)
		if err != nil {
			t.Fatalf("the re-registered relay-only share must still be admitted: %v", err)
		}
		if after != before {
			t.Errorf("an identical relay-only re-registration advanced the generation (%+v -> %+v); in-flight requests admitted before it fail Revalidate and the connection is closed", before, after)
		}
		close(release)
		assertInFlightServed(t, <-done, g.res)
	})
}

// TestChangedAdmissionFailsClosedWhileRequestIsInFlight is the C3 control: a
// GENUINE change (a different share code for the admitted origin) and a re-add
// after revocation must still advance the generation and still fail the stale
// in-flight request closed. Both route kinds.
func TestChangedAdmissionFailsClosedWhileRequestIsInFlight(t *testing.T) {
	t.Run("direct_pair_changed", func(t *testing.T) {
		g := startGenerationTestServer(t)
		before, _ := g.srv.Binder().AdmitSNI(g.direct)
		release, done := startGatedDownload(t, g, g.direct, reregCode)

		if err := g.srv.Binder().AllowShare(g.direct, g.relay, reregOther); err != nil {
			t.Fatalf("changed AllowShare: %v", err)
		}
		after, _ := g.srv.Binder().AdmitSNI(g.direct)
		if after == before {
			t.Errorf("a changed admission must advance the generation")
		}
		if g.srv.Binder().Revalidate(before) {
			t.Errorf("a changed admission must not revalidate the superseded binding")
		}
		close(release)
		assertRefused(t, <-done, g.res)
	})

	t.Run("relay_only_changed", func(t *testing.T) {
		g := startRelayOnlyGenerationTestServer(t)
		before, _ := g.srv.Binder().AdmitSNI(g.relay)
		release, done := startGatedDownload(t, g, g.relay, reregCode)

		if err := g.srv.Binder().Allow(g.relay, RouteRelay, reregOther); err != nil {
			t.Fatalf("changed Allow: %v", err)
		}
		after, _ := g.srv.Binder().AdmitSNI(g.relay)
		if after == before {
			t.Errorf("a changed relay-only admission must advance the generation")
		}
		if g.srv.Binder().Revalidate(before) {
			t.Errorf("a changed relay-only admission must not revalidate the superseded binding")
		}
		close(release)
		assertRefused(t, <-done, g.res)
	})

	t.Run("direct_pair_after_revoke", func(t *testing.T) {
		g := startGenerationTestServer(t)
		before, _ := g.srv.Binder().AdmitSNI(g.direct)
		release, done := startGatedDownload(t, g, g.direct, reregCode)

		g.srv.Binder().RevokeShare(g.direct, g.relay)
		if g.srv.Binder().Revalidate(before) {
			t.Errorf("revocation must invalidate the admitted binding")
		}
		if err := g.srv.Binder().AllowShare(g.direct, g.relay, reregCode); err != nil {
			t.Fatalf("re-AllowShare after revoke: %v", err)
		}
		if g.srv.Binder().Revalidate(before) {
			t.Errorf("a re-add after revocation must not resurrect the revoked admission")
		}
		close(release)
		assertRefused(t, <-done, g.res)
	})

	t.Run("relay_only_after_revoke", func(t *testing.T) {
		g := startRelayOnlyGenerationTestServer(t)
		before, _ := g.srv.Binder().AdmitSNI(g.relay)
		release, done := startGatedDownload(t, g, g.relay, reregCode)

		g.srv.Binder().Revoke(g.relay)
		if g.srv.Binder().Revalidate(before) {
			t.Errorf("revocation must invalidate the admitted relay-only binding")
		}
		if err := g.srv.Binder().Allow(g.relay, RouteRelay, reregCode); err != nil {
			t.Fatalf("re-Allow after revoke: %v", err)
		}
		if g.srv.Binder().Revalidate(before) {
			t.Errorf("a re-add after revocation must not resurrect the revoked relay-only admission")
		}
		close(release)
		assertRefused(t, <-done, g.res)
	})
}

// TestRepeatedIdenticalReregistrationKeepsLiveConnection runs the routine
// reconnect re-registration repeatedly against a keep-alive client: every
// download must still be served and the generation must not move.
func TestRepeatedIdenticalReregistrationKeepsLiveConnection(t *testing.T) {
	t.Run("direct_pair", func(t *testing.T) {
		g := startGenerationTestServer(t)
		before, _ := g.srv.Binder().AdmitSNI(g.direct)
		client := g.client(g.direct)
		for i := 0; i < 20; i++ {
			if err := g.srv.Binder().AllowShare(g.direct, g.relay, reregCode); err != nil {
				t.Fatalf("re-AllowShare %d: %v", i, err)
			}
			resp, err := g.download(client, g.direct, reregCode)
			if err != nil {
				t.Fatalf("download %d after an identical re-registration: %v", i, err)
			}
			n, _ := io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || n != reregDownload {
				t.Fatalf("download %d: status %d, %d bytes, want 200/%d", i, resp.StatusCode, n, reregDownload)
			}
		}
		after, _ := g.srv.Binder().AdmitSNI(g.direct)
		if after != before {
			t.Errorf("20 routine reconnects advanced the generation: %+v -> %+v", before, after)
		}
	})

	t.Run("relay_only", func(t *testing.T) {
		g := startRelayOnlyGenerationTestServer(t)
		before, _ := g.srv.Binder().AdmitSNI(g.relay)
		client := g.client(g.relay)
		for i := 0; i < 20; i++ {
			if err := g.srv.Binder().Allow(g.relay, RouteRelay, reregCode); err != nil {
				t.Fatalf("re-Allow %d: %v", i, err)
			}
			resp, err := g.download(client, g.relay, reregCode)
			if err != nil {
				t.Fatalf("download %d after an identical re-registration: %v", i, err)
			}
			n, _ := io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || n != reregDownload {
				t.Fatalf("download %d: status %d, %d bytes, want 200/%d", i, resp.StatusCode, n, reregDownload)
			}
		}
		after, _ := g.srv.Binder().AdmitSNI(g.relay)
		if after != before {
			t.Errorf("20 routine reconnects advanced the generation: %+v -> %+v", before, after)
		}
	})
}

// TestSetTestHookAfterAuthorizeIsOneShot pins the documented contract of the
// seam itself: it is one-shot, so it runs for exactly one authorized request.
func TestSetTestHookAfterAuthorizeIsOneShot(t *testing.T) {
	g := startGenerationTestServer(t)
	var calls atomic.Int64
	g.srv.SetTestHookAfterAuthorize(func() { calls.Add(1) })
	for i := 0; i < 3; i++ {
		resp, err := g.download(g.client(g.direct), g.direct, reregCode)
		if err != nil {
			t.Fatalf("download %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the post-authorize test seam ran %d times, want exactly 1 (it is documented one-shot)", n)
	}
}
