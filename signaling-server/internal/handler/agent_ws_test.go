package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/db"
	"sharebridge/server/internal/hub"
)

func TestAgentWS_HelloFlow(t *testing.T) {
	tmpDir := t.TempDir()
	database, err := db.Open(filepath.Join(tmpDir, "test.db"))
	require.NoError(t, err)
	defer database.Close()

	// Create API key
	keyRepo := db.NewAPIKeyRepo(database)
	hash, err := bcrypt.GenerateFromPassword([]byte("ak_test.agentsecret"), bcrypt.DefaultCost)
	require.NoError(t, err)
	err = keyRepo.Create("ak_test", string(hash))
	require.NoError(t, err)

	sessionRepo := db.NewSessionRepo(database)
	h := hub.New()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	cfg := config.Load()
	router.GET("/ws/agent", AgentWS(h, keyRepo, sessionRepo, cfg))

	server := httptest.NewServer(router)
	defer server.Close()

	// Connect with valid API key
	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key=ak_test.agentsecret", nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	// Send hello
	err = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","version":"1.0","agent_id":"test-agent-uuid"}`))
	require.NoError(t, err)

	// Expect welcome
	_, data, err := conn.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"type":"welcome"`)
	assert.Contains(t, string(data), `"ice_servers"`)
}

func TestAgentWS_InvalidAPIKey(t *testing.T) {
	tmpDir := t.TempDir()
	database, err := db.Open(filepath.Join(tmpDir, "test.db"))
	require.NoError(t, err)
	defer database.Close()

	keyRepo := db.NewAPIKeyRepo(database)
	sessionRepo := db.NewSessionRepo(database)
	h := hub.New()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	cfg := config.Load()
	router.GET("/ws/agent", AgentWS(h, keyRepo, sessionRepo, cfg))

	server := httptest.NewServer(router)
	defer server.Close()

	// Connect with invalid API key
	ctx := context.Background()
	_, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key=invalid", nil)

	assert.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAgentWS_CodeOwnership(t *testing.T) {
	tmpDir := t.TempDir()
	database, err := db.Open(filepath.Join(tmpDir, "test.db"))
	require.NoError(t, err)
	defer database.Close()

	// Create two API keys
	keyRepo := db.NewAPIKeyRepo(database)
	hash1, err := bcrypt.GenerateFromPassword([]byte("ak_alice.alicesecret"), bcrypt.DefaultCost)
	require.NoError(t, err)
	err = keyRepo.Create("ak_alice", string(hash1))
	require.NoError(t, err)
	hash2, err := bcrypt.GenerateFromPassword([]byte("ak_mallory.mallorysecret"), bcrypt.DefaultCost)
	require.NoError(t, err)
	err = keyRepo.Create("ak_mallory", string(hash2))
	require.NoError(t, err)

	sessionRepo := db.NewSessionRepo(database)
	h := hub.New()

	// Alice creates session "CUSTOM01"
	sessionRepo.Create(&db.Session{
		Code:     "CUSTOM01",
		APIKeyID: "ak_alice",
		AgentID:  "alice-agent",
		ShareURL: "ocs://alice.com/share",
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	cfg := config.Load()
	router.GET("/ws/agent", AgentWS(h, keyRepo, sessionRepo, cfg))

	server := httptest.NewServer(router)
	defer server.Close()

	ctx := context.Background()

	// Mallory tries to claim "CUSTOM01"
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key=ak_mallory.mallorysecret", nil)
	require.NoError(t, err)

	// Send hello
	err = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","version":"1.0","agent_id":"mallory-agent"}`))
	require.NoError(t, err)
	_, _, err = conn.Read(ctx) // welcome
	require.NoError(t, err)

	// Try to register same code
	err = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","share_url":"ocs://evil.com","code":"CUSTOM01"}`))
	require.NoError(t, err)
	_, data, err := conn.Read(ctx)
	require.NoError(t, err)

	assert.Contains(t, string(data), "error")
	assert.Contains(t, string(data), "code already in use")
	conn.CloseNow()
}
