package directctl

import (
	"context"
	"testing"
	"time"
)

func TestEmitOpenUsesHelloAgentID(t *testing.T) {
	app, ctrl := newTestController(t)
	// agents.api_key_id is a required relation, so enrollment needs a real API
	// key record; use its generated ID as apiKeyID (the plan's literal "key-1"
	// would fail the relation constraint inside LoadOrCreateAgent).
	apiKeyID := mustAPIKey(t, app, "key-1").Id
	ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1")
	var emitted map[string]any
	ctrl.sendToAgentFn = func(ctx context.Context, keyID string, msg any) error {
		emitted = msg.(map[string]any)
		go func() {
			time.Sleep(5 * time.Millisecond)
			ctrl.HandleOpenAck(nil, apiKeyID, OpenAck{
				ShareID: "abc", Nonce: emitted["nonce"].(string), Seq: emitted["seq"].(uint64),
				GrantedPort: 443, PublicIP: "1.2.3.4", Status: "ok",
			})
		}()
		return nil
	}
	ack, err := ctrl.EmitOpen(context.Background(), apiKeyID, "abc", "o", 120*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if emitted["agent_id"] != "agent-1" {
		t.Fatalf("agent_id = %v", emitted["agent_id"])
	}
	if ack.GrantedPort != 443 {
		t.Fatalf("ack = %+v", ack)
	}
}

func TestOpenAckValidatesFullTuple(t *testing.T) {
	app, ctrl := newTestController(t)
	apiKeyID := mustAPIKey(t, app, "key-1").Id
	ctrl.HandleHello(context.Background(), nil, apiKeyID, "acct-1", "agent-1")

	var emitted map[string]any
	started := make(chan struct{})
	ctrl.sendToAgentFn = func(ctx context.Context, keyID string, msg any) error {
		emitted = msg.(map[string]any)
		close(started)
		return nil
	}

	ackCh := make(chan OpenAck, 1)
	errCh := make(chan error, 1)
	go func() {
		ack, err := ctrl.EmitOpen(context.Background(), apiKeyID, "abc", "o", 120*time.Second)
		if err != nil {
			errCh <- err
			return
		}
		ackCh <- ack
	}()
	<-started

	nonce := emitted["nonce"].(string)
	seq := emitted["seq"].(uint64)

	// Wrong seq — discarded.
	ctrl.HandleOpenAck(nil, apiKeyID, OpenAck{ShareID: "abc", Nonce: nonce, Seq: seq + 1, GrantedPort: 443, PublicIP: "1.2.3.4", Status: "ok"})
	// Wrong share_id — discarded.
	ctrl.HandleOpenAck(nil, apiKeyID, OpenAck{ShareID: "wrong", Nonce: nonce, Seq: seq, GrantedPort: 443, PublicIP: "1.2.3.4", Status: "ok"})
	// Wrong apiKeyID — discarded.
	ctrl.HandleOpenAck(nil, "key-other", OpenAck{ShareID: "abc", Nonce: nonce, Seq: seq, GrantedPort: 443, PublicIP: "1.2.3.4", Status: "ok"})

	// No completion yet — the mismatched acks must not have satisfied the waiter.
	select {
	case ack := <-ackCh:
		t.Fatalf("mismatched ack completed waiter: %+v", ack)
	case err := <-errCh:
		t.Fatalf("mismatched ack errored waiter: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Correct full tuple — completes.
	ctrl.HandleOpenAck(nil, apiKeyID, OpenAck{ShareID: "abc", Nonce: nonce, Seq: seq, GrantedPort: 8080, PublicIP: "1.2.3.4", Status: "ok"})

	select {
	case ack := <-ackCh:
		if ack.GrantedPort != 8080 {
			t.Fatalf("ack = %+v", ack)
		}
	case err := <-errCh:
		t.Fatalf("valid ack errored: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for valid ack")
	}
}

func TestLateAckAfterTimeoutDiscarded(t *testing.T) {
	_, ctrl := newTestController(t)
	ctrl.ackTimeout = 20 * time.Millisecond
	ctrl.sendToAgentFn = func(ctx context.Context, apiKeyID string, msg any) error { return nil }
	if _, err := ctrl.EmitOpen(context.Background(), "key-1", "abc", "o", 120*time.Second); err == nil {
		t.Fatalf("expected timeout")
	}
}
