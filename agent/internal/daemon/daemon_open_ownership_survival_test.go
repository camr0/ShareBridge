package daemon

// M5 remediation round 1 (post-M5 Sol audit, availability finding): a later
// open's report / OK-ack transport failure must not tear down a mapping that a
// DIFFERENT recipient already established.
//
// The bug: every open in one unlocked §13.4 epoch shares the same generation
// stamp (`openEpoch`), so discarding "by generation" after a failed report or
// undelivered OK ack also ripped down an already-healthy mapping, its logical
// open state, and its sessions/holds. These tests drive the real
// handleOpenSignal path over a real OnDemandPort + Reporter with only the
// signaling transport substituted, so the discriminator under test (who
// CREATED the mapping) is the port's own serialized decision, not a shared
// epoch flag.
//
// The four contract cases:
//   - a later open that joins an existing healthy mapping and fails its report
//     (TestLaterOpenReportFailureKeepsHealthyMapping) or its OK-ack send
//     (TestLaterOpenOKAckFailureKeepsHealthyMapping, and the renewal write
//     variant TestLaterRenewalReportFailureKeepsHealthyMapping) leaves that
//     mapping, its session and its hold untouched, while the failing request
//     still gets its error ack;
//   - an open that CREATED the mapping and then fails still rolls it back
//     (TestCreatedOpenReportFailureStillDiscardsMapping,
//     TestCreatedOpenOKAckFailureStillDiscardsMapping);
//   - a normal successful join leaves the healthy mapping serving
//     (TestNoRegressionLaterJoinKeepsHealthyMappingAndSessions).

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/direct"
	"sharebridge/agent/internal/signaling"
)

// openOwnershipClient is the mock signaling client with independently
// controllable endpoint-report and OK-open-ack failures. Only status-"ok"
// open_acks can be failed, so the error ack the failing request must still
// receive always gets through.
type openOwnershipClient struct {
	*mockSignalingClient

	mu        sync.Mutex
	reportErr error
	okAckErr  error
}

func (c *openOwnershipClient) setReportErr(err error) {
	c.mu.Lock()
	c.reportErr = err
	c.mu.Unlock()
}

func (c *openOwnershipClient) setOKAckErr(err error) {
	c.mu.Lock()
	c.okAckErr = err
	c.mu.Unlock()
}

func (c *openOwnershipClient) ReportEndpoint(ctx context.Context, ip string, port int, status string) error {
	c.mu.Lock()
	err := c.reportErr
	c.mu.Unlock()
	if err != nil {
		return err
	}
	return c.mockSignalingClient.ReportEndpoint(ctx, ip, port, status)
}

func (c *openOwnershipClient) OpenAck(ctx context.Context, ack signaling.OpenAck) error {
	if ack.Status == "ok" {
		c.mu.Lock()
		err := c.okAckErr
		c.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return c.mockSignalingClient.OpenAck(ctx, ack)
}

// ownershipFixture is a ready direct state wired end to end: real SignalGate,
// real OnDemandPort over the supplied mapper, real Reporter over the
// controllable client.
type ownershipFixture struct {
	daemon   *Daemon
	client   *openOwnershipClient
	mapper   *remapDirectMapper
	port     *direct.OnDemandPort
	reporter *direct.Reporter
}

func newOwnershipFixture(t *testing.T, granted int) *ownershipFixture {
	t.Helper()
	cfg := &config.Config{SignalingURL: "ws://localhost:8080", APIKey: "test-key"}
	st := newMockStore()
	mapper := newRemapDirectMapper(granted)
	client := &openOwnershipClient{
		mockSignalingClient: newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID()),
	}
	port := direct.NewOnDemandPortOwned(mapper, 443, 8443, time.Minute, "test", "192.168.1.20")
	reporter := direct.NewReporter(func(ctx context.Context, ip string, port int, status string) error {
		return client.ReportEndpoint(ctx, ip, port, status)
	})
	port.SetTransitionCallback(reporter.OnTransition)

	d := &Daemon{
		store:     st,
		signaling: client,
		direct: &directState{
			ready:    true,
			gate:     direct.NewSignalGate(st.GetAgentID(), func(string, direct.RouteKind) bool { return true }),
			port:     port,
			mapper:   mapper,
			reporter: reporter,
		},
	}
	t.Cleanup(func() {
		_ = port.Close()
		reporter.Close()
	})
	return &ownershipFixture{daemon: d, client: client, mapper: mapper, port: port, reporter: reporter}
}

// openHealthyRecipientA drives one full successful open and then puts an
// active direct session and an in-flight stream hold on the mapping. It
// returns the session id, the hold token (which is the mapping's open-epoch
// identity) and the granted port, so a later open's failure can be checked
// against all three.
func (f *ownershipFixture) openHealthyRecipientA(t *testing.T) (string, uint64, int) {
	t.Helper()
	f.daemon.handleOpenSignal(openSignalMessageLease(1, "nonce-A", 600))
	if !f.port.Open() {
		t.Fatalf("recipient A's open did not open the mapping")
	}
	granted := f.port.GrantedPort()
	if granted == 0 {
		t.Fatalf("recipient A's open left no granted port")
	}
	sess, err := f.port.BeginSession("SHARE123")
	if err != nil {
		t.Fatalf("BeginSession: %v", err)
	}
	hold := f.port.Begin()
	if hold == 0 {
		t.Fatalf("Begin returned the zero hold token on an open port")
	}
	if !f.client.hasSentMessage("open_ack", map[string]any{"status": "ok", "was_already_open": false}) {
		t.Fatalf("recipient A's open was not acked OK: %#v", f.client.messagesSnapshot())
	}
	return sess, hold, granted
}

// assertRecipientASurvives is the survival contract: the healthy mapping, its
// granted port, its logical-open state (the hold token still belongs to the
// live open epoch), its active session and its router mapping are all
// untouched. `Open()` false, a changed hold epoch, or a dropped session would
// each mean some discardFencedMapping ran against A's mapping.
func (f *ownershipFixture) assertRecipientASurvives(t *testing.T, sess string, hold uint64, granted int) {
	t.Helper()
	if !f.port.Open() {
		t.Fatalf("the healthy mapping was torn down by a later open's failure")
	}
	if got := f.port.GrantedPort(); got != granted {
		t.Fatalf("granted port = %d, want the healthy mapping's %d", got, granted)
	}
	if got := f.port.State(); got != direct.StateOpen {
		t.Fatalf("port state = %v, want StateOpen", got)
	}
	if !f.port.SessionActive(sess) {
		t.Fatalf("the healthy direct session was dropped by a later open's failure")
	}
	if got := f.port.Begin(); got != hold {
		t.Fatalf("the hold epoch changed (Begin() = %d, want %d): the mapping's holds were cleared", got, hold)
	}
	listing, err := f.mapper.ListPortMappings()
	if err != nil {
		t.Fatalf("ListPortMappings: %v", err)
	}
	if len(listing) != 1 {
		t.Fatalf("%d mapping(s) survived, want exactly the healthy one: %#v", len(listing), listing)
	}
}

// assertFailedOpenRolledBack asserts the preserved created-open contract: the
// failing request gets its error ack, no OK ack beyond the expected count, no
// logical-open state, and no router mapping.
func (f *ownershipFixture) assertFailedOpenRolledBack(t *testing.T) {
	t.Helper()
	if f.client.hasSentMessage("open_ack", map[string]any{"status": "ok"}) {
		t.Fatalf("an OK open_ack was emitted for an unsuccessful open: %#v", f.client.messagesSnapshot())
	}
	if !f.client.hasSentMessage("open_ack", map[string]any{"status": "error"}) {
		t.Fatalf("an unsuccessful open must still get an error open_ack, got %#v", f.client.messagesSnapshot())
	}
	if f.port.Open() {
		t.Fatalf("the created mapping must be discarded after its open failed")
	}
	if got := f.port.State(); got != direct.StateClosed {
		t.Fatalf("port state = %v, want StateClosed after the created mapping was rolled back", got)
	}
	if got := f.port.GrantedPort(); got != 0 {
		t.Fatalf("granted port = %d, want 0 after the created mapping was rolled back", got)
	}
	listing, err := f.mapper.ListPortMappings()
	if err != nil {
		t.Fatalf("ListPortMappings: %v", err)
	}
	if len(listing) != 0 {
		t.Fatalf("%d mapping(s) survived a created open's failure: %#v", len(listing), listing)
	}
}

// TestLaterOpenReportFailureKeepsHealthyMapping is the audit's exact scenario
// for the report-failure arm (daemon.go's DiscardOpenMapping after
// confirmOpenEndpoint fails). Recipient A opened successfully and has an
// active session and hold; recipient B's subsequent open (the already-open
// non-renewal fast path) has its endpoint report fail. B must get the
// open_failed error ack, and A's mapping, logical-open state, session and hold
// must all survive untouched.
func TestLaterOpenReportFailureKeepsHealthyMapping(t *testing.T) {
	f := newOwnershipFixture(t, 52401)
	sess, hold, granted := f.openHealthyRecipientA(t)

	f.client.setReportErr(errors.New("endpoint report send failed"))
	f.daemon.handleOpenSignal(openSignalMessageLease(2, "nonce-B-report", 30)) // 30s does not extend A's 600s deadline

	if !f.client.hasSentMessage("open_ack", map[string]any{"status": "error", "error": "open_failed"}) {
		t.Fatalf("recipient B's unsuccessful open must get an open_failed error ack, got %#v", f.client.messagesSnapshot())
	}
	if n := countOpenAcks(f.client.messagesSnapshot(), "ok"); n != 1 {
		t.Fatalf("ok open_ack count = %d, want exactly recipient A's", n)
	}
	f.assertRecipientASurvives(t, sess, hold, granted)
}

// TestLaterOpenOKAckFailureKeepsHealthyMapping is the audit's exact scenario
// for the OK-ack transport-failure arm (daemon.go's DiscardOpenMapping after
// signaling.OpenAck returns an error inside ackOpenSuccess). Recipient B joins
// A's healthy mapping, reports successfully, but its OK ack never reaches
// control. B must still get an error ack, and A's mapping/session/hold must
// survive.
func TestLaterOpenOKAckFailureKeepsHealthyMapping(t *testing.T) {
	f := newOwnershipFixture(t, 52402)
	sess, hold, granted := f.openHealthyRecipientA(t)

	f.client.setOKAckErr(errors.New("open_ack transport write failed"))
	f.daemon.handleOpenSignal(openSignalMessageLease(2, "nonce-B-ack", 30))

	if !f.client.hasSentMessage("open_ack", map[string]any{"status": "error"}) {
		t.Fatalf("recipient B's undelivered OK ack must be followed by an error ack, got %#v", f.client.messagesSnapshot())
	}
	if n := countOpenAcks(f.client.messagesSnapshot(), "ok"); n != 1 {
		t.Fatalf("ok open_ack count = %d, want exactly recipient A's (B's never reached the wire)", n)
	}
	f.assertRecipientASurvives(t, sess, hold, granted)
}

// TestLaterRenewalReportFailureKeepsHealthyMapping covers the renewal fast
// path: B's longer lease extends the existing mapping's lease (a router write
// that rebinds the mapping to B's generation) and B's report then fails. The
// mapping belongs to A and is healthy; the lease extension must not become a
// licence to tear A down.
func TestLaterRenewalReportFailureKeepsHealthyMapping(t *testing.T) {
	f := newOwnershipFixture(t, 52403)
	sess, hold, granted := f.openHealthyRecipientA(t)

	f.client.setReportErr(errors.New("renewal report send failed"))
	f.daemon.handleOpenSignal(openSignalMessageLease(2, "nonce-B-renew", 900)) // extends A's 600s deadline → renewal branch (900s is maxLease)

	if !f.client.hasSentMessage("open_ack", map[string]any{"status": "error", "error": "open_failed"}) {
		t.Fatalf("the failed renewal must get an open_failed error ack, got %#v", f.client.messagesSnapshot())
	}
	if n := countOpenAcks(f.client.messagesSnapshot(), "ok"); n != 1 {
		t.Fatalf("ok open_ack count = %d, want exactly recipient A's", n)
	}
	f.assertRecipientASurvives(t, sess, hold, granted)
}

// TestCreatedOpenReportFailureStillDiscardsMapping preserves the rollback that
// genuinely must happen: when an open CREATED the mapping and its report then
// fails, that created mapping must still be discarded.
func TestCreatedOpenReportFailureStillDiscardsMapping(t *testing.T) {
	f := newOwnershipFixture(t, 52404)
	f.client.setReportErr(errors.New("report send failed"))

	f.daemon.handleOpenSignal(openSignalMessageLease(1, "nonce-A", 600))

	f.assertFailedOpenRolledBack(t)
}

// TestCreatedOpenOKAckFailureStillDiscardsMapping is the OK-ack half of the
// preserved rollback: a created mapping must not outlive an undelivered OK ack.
func TestCreatedOpenOKAckFailureStillDiscardsMapping(t *testing.T) {
	f := newOwnershipFixture(t, 52405)
	f.client.setOKAckErr(errors.New("ack write failed"))

	f.daemon.handleOpenSignal(openSignalMessageLease(1, "nonce-A", 600))

	f.assertFailedOpenRolledBack(t)
}

// TestNoRegressionLaterJoinKeepsHealthyMappingAndSessions is the normal-path
// control: a second recipient whose report AND ack both succeed joins A's
// mapping (acked was_already_open=true) and A's session and hold keep serving.
func TestNoRegressionLaterJoinKeepsHealthyMappingAndSessions(t *testing.T) {
	f := newOwnershipFixture(t, 52406)
	sess, hold, granted := f.openHealthyRecipientA(t)

	f.daemon.handleOpenSignal(openSignalMessageLease(2, "nonce-B-join", 30))

	if !f.client.hasSentMessage("open_ack", map[string]any{"status": "ok", "was_already_open": true}) {
		t.Fatalf("a successful join must be acked was_already_open=true, got %#v", f.client.messagesSnapshot())
	}
	if n := countOpenAcks(f.client.messagesSnapshot(), "ok"); n != 2 {
		t.Fatalf("ok open_ack count = %d, want A's and the join's", n)
	}
	f.assertRecipientASurvives(t, sess, hold, granted)
}
