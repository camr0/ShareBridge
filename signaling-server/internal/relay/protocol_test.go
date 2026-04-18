package relay

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	libp2p "github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/multiformats/go-multiaddr"
)

func newLibp2pHost(t *testing.T) host.Host {
	t.Helper()
	h, err := libp2p.New(libp2p.ListenAddrStrings("/ip4/127.0.0.1/tcp/0"))
	if err != nil {
		t.Fatalf("libp2p.New: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func connect(t *testing.T, dialer, target host.Host) {
	t.Helper()
	dialer.Peerstore().AddAddrs(target.ID(), target.Addrs(), time.Minute)
}

func writeTextFrame(t *testing.T, s network.Stream, payload string) {
	t.Helper()
	if _, err := s.Write([]byte{0x01}); err != nil {
		t.Fatalf("write kind: %v", err)
	}
	if err := binary.Write(s, binary.BigEndian, uint32(len(payload))); err != nil {
		t.Fatalf("write length: %v", err)
	}
	if _, err := s.Write([]byte(payload)); err != nil {
		t.Fatalf("write payload: %v", err)
	}
}

func readTextFrame(t *testing.T, s network.Stream) map[string]any {
	t.Helper()
	kindBuf := make([]byte, 1)
	if _, err := io.ReadFull(s, kindBuf); err != nil {
		t.Fatalf("read kind: %v", err)
	}
	if kindBuf[0] != 0x01 {
		t.Fatalf("expected text frame kind 0x01, got 0x%02x", kindBuf[0])
	}
	var length uint32
	if err := binary.Read(s, binary.BigEndian, &length); err != nil {
		t.Fatalf("read length: %v", err)
	}
	if length > 8*1024*1024 {
		t.Fatalf("frame too large: %d bytes", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(s, buf); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(buf, &resp); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return resp
}

func TestProtocol_rejectsInvalidKind(t *testing.T) {
	relayHost := newLibp2pHost(t)
	issuer := NewIssuer([]byte("test-secret-do-not-use-in-prod-abcd1234"), time.Minute)
	jtis := NewJTIStore(time.Minute)
	defer jtis.Close()
	reg := NewAgentRegistry()
	acl := newCircuitACL(time.Minute)

	h := Handler{Issuer: issuer, JTIs: jtis, Agents: reg, ACL: acl, AuthTTL: time.Minute}
	relayHost.SetStreamHandler(ProtocolID, h.Handle)

	browserHost := newLibp2pHost(t)
	connect(t, browserHost, relayHost)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := browserHost.NewStream(ctx, relayHost.ID(), ProtocolID)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	defer s.Close()

	s.Write([]byte{0x99, 0, 0, 0, 0})
	s.CloseWrite()

	resp := readTextFrame(t, s)
	if resp["type"] != "error" {
		t.Fatal("expected error response for invalid frame kind")
	}
}

func TestProtocol_validJWT_authorizesACL(t *testing.T) {
	relayHost := newLibp2pHost(t)
	agentHost := newLibp2pHost(t)
	browserHost := newLibp2pHost(t)

	issuer := NewIssuer([]byte("test-secret-do-not-use-in-prod-abcd1234"), time.Minute)
	jtis := NewJTIStore(time.Minute)
	defer jtis.Close()
	reg := NewAgentRegistry()
	reg.Register("api-key-xyz", agentHost.ID())
	acl := newCircuitACL(time.Minute)

	h := Handler{
		Issuer:  issuer,
		JTIs:    jtis,
		Agents:  reg,
		ACL:     acl,
		AuthTTL: time.Minute,
		CodeToAPIKey: func(code string) (string, bool) {
			if code == "abc12345" {
				return "api-key-xyz", true
			}
			return "", false
		},
	}
	relayHost.SetStreamHandler(ProtocolID, h.Handle)
	connect(t, browserHost, relayHost)

	tok, err := issuer.Issue(Claims{
		ShareCode:     "abc12345",
		BrowserPeerID: browserHost.ID().String(),
		RelayAllowed:  true,
		DCUtRAllowed:  true,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := browserHost.NewStream(ctx, relayHost.ID(), ProtocolID)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	envelope, _ := json.Marshal(map[string]string{"type": "jwt", "token": tok})
	writeTextFrame(t, s, string(envelope))
	s.CloseWrite()

	resp := readTextFrame(t, s)
	if resp["type"] != "auth_ok" {
		t.Fatalf("expected type:auth_ok, got %v", resp)
	}

	addr, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/9001")
	if !acl.AllowConnect(browserHost.ID(), addr, agentHost.ID()) {
		t.Fatal("ACL should allow browser to agent after successful auth")
	}
}

func TestProtocol_validJWT_keepsAuthStreamOpen(t *testing.T) {
	relayHost := newLibp2pHost(t)
	agentHost := newLibp2pHost(t)
	browserHost := newLibp2pHost(t)

	issuer := NewIssuer([]byte("test-secret-do-not-use-in-prod-abcd1234"), time.Minute)
	jtis := NewJTIStore(time.Minute)
	defer jtis.Close()
	reg := NewAgentRegistry()
	reg.Register("api-key-xyz", agentHost.ID())
	acl := newCircuitACL(time.Minute)

	h := Handler{
		Issuer:  issuer,
		JTIs:    jtis,
		Agents:  reg,
		ACL:     acl,
		AuthTTL: time.Minute,
		CodeToAPIKey: func(code string) (string, bool) {
			if code == "abc12345" {
				return "api-key-xyz", true
			}
			return "", false
		},
	}
	relayHost.SetStreamHandler(ProtocolID, h.Handle)
	connect(t, browserHost, relayHost)

	tok, err := issuer.Issue(Claims{
		ShareCode:     "abc12345",
		BrowserPeerID: browserHost.ID().String(),
		RelayAllowed:  true,
		DCUtRAllowed:  true,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := browserHost.NewStream(ctx, relayHost.ID(), ProtocolID)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	defer s.Close()

	envelope, _ := json.Marshal(map[string]string{"type": "jwt", "token": tok})
	writeTextFrame(t, s, string(envelope))
	s.CloseWrite()

	resp := readTextFrame(t, s)
	if resp["type"] != "auth_ok" {
		t.Fatalf("expected type:auth_ok, got %v", resp)
	}

	if err := s.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	defer s.SetReadDeadline(time.Time{})

	buf := make([]byte, 1)
	_, err = s.Read(buf)
	if err == nil {
		t.Fatal("expected auth stream read to block or timeout, got data")
	}
	if err == io.EOF {
		t.Fatal("expected auth stream to remain open after auth_ok, got EOF")
	}
	if !os.IsTimeout(err) {
		t.Fatalf("expected timeout while auth stream stayed open, got %T: %v", err, err)
	}
}

func TestProtocol_rejectsJTIReplay(t *testing.T) {
	relayHost := newLibp2pHost(t)
	agentHost := newLibp2pHost(t)
	browserHost := newLibp2pHost(t)

	issuer := NewIssuer([]byte("test-secret-do-not-use-in-prod-abcd1234"), time.Minute)
	jtis := NewJTIStore(time.Minute)
	defer jtis.Close()
	reg := NewAgentRegistry()
	reg.Register("k", agentHost.ID())
	acl := newCircuitACL(time.Minute)

	h := Handler{
		Issuer: issuer, JTIs: jtis, Agents: reg, ACL: acl, AuthTTL: time.Minute,
		CodeToAPIKey: func(string) (string, bool) { return "k", true },
	}
	relayHost.SetStreamHandler(ProtocolID, h.Handle)
	connect(t, browserHost, relayHost)

	tok, _ := issuer.Issue(Claims{
		ShareCode:     "abc12345",
		BrowserPeerID: browserHost.ID().String(),
		RelayAllowed:  true,
	})

	dial := func() map[string]any {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		s, err := browserHost.NewStream(ctx, relayHost.ID(), ProtocolID)
		if err != nil {
			t.Fatalf("NewStream: %v", err)
		}
		defer s.Close()
		envelope, _ := json.Marshal(map[string]string{"type": "jwt", "token": tok})
		writeTextFrame(t, s, string(envelope))
		s.CloseWrite()
		return readTextFrame(t, s)
	}

	first := dial()
	if first["type"] != "auth_ok" {
		t.Fatalf("first use should succeed, got %v", first)
	}
	second := dial()
	if second["type"] != "error" {
		t.Fatalf("second use (replay) should fail, got %v", second)
	}
}

func TestProtocol_rejectsRelayDisabledToken(t *testing.T) {
	relayHost := newLibp2pHost(t)
	agentHost := newLibp2pHost(t)
	browserHost := newLibp2pHost(t)

	issuer := NewIssuer([]byte("test-secret-do-not-use-in-prod-abcd1234"), time.Minute)
	jtis := NewJTIStore(time.Minute)
	defer jtis.Close()
	reg := NewAgentRegistry()
	reg.Register("k", agentHost.ID())
	acl := newCircuitACL(time.Minute)

	h := Handler{
		Issuer:  issuer,
		JTIs:    jtis,
		Agents:  reg,
		ACL:     acl,
		AuthTTL: time.Minute,
		CodeToAPIKey: func(string) (string, bool) {
			return "k", true
		},
	}
	relayHost.SetStreamHandler(ProtocolID, h.Handle)
	connect(t, browserHost, relayHost)

	tok, err := issuer.Issue(Claims{
		ShareCode:     "abc12345",
		BrowserPeerID: browserHost.ID().String(),
		RelayAllowed:  false,
		DCUtRAllowed:  true,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := browserHost.NewStream(ctx, relayHost.ID(), ProtocolID)
	if err != nil {
		t.Fatalf("NewStream: %v", err)
	}
	defer s.Close()

	envelope, _ := json.Marshal(map[string]string{"type": "jwt", "token": tok})
	writeTextFrame(t, s, string(envelope))
	s.CloseWrite()

	resp := readTextFrame(t, s)
	if resp["type"] != "error" {
		t.Fatalf("expected error response, got %v", resp)
	}

	addr, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/9001")
	if acl.AllowConnect(browserHost.ID(), addr, agentHost.ID()) {
		t.Fatal("ACL should not authorize relay-disabled token")
	}
}
