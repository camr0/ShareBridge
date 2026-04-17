package relay

import (
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

func TestCircuitACL_agentCanAlwaysReserve(t *testing.T) {
	acl := newCircuitACL(time.Minute)
	pid, _ := peer.Decode("12D3KooWGRUacMd4aSXwsNyEYxh3nC1dT3rBjoJ5ycnqZRuiETxF")
	addr, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/9001")
	if !acl.AllowReserve(pid, addr) {
		t.Fatal("agents should always be allowed to reserve")
	}
}

func TestCircuitACL_browserCanConnectAfterAuthorize(t *testing.T) {
	acl := newCircuitACL(time.Minute)
	browserPID, _ := peer.Decode("12D3KooWBrowserAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	agentPID, _ := peer.Decode("12D3KooWGRUacMd4aSXwsNyEYxh3nC1dT3rBjoJ5ycnqZRuiETxF")
	addr, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/9001")

	if acl.AllowConnect(browserPID, addr, agentPID) {
		t.Fatal("should not allow before Authorize")
	}

	acl.Authorize(browserPID, agentPID, "abc12345", "api-key-1", time.Minute)

	if !acl.AllowConnect(browserPID, addr, agentPID) {
		t.Fatal("should allow after Authorize")
	}
}

func TestCircuitACL_wrongTargetRejected(t *testing.T) {
	acl := newCircuitACL(time.Minute)
	browserPID, _ := peer.Decode("12D3KooWBrowserAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	agentPID, _ := peer.Decode("12D3KooWGRUacMd4aSXwsNyEYxh3nC1dT3rBjoJ5ycnqZRuiETxF")
	wrongPID, _ := peer.Decode("12D3KooWWrongAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	addr, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/9001")

	acl.Authorize(browserPID, agentPID, "abc12345", "api-key-1", time.Minute)

	if acl.AllowConnect(browserPID, addr, wrongPID) {
		t.Fatal("should reject connection to wrong agent")
	}
}

func TestCircuitACL_expiredEntryRejected(t *testing.T) {
	acl := newCircuitACL(time.Minute)
	browserPID, _ := peer.Decode("12D3KooWBrowserAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	agentPID, _ := peer.Decode("12D3KooWGRUacMd4aSXwsNyEYxh3nC1dT3rBjoJ5ycnqZRuiETxF")
	addr, _ := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/9001")

	acl.Authorize(browserPID, agentPID, "abc12345", "api-key-1", time.Millisecond)
	time.Sleep(10 * time.Millisecond)

	if acl.AllowConnect(browserPID, addr, agentPID) {
		t.Fatal("should reject expired entry")
	}
}