// agent/internal/relaychannel/channel_test.go
package relaychannel

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"sharebridge/agent/internal/multilane"
	"sharebridge/agent/internal/noise"
)

func TestSecureRelayChannelExposesAllLogicalLanes(t *testing.T) {
	channel, err := NewSecureRelayChannel(SecureRelayConfig{
		RelayURL:      "ws://relay.test",
		RelayJWT:      "token",
		StaticPrivate: mustGenerateKey(),
	})
	if err != nil {
		t.Fatalf("NewSecureRelayChannel: %v", err)
	}
	for _, lane := range []multilane.Lane{multilane.LaneControl, multilane.LaneMedia, multilane.LaneBulk} {
		if channel.Endpoint(lane) == nil {
			t.Fatalf("Endpoint(%d) is nil", lane)
		}
	}
	if channel.Endpoint(multilane.Lane(0x03)) != nil {
		t.Fatal("reserved lane unexpectedly has an endpoint")
	}
}

func TestSecureRelayEndpointThumbnailPriorityAndLaneValidation(t *testing.T) {
	channel, err := NewSecureRelayChannel(SecureRelayConfig{RelayURL: "ws://relay.test", RelayJWT: "token", StaticPrivate: mustGenerateKey()})
	if err != nil {
		t.Fatal(err)
	}
	_ = channel.scheduler.Close()
	classes := make(chan multilane.TrafficClass, 8)
	release := make(chan struct{}, 8)
	channel.scheduler = multilane.NewScheduler(func(class multilane.TrafficClass, _ multilane.Kind, _ []byte) error {
		classes <- class
		<-release
		return nil
	})
	t.Cleanup(func() { _ = channel.scheduler.Close() })

	media := channel.Endpoint(multilane.LaneMedia)
	if err := media.SendBinaryClass(multilane.ClassBulk, []byte{1}); err == nil {
		t.Fatal("media endpoint accepted bulk traffic class")
	}

	errs := make(chan error, 8)
	go func() {
		errs <- media.SendBinaryClass(multilane.ClassInteractiveMedia, make([]byte, multilane.BaseQuantumBytes))
	}()
	first := <-classes
	for i := 0; i < 5; i++ {
		go func() {
			errs <- media.SendBinaryClass(multilane.ClassInteractiveMedia, make([]byte, multilane.BaseQuantumBytes))
		}()
	}
	for i := 0; i < 2; i++ {
		go func() {
			errs <- media.SendBinaryClass(multilane.ClassThumbnail, make([]byte, multilane.BaseQuantumBytes))
		}()
	}
	// Hold the active write until all remaining producers have entered the
	// adapter's bounded scheduler, making both media subqueues continuously busy.
	time.Sleep(20 * time.Millisecond)
	release <- struct{}{}
	got := []multilane.TrafficClass{first}
	for len(got) < 8 {
		got = append(got, <-classes)
		release <- struct{}{}
	}
	for range got {
		if err := <-errs; err != nil {
			t.Fatalf("SendBinaryClass: %v", err)
		}
	}
	want := []multilane.TrafficClass{
		multilane.ClassInteractiveMedia, multilane.ClassInteractiveMedia,
		multilane.ClassInteractiveMedia, multilane.ClassThumbnail,
	}
	if !slices.Equal(got[1:5], want) {
		t.Fatalf("continuously busy media classes = %v, want %v", got[1:5], want)
	}
}

func TestSecureRelayEndpointsScheduleConcurrentMediaAndBulkThreeToOne(t *testing.T) {
	channel, _ := NewSecureRelayChannel(SecureRelayConfig{RelayURL: "ws://relay.test", RelayJWT: "token", StaticPrivate: mustGenerateKey()})
	_ = channel.scheduler.Close()
	classes := make(chan multilane.TrafficClass, 9)
	release := make(chan struct{}, 9)
	channel.scheduler = multilane.NewScheduler(func(class multilane.TrafficClass, _ multilane.Kind, _ []byte) error {
		classes <- class
		<-release
		return nil
	})
	t.Cleanup(func() { _ = channel.scheduler.Close() })
	results := make(chan error, 9)
	media := channel.Endpoint(multilane.LaneMedia)
	bulk := channel.Endpoint(multilane.LaneBulk)
	go func() { results <- media.SendBinary(make([]byte, multilane.BaseQuantumBytes)) }()
	got := []multilane.TrafficClass{<-classes}
	for i := 0; i < 6; i++ {
		go func() { results <- media.SendBinary(make([]byte, multilane.BaseQuantumBytes)) }()
	}
	for i := 0; i < 2; i++ {
		go func() { results <- bulk.SendBinary(make([]byte, multilane.BaseQuantumBytes)) }()
	}
	time.Sleep(20 * time.Millisecond)
	release <- struct{}{}
	for len(got) < 9 {
		got = append(got, <-classes)
		release <- struct{}{}
	}
	for range got {
		if err := <-results; err != nil {
			t.Fatalf("endpoint send: %v", err)
		}
	}
	want := []multilane.TrafficClass{
		multilane.ClassInteractiveMedia, multilane.ClassInteractiveMedia,
		multilane.ClassInteractiveMedia, multilane.ClassBulk,
	}
	if !slices.Equal(got[1:5], want) {
		t.Fatalf("continuously busy outer classes = %v, want %v", got[1:5], want)
	}
}

func TestSecureRelayEndpointRejectsOversizeBeforeWriterAndKeepsSessionUsable(t *testing.T) {
	channel, _ := NewSecureRelayChannel(SecureRelayConfig{RelayURL: "ws://relay.test", RelayJWT: "token", StaticPrivate: mustGenerateKey()})
	_ = channel.scheduler.Close()
	writes := 0
	channel.scheduler = multilane.NewScheduler(func(_ multilane.TrafficClass, _ multilane.Kind, _ []byte) error { writes++; return nil })
	t.Cleanup(func() { _ = channel.scheduler.Close() })
	control := channel.Endpoint(multilane.LaneControl)
	if err := control.SendBinary(make([]byte, MaxRelayPayloadBytes+1)); err == nil {
		t.Fatal("boundary+1 payload was accepted")
	}
	if writes != 0 {
		t.Fatalf("oversize payload reached writer %d times", writes)
	}
	if err := control.SendBinary(make([]byte, MaxRelayPayloadBytes)); err != nil {
		t.Fatalf("exact boundary rejected: %v", err)
	}
	if err := control.SendBinary([]byte{1}); err != nil {
		t.Fatalf("following payload rejected: %v", err)
	}
	if writes != 2 {
		t.Fatalf("writes = %d, want 2", writes)
	}
}

func TestSecureRelayEndpointBackpressureAndWriterFailureUnblockAll(t *testing.T) {
	channel, _ := NewSecureRelayChannel(SecureRelayConfig{RelayURL: "ws://relay.test", RelayJWT: "token", StaticPrivate: mustGenerateKey()})
	_ = channel.scheduler.Close()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	wantErr := fmt.Errorf("relay writer failed")
	channel.scheduler = multilane.NewScheduler(func(_ multilane.TrafficClass, _ multilane.Kind, _ []byte) error {
		entered <- struct{}{}
		<-release
		return wantErr
	})
	t.Cleanup(func() { _ = channel.scheduler.Close() })

	bulk := channel.Endpoint(multilane.LaneBulk)
	results := make(chan error, 5)
	for i := 0; i < 5; i++ {
		go func() { results <- bulk.SendBinary(make([]byte, multilane.BaseQuantumBytes)) }()
	}
	<-entered
	select {
	case err := <-results:
		t.Fatalf("sender returned before writer released: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	for i := 0; i < 5; i++ {
		if err := <-results; err == nil {
			t.Fatal("sender was not unblocked with an error")
		}
	}
}

func TestSecureRelayChannelStartFailureClosesLifecycleOnce(t *testing.T) {
	channel, _ := NewSecureRelayChannel(SecureRelayConfig{RelayURL: "ws://127.0.0.1:1", RelayJWT: "token", StaticPrivate: mustGenerateKey()})
	closed := 0
	channel.SetOnClose(func() { closed++ })
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := channel.Start(ctx); err == nil {
		t.Fatal("Start unexpectedly succeeded")
	}
	_ = channel.Close()
	if closed != 1 {
		t.Fatalf("onClose calls = %d, want 1", closed)
	}
	if err := channel.Endpoint(multilane.LaneControl).SendText("after failure"); err == nil {
		t.Fatal("scheduler remained usable after failed Start")
	}
}

func TestSecureRelayChannelOnCloseMayCallCloseReentrantly(t *testing.T) {
	channel, _ := NewSecureRelayChannel(SecureRelayConfig{RelayURL: "ws://relay.test", RelayJWT: "token", StaticPrivate: mustGenerateKey()})
	callbackReturned := make(chan struct{})
	closeReturned := make(chan struct{})
	calls := 0
	channel.SetOnClose(func() {
		calls++
		_ = channel.Close()
		close(callbackReturned)
	})
	go func() {
		_ = channel.Close()
		close(closeReturned)
	}()
	for name, done := range map[string]<-chan struct{}{
		"callback": callbackReturned,
		"Close":    closeReturned,
	} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s deadlocked during re-entrant close", name)
		}
	}
	if calls != 1 {
		t.Fatalf("onClose calls = %d, want 1", calls)
	}
}

func TestSecureRelayChannel_HandshakeAndRoundTrip(t *testing.T) {
	serverStatic, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey(serverStatic): %v", err)
	}
	expectedResponderPub := serverStatic.PublicKey().Bytes()

	relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		defer conn.CloseNow()

		ctx := r.Context()
		_, helloBytes, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read hello: %v", err)
		}
		if !bytes.Contains(helloBytes, []byte(`"relay.jwt.token"`)) {
			t.Fatalf("unexpected hello payload: %s", string(helloBytes))
		}

		initiator, err := noise.NewInitiator()
		if err != nil {
			t.Fatalf("NewInitiator: %v", err)
		}
		msg1, err := initiator.WriteMessage1()
		if err != nil {
			t.Fatalf("WriteMessage1: %v", err)
		}
		if err := conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameHandshake, msg1)); err != nil {
			t.Fatalf("Write msg1: %v", err)
		}

		_, msg2Frame, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read msg2: %v", err)
		}
		msg2, err := mustDecodeFramePayload(msg2Frame, FrameHandshake)
		if err != nil {
			t.Fatalf("Decode msg2: %v", err)
		}
		if err := initiator.ReadMessage2(msg2); err != nil {
			t.Fatalf("ReadMessage2: %v", err)
		}
		if !bytes.Equal(initiator.RemoteStaticPub(), expectedResponderPub) {
			t.Fatal("responder static public key mismatch")
		}

		msg3, err := initiator.WriteMessage3()
		if err != nil {
			t.Fatalf("WriteMessage3: %v", err)
		}
		if err := conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameHandshake, msg3)); err != nil {
			t.Fatalf("Write msg3: %v", err)
		}

		iSend, iRecv := initiator.Split()
		textCipher, err := iSend.Encrypt(nil, mustEnvelope(t, multilane.LaneControl, []byte(`{"type":"list_request","path":""}`)))
		if err != nil {
			t.Fatalf("Encrypt text: %v", err)
		}
		if err := conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameText, textCipher)); err != nil {
			t.Fatalf("Write encrypted text: %v", err)
		}

		_, replyFrame, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read encrypted reply: %v", err)
		}
		replyCipher, err := mustDecodeFramePayload(replyFrame, FrameText)
		if err != nil {
			t.Fatalf("Decode reply frame: %v", err)
		}
		replyPlain, err := iRecv.Decrypt(nil, replyCipher)
		if err != nil {
			t.Fatalf("Decrypt reply: %v", err)
		}
		replyLane, replyPayload, err := multilane.DecodeEnvelope(replyPlain)
		if err != nil || replyLane != multilane.LaneControl || string(replyPayload) != `{"type":"hello"}` {
			t.Fatalf("reply envelope lane=%d payload=%s err=%v", replyLane, replyPayload, err)
		}
	}))
	defer relayServer.Close()

	channel, err := NewSecureRelayChannel(SecureRelayConfig{
		RelayURL:      "ws" + strings.TrimPrefix(relayServer.URL, "http"),
		RelayJWT:      "relay.jwt.token",
		StaticPrivate: serverStatic,
	})
	if err != nil {
		t.Fatalf("NewSecureRelayChannel: %v", err)
	}

	gotMessages := make(chan []byte, 1)
	channel.SetOnMessage(func(data []byte) { gotMessages <- append([]byte(nil), data...) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := channel.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer channel.Close()

	if got := <-gotMessages; string(got) != `{"type":"list_request","path":""}` {
		t.Fatalf("got message = %s", got)
	}
	if err := channel.SendText(`{"type":"hello"}`); err != nil {
		t.Fatalf("SendText: %v", err)
	}
}

func TestSecureRelayChannel_RequiresConfig(t *testing.T) {
	_, err := NewSecureRelayChannel(SecureRelayConfig{})
	if err == nil {
		t.Fatal("expected error for missing config")
	}
	if !strings.Contains(err.Error(), "missing required config") {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = NewSecureRelayChannel(SecureRelayConfig{RelayURL: "ws://test"})
	if err == nil {
		t.Fatal("expected error for missing RelayJWT")
	}

	_, err = NewSecureRelayChannel(SecureRelayConfig{RelayURL: "ws://test", RelayJWT: "token"})
	if err == nil {
		t.Fatal("expected error for missing StaticPrivate")
	}
}

func TestSecureRelayChannel_SendBinary(t *testing.T) {
	serverStatic, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey(serverStatic): %v", err)
	}

	relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		defer conn.CloseNow()

		ctx := r.Context()
		// Read hello
		_, _, err = conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read hello: %v", err)
		}

		// Run handshake as initiator
		initiator, _ := noise.NewInitiator()
		msg1, _ := initiator.WriteMessage1()
		conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameHandshake, msg1))

		_, msg2Frame, _ := conn.Read(ctx)
		msg2, _ := mustDecodeFramePayload(msg2Frame, FrameHandshake)
		initiator.ReadMessage2(msg2)

		msg3, _ := initiator.WriteMessage3()
		conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameHandshake, msg3))

		iSend, iRecv := initiator.Split()

		// Receive binary message
		_, binFrame, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read binary: %v", err)
		}
		binCipher, err := mustDecodeFramePayload(binFrame, FrameBinary)
		if err != nil {
			t.Fatalf("Decode binary frame: %v", err)
		}
		binPlain, err := iRecv.Decrypt(nil, binCipher)
		if err != nil {
			t.Fatalf("Decrypt binary: %v", err)
		}
		binLane, binPayload, err := multilane.DecodeEnvelope(binPlain)
		if err != nil || binLane != multilane.LaneMedia || string(binPayload) != "binary-data" {
			t.Fatalf("binary envelope lane=%d payload=%s err=%v", binLane, binPayload, err)
		}

		// Send reply
		replyCipher, _ := iSend.Encrypt(nil, mustEnvelope(t, multilane.LaneMedia, []byte("ok")))
		conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameText, replyCipher))
	}))
	defer relayServer.Close()

	channel, _ := NewSecureRelayChannel(SecureRelayConfig{
		RelayURL:      "ws" + strings.TrimPrefix(relayServer.URL, "http"),
		RelayJWT:      "relay.jwt.token",
		StaticPrivate: serverStatic,
	})

	gotMessages := make(chan []byte, 1)
	channel.Endpoint(multilane.LaneMedia).SetOnMessage(func(data []byte) { gotMessages <- data })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = channel.Start(ctx)
	defer channel.Close()

	// Send binary
	if err := channel.Endpoint(multilane.LaneMedia).SendBinaryClass(multilane.ClassThumbnail, []byte("binary-data")); err != nil {
		t.Fatalf("SendBinary: %v", err)
	}

	// Wait for reply
	select {
	case got := <-gotMessages:
		if string(got) != "ok" {
			t.Fatalf("unexpected reply: %s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for reply")
	}
}

func TestSecureRelayChannel_ConcurrentSendsAreSerialized(t *testing.T) {
	serverStatic, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey(serverStatic): %v", err)
	}

	const sends = 32
	received := make(chan []string, 1)
	relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Fatalf("Accept: %v", err)
		}
		defer conn.CloseNow()

		ctx := r.Context()
		if _, _, err := conn.Read(ctx); err != nil {
			t.Fatalf("Read hello: %v", err)
		}

		initiator, err := noise.NewInitiator()
		if err != nil {
			t.Fatalf("NewInitiator: %v", err)
		}
		msg1, err := initiator.WriteMessage1()
		if err != nil {
			t.Fatalf("WriteMessage1: %v", err)
		}
		if err := conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameHandshake, msg1)); err != nil {
			t.Fatalf("Write msg1: %v", err)
		}
		_, msg2Frame, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("Read msg2: %v", err)
		}
		msg2, err := mustDecodeFramePayload(msg2Frame, FrameHandshake)
		if err != nil {
			t.Fatalf("Decode msg2: %v", err)
		}
		if err := initiator.ReadMessage2(msg2); err != nil {
			t.Fatalf("ReadMessage2: %v", err)
		}
		msg3, err := initiator.WriteMessage3()
		if err != nil {
			t.Fatalf("WriteMessage3: %v", err)
		}
		if err := conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameHandshake, msg3)); err != nil {
			t.Fatalf("Write msg3: %v", err)
		}

		_, iRecv := initiator.Split()
		var out []string
		for i := 0; i < sends; i++ {
			_, frameBytes, err := conn.Read(ctx)
			if err != nil {
				t.Fatalf("Read encrypted frame %d: %v", i, err)
			}
			payload, err := mustDecodeFramePayload(frameBytes, FrameText)
			if err != nil {
				t.Fatalf("Decode encrypted frame %d: %v", i, err)
			}
			plain, err := iRecv.Decrypt(nil, payload)
			if err != nil {
				t.Fatalf("Decrypt encrypted frame %d: %v", i, err)
			}
			lane, decoded, err := multilane.DecodeEnvelope(plain)
			if err != nil || (lane != multilane.LaneMedia && lane != multilane.LaneBulk) {
				t.Fatalf("DecodeEnvelope frame %d: lane=%d err=%v", i, lane, err)
			}
			out = append(out, fmt.Sprintf("%d:%s", lane, decoded))
		}
		received <- out
	}))
	defer relayServer.Close()

	channel, err := NewSecureRelayChannel(SecureRelayConfig{
		RelayURL:      "ws" + strings.TrimPrefix(relayServer.URL, "http"),
		RelayJWT:      "relay.jwt.token",
		StaticPrivate: serverStatic,
	})
	if err != nil {
		t.Fatalf("NewSecureRelayChannel: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := channel.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer channel.Close()

	var wg sync.WaitGroup
	for i := 0; i < sends; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lane := multilane.LaneMedia
			if i%2 == 1 {
				lane = multilane.LaneBulk
			}
			if err := channel.Endpoint(lane).SendText(fmt.Sprintf("msg-%02d", i)); err != nil {
				t.Errorf("SendText(%d): %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	select {
	case got := <-received:
		if len(got) != sends {
			t.Fatalf("received %d frames, want %d", len(got), sends)
		}
		seen := make(map[string]bool, sends)
		for _, msg := range got {
			seen[msg] = true
		}
		for i := 0; i < sends; i++ {
			lane := multilane.LaneMedia
			if i%2 == 1 {
				lane = multilane.LaneBulk
			}
			want := fmt.Sprintf("%d:msg-%02d", lane, i)
			if !seen[want] {
				t.Fatalf("missing %q in %#v", want, got)
			}
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for concurrent sends")
	}
}

func TestSecureRelayChannel_CloseCallsOnClose(t *testing.T) {
	serverStatic, _ := ecdh.P256().GenerateKey(rand.Reader)

	onCloseCalled := make(chan struct{}, 1)

	relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _ := websocket.Accept(w, r, nil)
		defer conn.CloseNow()
		ctx := r.Context()
		// Read hello
		conn.Read(ctx)
		// Run handshake
		initiator, _ := noise.NewInitiator()
		msg1, _ := initiator.WriteMessage1()
		conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameHandshake, msg1))
		_, msg2Frame, _ := conn.Read(ctx)
		msg2, _ := mustDecodeFramePayload(msg2Frame, FrameHandshake)
		initiator.ReadMessage2(msg2)
		msg3, _ := initiator.WriteMessage3()
		conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameHandshake, msg3))
		// Wait for channel to close (will cause read to fail)
		conn.Read(ctx)
	}))
	defer relayServer.Close()

	channel, _ := NewSecureRelayChannel(SecureRelayConfig{
		RelayURL:      "ws" + strings.TrimPrefix(relayServer.URL, "http"),
		RelayJWT:      "relay.jwt.token",
		StaticPrivate: serverStatic,
	})
	channel.SetOnClose(func() { onCloseCalled <- struct{}{} })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = channel.Start(ctx)

	// Close the channel
	time.Sleep(100 * time.Millisecond) // let readLoop start
	channel.Close()

	select {
	case <-onCloseCalled:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("onClose was not called")
	}
}

func TestSecureRelayChannel_HandshakeFailure(t *testing.T) {
	serverStatic, _ := ecdh.P256().GenerateKey(rand.Reader)

	relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _ := websocket.Accept(w, r, nil)
		defer conn.CloseNow()
		ctx := r.Context()
		conn.Read(ctx) // read hello

		// Send malformed handshake frame (wrong length)
		conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameHandshake, []byte("bad")))
	}))
	defer relayServer.Close()

	channel, _ := NewSecureRelayChannel(SecureRelayConfig{
		RelayURL:      "ws" + strings.TrimPrefix(relayServer.URL, "http"),
		RelayJWT:      "relay.jwt.token",
		StaticPrivate: serverStatic,
	})
	closed := 0
	channel.SetOnClose(func() { closed++ })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := channel.Start(ctx)
	if err == nil {
		t.Fatal("expected handshake failure")
	}
	// Should contain noise error about expected 65 bytes
	if !strings.Contains(err.Error(), "expected 65 bytes") {
		t.Fatalf("unexpected error: %v", err)
	}
	_ = channel.Close()
	if closed != 1 {
		t.Fatalf("onClose calls = %d, want 1", closed)
	}
	if err := channel.Endpoint(multilane.LaneControl).SendText("after failed handshake"); err == nil {
		t.Fatal("scheduler remained usable after failed Noise handshake")
	}
}

func TestSecureRelayChannel_OnOpenCallback(t *testing.T) {
	serverStatic, _ := ecdh.P256().GenerateKey(rand.Reader)
	onOpenCalled := make(chan struct{}, 1)

	relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _ := websocket.Accept(w, r, nil)
		defer conn.CloseNow()
		ctx := r.Context()
		conn.Read(ctx) // hello
		initiator, _ := noise.NewInitiator()
		msg1, _ := initiator.WriteMessage1()
		conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameHandshake, msg1))
		_, msg2Frame, _ := conn.Read(ctx)
		msg2, _ := mustDecodeFramePayload(msg2Frame, FrameHandshake)
		initiator.ReadMessage2(msg2)
		msg3, _ := initiator.WriteMessage3()
		conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameHandshake, msg3))
		// Keep alive briefly
		time.Sleep(500 * time.Millisecond)
	}))
	defer relayServer.Close()

	channel, _ := NewSecureRelayChannel(SecureRelayConfig{
		RelayURL:      "ws" + strings.TrimPrefix(relayServer.URL, "http"),
		RelayJWT:      "relay.jwt.token",
		StaticPrivate: serverStatic,
	})
	channel.SetOnOpen(func() { onOpenCalled <- struct{}{} })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = channel.Start(ctx)
	defer channel.Close()

	select {
	case <-onOpenCalled:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("onOpen was not called")
	}
}

func TestSecureRelayChannel_InvalidEncryptedLaneClosesWholeSet(t *testing.T) {
	serverStatic, _ := ecdh.P256().GenerateKey(rand.Reader)
	onCloseCalled := make(chan struct{}, 1)

	relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _ := websocket.Accept(w, r, nil)
		defer conn.CloseNow()
		ctx := r.Context()
		conn.Read(ctx) // hello
		initiator, _ := noise.NewInitiator()
		msg1, _ := initiator.WriteMessage1()
		conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameHandshake, msg1))
		_, msg2Frame, _ := conn.Read(ctx)
		msg2, _ := mustDecodeFramePayload(msg2Frame, FrameHandshake)
		initiator.ReadMessage2(msg2)
		msg3, _ := initiator.WriteMessage3()
		conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameHandshake, msg3))

		iSend, _ := initiator.Split()
		ciphertext, _ := iSend.Encrypt(nil, []byte{0x03, 0x01})
		conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameBinary, ciphertext))
	}))
	defer relayServer.Close()

	channel, _ := NewSecureRelayChannel(SecureRelayConfig{
		RelayURL:      "ws" + strings.TrimPrefix(relayServer.URL, "http"),
		RelayJWT:      "relay.jwt.token",
		StaticPrivate: serverStatic,
	})
	channel.SetOnClose(func() { onCloseCalled <- struct{}{} })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = channel.Start(ctx)

	select {
	case <-onCloseCalled:
		// success - one invalid encrypted lane closes the shared relay set
	case <-time.After(2 * time.Second):
		t.Fatal("onClose was not called after invalid encrypted lane")
	}
}

func TestSecureRelayChannelRejectsTrailingFrameInWebSocketMessage(t *testing.T) {
	serverStatic, _ := ecdh.P256().GenerateKey(rand.Reader)
	onCloseCalled := make(chan struct{}, 1)
	relayServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _ := websocket.Accept(w, r, nil)
		defer conn.CloseNow()
		ctx := r.Context()
		_, _, _ = conn.Read(ctx)
		initiator, _ := noise.NewInitiator()
		msg1, _ := initiator.WriteMessage1()
		_ = conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameHandshake, msg1))
		_, msg2Frame, _ := conn.Read(ctx)
		msg2, _ := mustDecodeFramePayload(msg2Frame, FrameHandshake)
		_ = initiator.ReadMessage2(msg2)
		msg3, _ := initiator.WriteMessage3()
		_ = conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameHandshake, msg3))
		iSend, _ := initiator.Split()
		first, _ := iSend.Encrypt(nil, mustEnvelope(t, multilane.LaneControl, []byte("one")))
		second, _ := iSend.Encrypt(nil, mustEnvelope(t, multilane.LaneControl, []byte("two")))
		combined := append(mustFrame(FrameText, first), mustFrame(FrameText, second)...)
		_ = conn.Write(ctx, websocket.MessageBinary, combined)
	}))
	defer relayServer.Close()

	channel, _ := NewSecureRelayChannel(SecureRelayConfig{
		RelayURL: "ws" + strings.TrimPrefix(relayServer.URL, "http"), RelayJWT: "relay.jwt.token", StaticPrivate: serverStatic,
	})
	channel.SetOnClose(func() { onCloseCalled <- struct{}{} })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := channel.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case <-onCloseCalled:
	case <-time.After(2 * time.Second):
		t.Fatal("onClose was not called after trailing relay frame")
	}
}

// Helper functions for tests

func mustEnvelope(t *testing.T, lane multilane.Lane, payload []byte) []byte {
	t.Helper()
	encoded, err := multilane.EncodeEnvelope(lane, payload)
	if err != nil {
		t.Fatalf("EncodeEnvelope: %v", err)
	}
	return encoded
}

func mustFrame(kind byte, payload []byte) []byte {
	frame, err := WriteFrame(kind, payload)
	if err != nil {
		panic(fmt.Sprintf("mustFrame: %v", err))
	}
	return frame
}

func mustDecodeFramePayload(raw []byte, wantKind byte) ([]byte, error) {
	if len(raw) < 5 {
		return nil, fmt.Errorf("frame too short")
	}
	kind := raw[0]
	length := int(binary.BigEndian.Uint32(raw[1:5]))
	if kind != wantKind {
		return nil, fmt.Errorf("unexpected frame kind: got 0x%02x want 0x%02x", kind, wantKind)
	}
	if len(raw) < 5+length {
		return nil, fmt.Errorf("incomplete frame payload")
	}
	return raw[5 : 5+length], nil
}

func mustGenerateKey() *ecdh.PrivateKey {
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return key
}
