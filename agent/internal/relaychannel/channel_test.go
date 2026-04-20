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
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"sharebridge/agent/internal/noise"
)

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
		textCipher, err := iSend.Encrypt(nil, []byte(`{"type":"list_request","path":""}`))
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
		if string(replyPlain) != `{"type":"hello"}` {
			t.Fatalf("reply plaintext = %s", replyPlain)
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
		if string(binPlain) != "binary-data" {
			t.Fatalf("binary plaintext = %s", binPlain)
		}

		// Send reply
		replyCipher, _ := iSend.Encrypt(nil, []byte("ok"))
		conn.Write(ctx, websocket.MessageBinary, mustFrame(FrameText, replyCipher))
	}))
	defer relayServer.Close()

	channel, _ := NewSecureRelayChannel(SecureRelayConfig{
		RelayURL:      "ws" + strings.TrimPrefix(relayServer.URL, "http"),
		RelayJWT:      "relay.jwt.token",
		StaticPrivate: serverStatic,
	})

	gotMessages := make(chan []byte, 1)
	channel.SetOnMessage(func(data []byte) { gotMessages <- data })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = channel.Start(ctx)
	defer channel.Close()

	// Send binary
	if err := channel.SendBinary([]byte("binary-data")); err != nil {
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

func TestSecureRelayChannel_BufferedAmount(t *testing.T) {
	channel, _ := NewSecureRelayChannel(SecureRelayConfig{
		RelayURL:      "ws://test",
		RelayJWT:      "token",
		StaticPrivate: mustGenerateKey(),
	})
	// BufferedAmount always returns 0 for WebSocket transport (no concept of buffering)
	if channel.BufferedAmount() != 0 {
		t.Fatalf("BufferedAmount should return 0")
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

func TestSecureRelayChannel_InvalidFrameInReadLoop(t *testing.T) {
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

		// Send invalid frame (unknown kind)
		conn.Write(ctx, websocket.MessageBinary, []byte{0xff, 0, 0, 0, 0})
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
		// success - onClose should be called when invalid frame causes close
	case <-time.After(2 * time.Second):
		t.Fatal("onClose was not called after invalid frame")
	}
}

// Helper functions for tests

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