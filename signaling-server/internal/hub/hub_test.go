// signaling-server/internal/hub/hub_test.go
package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestServer creates a test WebSocket server that accepts connections
func setupTestServer(t *testing.T) (*httptest.Server, chan *websocket.Conn) {
	connChan := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		require.NoError(t, err)
		connChan <- conn
	}))
	return server, connChan
}

// dialTestClient connects to the test server and returns the client connection
func dialTestClient(t *testing.T, server *httptest.Server) *websocket.Conn {
	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, server.URL, nil)
	require.NoError(t, err)
	return conn
}

func newTestWebSocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()

	server, connChan := setupTestServer(t)
	t.Cleanup(server.Close)

	clientConn := dialTestClient(t, server)
	serverConn := <-connChan
	return clientConn, serverConn
}

func TestNewHub(t *testing.T) {
	h := New()
	assert.NotNil(t, h)
	assert.NotNil(t, h.agents)
	assert.NotNil(t, h.codes)
	assert.NotNil(t, h.pairs)
}

func TestRegisterAndUnregisterAgent(t *testing.T) {
	server, connChan := setupTestServer(t)
	defer server.Close()

	h := New()
	apiKey := "test-api-key-123"

	// Initially not connected
	assert.False(t, h.AgentConnected(apiKey))

	// Dial and get server-side conn
	_ = dialTestClient(t, server)
	agentConn := <-connChan

	// Register agent
	h.RegisterAgent(apiKey, agentConn)
	assert.True(t, h.AgentConnected(apiKey))

	// Unregister agent
	h.UnregisterAgent(apiKey)
	assert.False(t, h.AgentConnected(apiKey))

	// Cleanup
	_ = agentConn.Close(websocket.StatusNormalClosure, "test complete")
}

func TestRegisterCodeAndGetAgentConn(t *testing.T) {
	server, connChan := setupTestServer(t)
	defer server.Close()

	h := New()
	apiKey := "test-api-key-456"
	code := "ABC123"

	// Register code before agent is connected - should return nil
	h.RegisterCode(code, apiKey)

	// GetAgentConn should fail because agent not connected
	conn, ok := h.GetAgentConn(code)
	assert.False(t, ok)
	assert.Nil(t, conn)

	// Now connect the agent
	_ = dialTestClient(t, server)
	agentConn := <-connChan
	h.RegisterAgent(apiKey, agentConn)

	// GetAgentConn should now succeed
	conn, ok = h.GetAgentConn(code)
	assert.True(t, ok)
	assert.Equal(t, agentConn, conn)

	// Cleanup
	_ = agentConn.Close(websocket.StatusNormalClosure, "test complete")
}

func TestGetAgentConn_UnknownCode(t *testing.T) {
	h := New()

	// Unknown code should return nil, false
	conn, ok := h.GetAgentConn("UNKNOWN-CODE")
	assert.False(t, ok)
	assert.Nil(t, conn)
}

func TestGetAgentConn_AgentDisconnected(t *testing.T) {
	server, connChan := setupTestServer(t)
	defer server.Close()

	h := New()
	apiKey := "test-api-key-789"
	code := "DEF456"

	// Connect and register agent
	_ = dialTestClient(t, server)
	agentConn := <-connChan
	h.RegisterAgent(apiKey, agentConn)
	h.RegisterCode(code, apiKey)

	// Verify connection works
	conn, ok := h.GetAgentConn(code)
	assert.True(t, ok)
	assert.NotNil(t, conn)

	// Unregister agent (simulates disconnect)
	h.UnregisterAgent(apiKey)

	// GetAgentConn should now fail even though code is still registered
	conn, ok = h.GetAgentConn(code)
	assert.False(t, ok)
	assert.Nil(t, conn)

	// Cleanup
	_ = agentConn.Close(websocket.StatusNormalClosure, "test complete")
}

func TestPairSession(t *testing.T) {
	server, connChan := setupTestServer(t)
	defer server.Close()

	h := New()
	apiKey := "test-api-key-pair"
	code := "PAIR123"

	// Connect agent
	_ = dialTestClient(t, server)
	agentConn := <-connChan
	h.RegisterAgent(apiKey, agentConn)
	h.RegisterCode(code, apiKey)

	// Connect browser
	browserClient := dialTestClient(t, server)
	browserServerConn := <-connChan

	// Pair session
	err := h.PairSession(code, browserServerConn)
	require.NoError(t, err)

	// Unpair
	h.UnpairSession(code, browserServerConn)

	// Cleanup
	_ = agentConn.Close(websocket.StatusNormalClosure, "test complete")
	_ = browserClient.Close(websocket.StatusNormalClosure, "test complete")
	_ = browserServerConn.Close(websocket.StatusNormalClosure, "test complete")
}

func TestPairSession_UnknownCode(t *testing.T) {
	h := New()

	// Try to pair with unregistered code
	err := h.PairSession("UNKNOWN-CODE", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "code not registered")
}

func TestPairSession_AgentNotConnected(t *testing.T) {
	h := New()
	apiKey := "test-api-key-no-agent"
	code := "NOAGENT123"

	// Register code but don't connect agent
	h.RegisterCode(code, apiKey)

	// Try to pair
	err := h.PairSession(code, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "agent not connected")
}

func TestMultipleCodesPerApiKey(t *testing.T) {
	server, connChan := setupTestServer(t)
	defer server.Close()

	h := New()
	apiKey := "test-api-key-multi"
	code1 := "CODE1"
	code2 := "CODE2"
	code3 := "CODE3"

	// Connect agent
	_ = dialTestClient(t, server)
	agentConn := <-connChan
	h.RegisterAgent(apiKey, agentConn)

	// Register multiple codes for same API key
	h.RegisterCode(code1, apiKey)
	h.RegisterCode(code2, apiKey)
	h.RegisterCode(code3, apiKey)

	// All codes should resolve to the same agent connection
	conn1, ok1 := h.GetAgentConn(code1)
	conn2, ok2 := h.GetAgentConn(code2)
	conn3, ok3 := h.GetAgentConn(code3)

	assert.True(t, ok1)
	assert.True(t, ok2)
	assert.True(t, ok3)
	assert.Equal(t, agentConn, conn1)
	assert.Equal(t, agentConn, conn2)
	assert.Equal(t, agentConn, conn3)
	assert.Equal(t, conn1, conn2)
	assert.Equal(t, conn2, conn3)

	// Cleanup
	_ = agentConn.Close(websocket.StatusNormalClosure, "test complete")
}

func TestUnpairSession(t *testing.T) {
	server, connChan := setupTestServer(t)
	defer server.Close()

	h := New()
	apiKey := "test-api-key-unpair"
	code := "UNPAIR123"

	// Setup: connect agent and pair
	_ = dialTestClient(t, server)
	agentConn := <-connChan
	h.RegisterAgent(apiKey, agentConn)
	h.RegisterCode(code, apiKey)

	browserClient := dialTestClient(t, server)
	browserServerConn := <-connChan

	err := h.PairSession(code, browserServerConn)
	require.NoError(t, err)

	// Unpair
	h.UnpairSession(code, browserServerConn)

	// ForwardToAgent should return nil (no error, but no-op)
	ctx := context.Background()
	err = h.ForwardToAgent(ctx, code, map[string]string{"test": "message"})
	assert.NoError(t, err)

	// ForwardToBrowser should also return nil
	err = h.ForwardToBrowser(ctx, code, map[string]string{"test": "message"})
	assert.NoError(t, err)

	// Cleanup
	_ = agentConn.Close(websocket.StatusNormalClosure, "test complete")
	_ = browserClient.Close(websocket.StatusNormalClosure, "test complete")
	_ = browserServerConn.Close(websocket.StatusNormalClosure, "test complete")
}

func TestUnpairSession_ReplacementPairSurvivesOldCleanup(t *testing.T) {
	h := New()
	apiKey := "test-api-key-replacement"
	code := "REPLACE123"

	agentConn := new(websocket.Conn)
	originalBrowserConn := new(websocket.Conn)
	replacementBrowserConn := new(websocket.Conn)

	h.RegisterAgent(apiKey, agentConn)
	h.RegisterCode(code, apiKey)

	require.NoError(t, h.PairSession(code, originalBrowserConn))
	require.NoError(t, h.PairSession(code, replacementBrowserConn))

	h.UnpairSession(code, originalBrowserConn)

	h.mu.RLock()
	sessionPair := h.pairs[code]
	h.mu.RUnlock()

	require.NotNil(t, sessionPair)
	assert.Equal(t, replacementBrowserConn, sessionPair.browserConn)
}

func TestHubUnregisterCodeClosesPairedBrowser(t *testing.T) {
	h := New()
	ctx := context.Background()
	agentConn, agentServer := newTestWebSocketPair(t)
	browserConn, browserServer := newTestWebSocketPair(t)
	defer agentConn.CloseNow()
	defer agentServer.CloseNow()
	defer browserConn.CloseNow()
	defer browserServer.CloseNow()

	h.RegisterAgent("api-key-1", agentConn)
	h.RegisterCode("IMMICHKEY1", "api-key-1")
	require.NoError(t, h.PairSession("IMMICHKEY1", browserConn))

	start := time.Now()
	h.UnregisterCode(ctx, "IMMICHKEY1", "api-key-1", "share has been removed")
	require.Less(t, time.Since(start), 500*time.Millisecond)

	_, ok := h.GetAgentConn("IMMICHKEY1")
	require.False(t, ok)

	_, raw, err := browserServer.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(raw), "share has been removed")
}

func TestSendToAgent(t *testing.T) {
	server, connChan := setupTestServer(t)
	defer server.Close()

	h := New()
	apiKey := "test-api-key-send"

	// Connect agent
	_ = dialTestClient(t, server)
	agentConn := <-connChan
	h.RegisterAgent(apiKey, agentConn)

	// Send message to agent
	ctx := context.Background()
	msg := map[string]string{"type": "test", "data": "hello"}
	err := h.SendToAgent(ctx, apiKey, msg)
	require.NoError(t, err)

	// Cleanup
	_ = agentConn.Close(websocket.StatusNormalClosure, "test complete")
}

func TestSendToAgent_NotConnected(t *testing.T) {
	h := New()
	apiKey := "test-api-key-not-connected"

	// Try to send to unregistered agent
	ctx := context.Background()
	msg := map[string]string{"type": "test"}
	err := h.SendToAgent(ctx, apiKey, msg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "agent not connected")
}
