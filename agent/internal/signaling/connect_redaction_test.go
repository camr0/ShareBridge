package signaling

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// TestConnectDialErrorRedactsAPIKey pins the credential-logging constraint on
// the reconnect path: the signaling URL embeds the API key as a query
// parameter, and net/http's *url.Error renders the full URL. The error
// Connect returns — and therefore the daemon's rate-limited retry log line —
// must never contain the key.
func TestConnectDialErrorRedactsAPIKey(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve refused port: %v", err)
	}
	addr := listener.Addr().String()
	listener.Close()

	const apiKey = "super-secret-api-key"
	client := New("ws://"+addr, apiKey, "test-agent")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = client.Connect(ctx)
	if err == nil {
		t.Fatal("expected the dial to a closed port to fail")
	}
	if strings.Contains(err.Error(), apiKey) {
		t.Fatalf("dial error leaked the API key: %v", err)
	}
	if !strings.Contains(err.Error(), "dial signaling server") {
		t.Fatalf("dial error lost its context: %v", err)
	}
}
