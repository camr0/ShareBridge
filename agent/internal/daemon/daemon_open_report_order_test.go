package daemon

// M4 remediation round C: the endpoint report that carries the ACTUAL mapped
// external port must be sent and CONFIRMED before the OK open_ack. These tests
// are the ordering/failure contract for that guarantee. They drive the real
// handleOpenSignal path over a real OnDemandPort and a real Reporter, with only
// the signaling transport (and, where needed, the mapper) substituted.
//
// The ordering assertion is channel-gated: the report send is held open by the
// test, and the fixture's OpenAck records a violation if an OK ack is emitted
// before that send completes. A bounded negative wait then proves the handler
// does not return early while the report is unconfirmed (on the pre-fix agent
// the OK ack is sent immediately, so both checks fail deterministically).

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/direct"
	"sharebridge/agent/internal/signaling"
)

// remapDirectMapper is the NAT-PMP "the router granted a different external
// port than requested" fixture: AddPortMapping always records (and returns) a
// fixed non-default external port, so a report that carries the requested or
// internal port (or 0) instead of the granted one is caught.
type remapDirectMapper struct {
	*recordingDirectMapper
	granted int
}

func newRemapDirectMapper(granted int) *remapDirectMapper {
	return &remapDirectMapper{
		recordingDirectMapper: &recordingDirectMapper{ip: "203.0.113.7"},
		granted:               granted,
	}
}

func (m *remapDirectMapper) AddPortMapping(ext, internal int, desc string, lease int) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mappings == nil {
		m.mappings = map[int]direct.PortMapping{}
	}
	m.mappings[m.granted] = direct.PortMapping{
		ExternalPort:   m.granted,
		InternalPort:   internal,
		InternalClient: m.InternalIP(),
		Protocol:       "TCP",
		Description:    desc,
	}
	return m.granted, nil
}

// reportGateClient is the mock signaling client with a controllable
// ReportEndpoint and an ordering detector: reportDone is set by the test's
// report hook once the endpoint report send has completed, and a successful
// OpenAck observed before that is recorded as a violation.
type reportGateClient struct {
	*mockSignalingClient

	mu       sync.Mutex
	reportFn func(ctx context.Context, ip string, port int, status string) error

	reportDone atomic.Bool
	orderBreak atomic.Bool
}

// setReportFn installs the controllable report hook. The hook is read from the
// Reporter's drain goroutine, so access is mutex-guarded to keep tests race
// free when they swap the hook between opens.
func (c *reportGateClient) setReportFn(fn func(ctx context.Context, ip string, port int, status string) error) {
	c.mu.Lock()
	c.reportFn = fn
	c.mu.Unlock()
}

func (c *reportGateClient) ReportEndpoint(ctx context.Context, ip string, port int, status string) error {
	c.mu.Lock()
	fn := c.reportFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, ip, port, status)
	}
	return c.mockSignalingClient.ReportEndpoint(ctx, ip, port, status)
}

func (c *reportGateClient) OpenAck(ctx context.Context, ack signaling.OpenAck) error {
	if ack.Status == "ok" && !c.reportDone.Load() {
		c.orderBreak.Store(true)
	}
	return c.mockSignalingClient.OpenAck(ctx, ack)
}

// reportOrderFixture is a ready direct state wired end to end: real SignalGate,
// real OnDemandPort over the supplied mapper, real Reporter over the supplied
// sending client.
type reportOrderFixture struct {
	daemon   *Daemon
	client   *reportGateClient
	mapper   direct.PortMapper
	port     *direct.OnDemandPort
	reporter *direct.Reporter
}

// newReportOrderFixture builds the fixture. The Reporter's send is the
// production wiring: it forwards to the signaling client's ReportEndpoint, so
// both the report and the ack land in the client's one ordered message log.
func newReportOrderFixture(t *testing.T, mapper direct.PortMapper) *reportOrderFixture {
	t.Helper()
	cfg := &config.Config{SignalingURL: "ws://localhost:8080", APIKey: "test-key"}
	st := newMockStore()
	client := &reportGateClient{mockSignalingClient: newMockSignalingClient(cfg.SignalingURL, cfg.APIKey, st.GetAgentID())}

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
	return &reportOrderFixture{daemon: d, client: client, mapper: mapper, port: port, reporter: reporter}
}

// blockingReportHook arms the fixture's ReportEndpoint so a call signals
// entered and then blocks until release (or ctx is done), returning ctx.Err()
// on cancellation. reportDone is set only after the underlying send completed,
// so the ordering detector sees the report as confirmed only after delivery.
func (f *reportOrderFixture) blockingReportHook(entered chan<- struct{}, release <-chan struct{}) {
	f.client.setReportFn(func(ctx context.Context, ip string, port int, status string) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		err := f.client.mockSignalingClient.ReportEndpoint(ctx, ip, port, status)
		if err == nil {
			f.client.reportDone.Store(true)
		}
		return err
	})
}

// assertNoOKAckForFailedReport fails when any OK open_ack was emitted on a
// report-failure path. It is distinct from the fence tests' helper only so the
// failure message names the actual condition.
func assertNoOKAckForFailedReport(t *testing.T, client *reportGateClient) {
	t.Helper()
	if client.hasSentMessage("open_ack", map[string]any{"status": "ok"}) {
		t.Fatalf("an OK open_ack was emitted after a failed endpoint report: %#v", client.messagesSnapshot())
	}
}

// countOpenAcks counts open_ack messages with the given status.
func countOpenAcks(messages []map[string]any, status string) int {
	n := 0
	for _, m := range messages {
		if m["type"] == "open_ack" && m["status"] == status {
			n++
		}
	}
	return n
}

// assertReportFailedOpen asserts the unsuccessful-open contract shared by every
// report failure: a single error open_ack (open_failed), no OK ack, no direct
// reachability advertised, no surviving mapping, and no port leftovers.
func assertReportFailedOpen(t *testing.T, f *reportOrderFixture) {
	t.Helper()
	assertNoOKAckForFailedReport(t, f.client)
	if !f.client.hasSentMessage("open_ack", map[string]any{"status": "error", "error": "open_failed"}) {
		t.Fatalf("expected an open_failed error open_ack, got %#v", f.client.messagesSnapshot())
	}
	if f.port.Open() {
		t.Fatalf("the on-demand port must not be open after a failed report")
	}
	if got := f.port.State(); got != direct.StateClosed {
		t.Fatalf("port state = %v, want StateClosed", got)
	}
	if got := f.port.GrantedPort(); got != 0 {
		t.Fatalf("granted port = %d, want 0 after a failed report", got)
	}
	listing, err := f.mapper.ListPortMappings()
	if err != nil {
		t.Fatalf("ListPortMappings: %v", err)
	}
	if len(listing) != 0 {
		t.Fatalf("%d mapping(s) survived a failed report: %#v", len(listing), listing)
	}
	if _, err := f.port.BeginSession("SHARE123"); err == nil {
		t.Fatalf("BeginSession succeeded while the port is closed after a failed report")
	}
}

// messageIndex returns the index of the first message matching msgType and the
// supplied predicate, or -1.
func messageIndex(messages []map[string]any, msgType string, match func(map[string]any) bool) int {
	for i, m := range messages {
		if m["type"] != msgType {
			continue
		}
		if match == nil || match(m) {
			return i
		}
	}
	return -1
}

// TestOpenSignalConfirmsMappedEndpointReportBeforeOKAck is the ordering proof:
// the endpoint report carrying the MAPPED external port (not the requested
// default, not 0, not the internal port) must complete before the OK open_ack.
// The report is held open until the test releases it, so on the pre-fix agent
// the OK ack necessarily lands while the report is unconfirmed and the
// detector records the violation.
func TestOpenSignalConfirmsMappedEndpointReportBeforeOKAck(t *testing.T) {
	const mappedPort = 52017 // non-default, != requested 443, != internal 8443
	f := newReportOrderFixture(t, newRemapDirectMapper(mappedPort))

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	f.blockingReportHook(entered, release)

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.daemon.handleOpenSignal(openSignalMessage())
	}()

	awaitRecv(t, entered, "endpoint report send entering")

	// The report send is held open. The handler must NOT return while the
	// report is unconfirmed: returning early is exactly the pre-fix bug (the
	// OK ack is emitted before DDNS has the mapped port). This bounded
	// negative wait is the assertion, not synchronization between goroutines.
	select {
	case <-done:
		t.Fatalf("handleOpenSignal returned (OK open_ack emitted) while the endpoint report send was unconfirmed")
	case <-time.After(250 * time.Millisecond):
	}

	close(release)
	awaitClosed(t, done, "handleOpenSignal")

	if f.client.orderBreak.Load() {
		t.Fatalf("the OK open_ack was emitted before the endpoint report was confirmed: %#v", f.client.messagesSnapshot())
	}
	messages := f.client.messagesSnapshot()
	reportIdx := messageIndex(messages, "report_endpoint", func(m map[string]any) bool {
		port, _ := m["port"].(int)
		return port == mappedPort
	})
	ackIdx := messageIndex(messages, "open_ack", func(m map[string]any) bool {
		return m["status"] == "ok"
	})
	if reportIdx < 0 {
		t.Fatalf("no report_endpoint carrying the mapped external port %d: %#v", mappedPort, messages)
	}
	if ackIdx < 0 {
		t.Fatalf("no OK open_ack: %#v", messages)
	}
	if reportIdx > ackIdx {
		t.Fatalf("endpoint report index %d must precede the OK open_ack index %d: %#v", reportIdx, ackIdx, messages)
	}
	if got := messages[reportIdx]["port"]; got != mappedPort {
		t.Fatalf("report_endpoint port = %v, want the mapped external port %d", got, mappedPort)
	}
	if got := messages[reportIdx]["ip"]; got != "203.0.113.7" {
		t.Fatalf("report_endpoint ip = %v, want the fresh mapper IP", got)
	}
	if got := messages[ackIdx]["granted_port"]; got != mappedPort {
		t.Fatalf("open_ack granted_port = %v, want the mapped external port %d", got, mappedPort)
	}
}

// TestOpenSignalReportSendFailureFailsOpen covers failure (a): the report send
// returns an error. The open must be unsuccessful — error ack, no OK ack, no
// mapping, no leftovers.
func TestOpenSignalReportSendFailureFailsOpen(t *testing.T) {
	f := newReportOrderFixture(t, newRemapDirectMapper(52018))
	f.client.setReportFn(func(ctx context.Context, ip string, port int, status string) error {
		return errors.New("report send failed")
	})

	f.daemon.handleOpenSignal(openSignalMessage())

	assertReportFailedOpen(t, f)

	// No wedge: a later open whose report succeeds still works.
	f.client.setReportFn(nil)
	f.daemon.handleOpenSignal(openSignalMessageAt(2, "nonce-recovered"))
	if !f.client.hasSentMessage("open_ack", map[string]any{"status": "ok"}) {
		t.Fatalf("a subsequent open with a working report must succeed, got %#v", f.client.messagesSnapshot())
	}
}

// TestOpenSignalAlreadyOpenReportFailureTearsDownMapping proves the failure
// contract also holds on the already-open fast path: once a signal's report
// cannot be confirmed, the mapping is closed (no surviving mapping) even though
// the port was open from an earlier successful signal, and the ack is an error
// rather than a second OK.
func TestOpenSignalAlreadyOpenReportFailureTearsDownMapping(t *testing.T) {
	f := newReportOrderFixture(t, newRemapDirectMapper(52022))

	// First open succeeds and reports.
	f.daemon.handleOpenSignal(openSignalMessage())
	if !f.port.Open() {
		t.Fatalf("the first open must leave the mapping open")
	}

	// The second signal finds the port already open (its shorter lease takes
	// the non-renewal fast path); its report fails, so the open is unsuccessful.
	f.client.setReportFn(func(ctx context.Context, ip string, port int, status string) error {
		return errors.New("report send failed on the already-open path")
	})
	f.daemon.handleOpenSignal(openSignalMessageLease(2, "nonce-already-open", 5))

	if !f.client.hasSentMessage("open_ack", map[string]any{"status": "error", "error": "open_failed"}) {
		t.Fatalf("expected open_failed on the already-open report failure, got %#v", f.client.messagesSnapshot())
	}
	if acks := countOpenAcks(f.client.messagesSnapshot(), "ok"); acks != 1 {
		t.Fatalf("ok open_ack count = %d, want exactly the first open's", acks)
	}
	if f.port.Open() {
		t.Fatalf("a failed report on the already-open path must not leave the mapping open")
	}
	if got := f.port.GrantedPort(); got != 0 {
		t.Fatalf("granted port = %d, want 0 after teardown", got)
	}
	if listing, err := f.mapper.ListPortMappings(); err != nil || len(listing) != 0 {
		t.Fatalf("mappings after already-open report failure = %#v (err %v), want none", listing, err)
	}
}

// TestOpenSignalReportQueueFullFailsOpen covers failure (b): the reporter's
// advisory queue is full (the drain is wedged in a send), so the open report
// cannot be enqueued. The open must fail closed rather than silently dropping
// the report and acking OK.
func TestOpenSignalReportQueueFullFailsOpen(t *testing.T) {
	f := newReportOrderFixture(t, newRemapDirectMapper(52019))
	f.daemon.direct.reportTimeout = 200 * time.Millisecond

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)

	// Wedge the drain: the first advisory close report blocks in send forever.
	f.client.setReportFn(func(ctx context.Context, ip string, port int, status string) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil
	})
	f.reporter.OnTransition(direct.StateOpen, direct.StateClosed, 0)
	awaitRecv(t, entered, "drain entering the wedged send")

	// Fill the 64-slot queue so the next enqueue (the open report) cannot fit.
	for i := 0; i < 64; i++ {
		f.reporter.OnTransition(direct.StateOpen, direct.StateClosed, 0)
	}

	start := time.Now()
	f.daemon.handleOpenSignal(openSignalMessage())
	elapsed := time.Since(start)

	assertReportFailedOpen(t, f)
	if elapsed > 3*time.Second {
		t.Fatalf("the queue-full report failure took %s, want it bounded near the report timeout", elapsed)
	}
}

// TestOpenSignalReportConfirmationTimeoutFailsOpen covers failure (c): the
// report is enqueued, but its delivery never completes inside the bounded
// confirmation window. The open must fail closed, the wait must be bounded, and
// no daemon/port lock may be held across the wait.
func TestOpenSignalReportConfirmationTimeoutFailsOpen(t *testing.T) {
	f := newReportOrderFixture(t, newRemapDirectMapper(52020))
	f.daemon.direct.reportTimeout = 200 * time.Millisecond

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	f.client.setReportFn(func(ctx context.Context, ip string, port int, status string) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		// Deliberately ignore ctx: the daemon's confirmation bound must fire
		// even if the transport never answers, and it must not hold a lock.
		<-release
		return nil
	})

	start := time.Now()
	f.daemon.handleOpenSignal(openSignalMessage())
	elapsed := time.Since(start)

	awaitRecv(t, entered, "endpoint report send entering")
	assertReportFailedOpen(t, f)
	if elapsed > 3*time.Second {
		t.Fatalf("the report confirmation timeout took %s, want it bounded near the report timeout", elapsed)
	}

	// The state loop must be free: State()/Open() round-trip through it.
	stateCh := make(chan direct.PortState, 1)
	go func() { stateCh <- f.port.State() }()
	select {
	case <-stateCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("the port state loop is blocked across the report confirmation wait")
	}
	// ackMu must be free: SetGeneration publishes under it.
	genCh := make(chan struct{}, 1)
	go func() {
		f.port.SetGeneration(7, false)
		genCh <- struct{}{}
	}()
	select {
	case <-genCh:
	case <-time.After(2 * time.Second):
		t.Fatalf("ackMu is held across the report confirmation wait")
	}
}

// TestOpenSignalReportFailureLeavesNoGateBooking proves a failed-report open
// consumes no future capacity: a fresh signal (new nonce + sequence) is still
// admissible and succeeds once reporting works. The signal gate must not be
// left in a state that rejects the retry.
func TestOpenSignalReportFailureLeavesNoGateBooking(t *testing.T) {
	f := newReportOrderFixture(t, newRemapDirectMapper(52021))
	f.client.setReportFn(func(ctx context.Context, ip string, port int, status string) error {
		return errors.New("report send failed")
	})

	f.daemon.handleOpenSignal(openSignalMessage())
	assertReportFailedOpen(t, f)

	f.client.setReportFn(nil)
	f.daemon.handleOpenSignal(openSignalMessageAt(2, "nonce-after-failure"))
	if !f.client.hasSentMessage("open_ack", map[string]any{"status": "ok"}) {
		t.Fatalf("a fresh signal after a failed-report open must be admissible and succeed, got %#v", f.client.messagesSnapshot())
	}
}
