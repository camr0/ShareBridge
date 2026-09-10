// agent/internal/daemon/daemon_reregistration_test.go
package daemon

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"sharebridge/agent/internal/direct"
	"sharebridge/agent/internal/store"
)

// This file pins the M4 remediation round B2b fix at the daemon level: every
// successful signaling connect re-runs loadSessionsFromStore, which re-binds
// every persisted share. That re-registration must be IDEMPOTENT for an
// unchanged share — it must not advance the Binder generation, because
// advancing it makes every in-flight request admitted from the previous epoch
// fail Revalidate, and the handler then closes the (possibly shared,
// HTTP/2 multiplexed) connection, killing live downloads on a routine WS flap
// or network blip. Generation advances stay reserved for an admission that
// actually changes, and for revoke / re-add after revoke.

const (
	reregStoreCode  = "IMMICHREREG1"
	reregStoreLabel = "immichrereg1"
)

// reregDaemonConn serves the daemon's DirectServer on a real TLS listener and
// downloads through it with the appropriate SNI/Host.
type reregDaemonConn struct {
	addr string
}

func startReregDaemonConn(t *testing.T, ds *directState) *reregDaemonConn {
	t.Helper()
	ts := httptest.NewUnstartedServer(ds.server.Handler())
	ts.TLS = ds.server.TLSConfig()
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return &reregDaemonConn{addr: ts.Listener.Addr().String()}
}

func (c *reregDaemonConn) download(origin, code string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, "https://"+origin+"/s/"+code+"/download?size=4096", nil)
	if err != nil {
		return nil, err
	}
	req.Host = origin
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{ServerName: origin, InsecureSkipVerify: true, NextProtos: []string{"http/1.1"}}, // test-only: self-signed
		ForceAttemptHTTP2: false,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, c.addr)
		},
	}
	return (&http.Client{Transport: tr}).Do(req)
}

// persistReregSession writes the ONE session row loadSessionsFromStore restores:
// the same code, the same deterministic origin pair, and the same route kind.
func persistReregSession(t *testing.T, d *Daemon, code, label string, relayOnly bool) {
	t.Helper()
	if err := d.store.SaveSession(store.SessionEntry{
		Code:      code,
		ShareURL:  "immich://" + code,
		ShareType: "immich",
		RelayOnly: relayOnly,
		Origin:    testOriginFor(label),
	}); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
}

// reregAdmittedOrigin returns the origin a request for the share uses for its
// route kind.
func reregAdmittedOrigin(label string, relayOnly bool) string {
	if relayOnly {
		return relayOriginForLabel(label)
	}
	return testOriginFor(label)
}

// TestReconnectReregistrationPreservesEpochAndInFlightRequest is the reviewed
// regression end to end: the persisted share is restored (first connect), a
// request is admitted and parked in the authorization→registration window, the
// routine reconnect re-runs loadSessionsFromStore for the UNCHANGED share, and
// the parked request must still complete. Both route kinds.
func TestReconnectReregistrationPreservesEpochAndInFlightRequest(t *testing.T) {
	for _, tc := range []struct {
		name      string
		relayOnly bool
	}{
		{"direct_pair", false},
		{"relay_only", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _, ds := newGenerationDaemon(t)
			persistReregSession(t, d, reregStoreCode, reregStoreLabel, tc.relayOnly)

			// First connect: restore the persisted share and install its binding.
			d.loadSessionsFromStore(context.Background())
			origin := reregAdmittedOrigin(reregStoreLabel, tc.relayOnly)
			before, err := ds.binder.AdmitSNI(origin)
			if err != nil {
				t.Fatalf("precondition: the restored share must be admitted: %v", err)
			}

			// Deterministic synthetic content for the in-flight stream.
			ds.server.SetResolver(stubResolver{})

			entered := make(chan struct{})
			release := make(chan struct{})
			ds.server.SetTestHookAfterAuthorize(func() {
				close(entered)
				<-release
			})
			conn := startReregDaemonConn(t, ds)

			type result struct {
				resp *http.Response
				err  error
			}
			done := make(chan result, 1)
			go func() {
				resp, err := conn.download(origin, reregStoreCode)
				done <- result{resp, err}
			}()
			awaitRecv(t, entered, "handler entering the post-authorize window")

			// The routine reconnect: the SAME persisted share is re-registered.
			d.loadSessionsFromStore(context.Background())

			after, err := ds.binder.AdmitSNI(origin)
			if err != nil {
				t.Errorf("the share must still be admitted after the reconnect: %v", err)
			} else if after != before {
				t.Errorf("the reconnect re-registration advanced the generation for an unchanged share (%+v -> %+v); in-flight requests admitted before it fail Revalidate and the connection is closed", before, after)
			}
			close(release)
			res := <-done
			if res.err != nil {
				t.Errorf("in-flight request must complete across the reconnect: %v", res.err)
				return
			}
			defer res.resp.Body.Close()
			if res.resp.StatusCode != http.StatusOK {
				t.Errorf("in-flight request status = %d, want 200", res.resp.StatusCode)
				return
			}
			if n, _ := io.Copy(io.Discard, res.resp.Body); n != 4096 {
				t.Errorf("in-flight request served %d bytes, want 4096", n)
			}
		})
	}
}

// TestChangedAdmissionFailsClosedAtDaemon is the C3 control at the daemon
// level: re-binding the SAME origin to a DIFFERENT share code is a genuine
// admission change — it advances the generation and the parked request served
// from the superseded binding is refused. Both route kinds.
func TestChangedAdmissionFailsClosedAtDaemon(t *testing.T) {
	for _, tc := range []struct {
		name      string
		relayOnly bool
	}{
		{"direct_pair_changed", false},
		{"relay_only_changed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _, ds := newGenerationDaemon(t)
			const (
				label     = "immichchanged1"
				oldCode   = "IMMICHCHANGED1"
				otherCode = "IMMICHCHANGED2"
			)
			origin := testOriginFor(label)
			if tc.relayOnly {
				d.bindRelayOnlyOrigin(oldCode, origin)
			} else {
				d.bindOrigin(oldCode, origin)
			}
			admitted := reregAdmittedOrigin(label, tc.relayOnly)
			before, err := ds.binder.AdmitSNI(admitted)
			if err != nil {
				t.Fatalf("precondition: %v", err)
			}

			ds.server.SetResolver(stubResolver{})
			entered := make(chan struct{})
			release := make(chan struct{})
			ds.server.SetTestHookAfterAuthorize(func() {
				close(entered)
				<-release
			})
			conn := startReregDaemonConn(t, ds)
			type result struct {
				resp *http.Response
				err  error
			}
			done := make(chan result, 1)
			go func() {
				resp, err := conn.download(admitted, oldCode)
				done <- result{resp, err}
			}()
			awaitRecv(t, entered, "handler entering the post-authorize window")

			// The same origin now authorizes a different content session.
			if tc.relayOnly {
				d.bindRelayOnlyOrigin(otherCode, origin)
			} else {
				d.bindOrigin(otherCode, origin)
			}
			after, err := ds.binder.AdmitSNI(admitted)
			if err != nil {
				t.Errorf("the origin must stay admitted for its new share: %v", err)
			} else if after == before {
				t.Errorf("a changed admission must advance the generation")
			}
			if ds.binder.Revalidate(before) {
				t.Errorf("a changed admission must not revalidate the superseded binding")
			}
			close(release)
			res := <-done
			if res.err == nil {
				defer res.resp.Body.Close()
				if res.resp.StatusCode == http.StatusOK {
					t.Errorf("a superseded admission served a stream (status 200)")
				}
			}
		})
	}
}

// TestRevokeStillAdvancesGenerationAndFailsClosedAtDaemon is the C3 control for
// revocation: the epoch advances, the entry is gone, and the parked request is
// refused. Both route kinds.
func TestRevokeStillAdvancesGenerationAndFailsClosedAtDaemon(t *testing.T) {
	for _, tc := range []struct {
		name      string
		relayOnly bool
	}{
		{"direct_pair_revoked", false},
		{"relay_only_revoked", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _, ds := newGenerationDaemon(t)
			const (
				label = "immichrevoked1"
				code  = "IMMICHREVOKED1"
			)
			origin := testOriginFor(label)
			if tc.relayOnly {
				d.bindRelayOnlyOrigin(code, origin)
			} else {
				d.bindOrigin(code, origin)
			}
			admitted := reregAdmittedOrigin(label, tc.relayOnly)
			before, err := ds.binder.AdmitSNI(admitted)
			if err != nil {
				t.Fatalf("precondition: %v", err)
			}

			ds.server.SetResolver(stubResolver{})
			entered := make(chan struct{})
			release := make(chan struct{})
			ds.server.SetTestHookAfterAuthorize(func() {
				close(entered)
				<-release
			})
			conn := startReregDaemonConn(t, ds)
			type result struct {
				resp *http.Response
				err  error
			}
			done := make(chan result, 1)
			go func() {
				resp, err := conn.download(admitted, code)
				done <- result{resp, err}
			}()
			awaitRecv(t, entered, "handler entering the post-authorize window")

			d.revokeOrigin(code)
			if _, err := ds.binder.AdmitSNI(admitted); err == nil {
				t.Errorf("a revoked origin must not stay admitted")
			}
			if ds.binder.Revalidate(before) {
				t.Errorf("revocation must invalidate the admitted binding")
			}
			close(release)
			res := <-done
			if res.err == nil {
				defer res.resp.Body.Close()
				if res.resp.StatusCode == http.StatusOK {
					t.Errorf("a revoked share served a stream after revocation")
				}
			}
		})
	}
}

// TestRepeatedReconnectReregistrationKeepsEpochAndServing runs the reconnect
// path repeatedly: the unchanged persisted share must keep serving and the
// generation must not move. Both route kinds.
func TestRepeatedReconnectReregistrationKeepsEpochAndServing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		relayOnly bool
	}{
		{"direct_pair", false},
		{"relay_only", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _, ds := newGenerationDaemon(t)
			persistReregSession(t, d, reregStoreCode, reregStoreLabel, tc.relayOnly)
			d.loadSessionsFromStore(context.Background())
			origin := reregAdmittedOrigin(reregStoreLabel, tc.relayOnly)
			before, err := ds.binder.AdmitSNI(origin)
			if err != nil {
				t.Fatalf("precondition: %v", err)
			}
			ds.server.SetResolver(stubResolver{})
			conn := startReregDaemonConn(t, ds)

			for i := 0; i < 6; i++ {
				d.loadSessionsFromStore(context.Background())
				resp, err := conn.download(origin, reregStoreCode)
				if err != nil {
					t.Fatalf("download %d across a reconnect: %v", i, err)
				}
				n, _ := io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK || n != 4096 {
					t.Fatalf("download %d: status %d, %d bytes, want 200/4096", i, resp.StatusCode, n)
				}
				after, err := ds.binder.AdmitSNI(origin)
				if err != nil {
					t.Fatalf("iteration %d: the share must stay admitted: %v", i, err)
				}
				if after != before {
					t.Fatalf("iteration %d: an identical reconnect re-registration advanced the generation (%+v -> %+v)", i, before, after)
				}
			}
		})
	}
}

// TestRelayOnlyBindDoesNotDisturbDirectAdmissionOfOtherCodes is a scoping guard:
// the idempotency/supersede logic is per share code, so re-binding one code
// must leave every other code's admission untouched.
func TestRelayOnlyBindDoesNotDisturbDirectAdmissionOfOtherCodes(t *testing.T) {
	d, _, ds := newGenerationDaemon(t)
	const codeA = "IMMICHGUARDA"
	const codeB = "IMMICHGUARDB"
	if _, ok := d.bindOrigin(codeA, testOriginFor("immichguard1")); !ok {
		t.Fatalf("bindOrigin A must succeed")
	}
	if _, ok := d.bindRelayOnlyOrigin(codeB, testOriginFor("immichguard2")); !ok {
		t.Fatalf("bindRelayOnlyOrigin B must succeed")
	}
	aBefore, err := ds.binder.AdmitSNI(testOriginFor("immichguard1"))
	if err != nil {
		t.Fatalf("precondition A: %v", err)
	}
	// Re-register B identically; A's binding must be bit-identical afterwards.
	d.bindRelayOnlyOrigin(codeB, testOriginFor("immichguard2"))
	aAfter, err := ds.binder.AdmitSNI(testOriginFor("immichguard1"))
	if err != nil {
		t.Fatalf("A must stay admitted: %v", err)
	}
	if aAfter != aBefore {
		t.Errorf("re-binding an unrelated code disturbed A's admission: %+v -> %+v", aBefore, aAfter)
	}
	if entry, err := ds.binder.AdmitSNI(testOriginFor("immichguard1")); err != nil || entry.RouteKind != direct.RouteDirect {
		t.Errorf("A's admission must still be the direct route kind: %+v, %v", entry, err)
	}
}
