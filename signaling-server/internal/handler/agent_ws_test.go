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
	"opencloudshare/server/internal/db"
	"opencloudshare/server/internal/hub"
)

func TestAgentWS_HelloFlow(t *testing.T) {
	tmpDir := t.TempDir()
	database, err := db.Open(filepath.Join(tmpDir, "test.db"))
	require.NoError(t, err)
	defer database.Close()

	// Create API key
	keyRepo := db.NewAPIKeyRepo(database)
	hash, _ := bcrypt.GenerateFromPassword([]byte("ak_test.agentsecret"), bcrypt.DefaultCost)
	keyRepo.Create("ak_test", string(hash))

	sessionRepo := db.NewSessionRepo(database)
	h := hub.New()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/ws/agent", AgentWS(h, keyRepo, sessionRepo))

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
	router.GET("/ws/agent", AgentWS(h, keyRepo, sessionRepo))

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
	hash1, _ := bcrypt.GenerateFromPassword([]byte("ak_alice.alicesecret"), bcrypt.DefaultCost)
	keyRepo.Create("ak_alice", string(hash1))
	hash2, _ := bcrypt.GenerateFromPassword([]byte("ak_mallory.mallorysecret"), bcrypt.DefaultCost)
	keyRepo.Create("ak_mallory", string(hash2))

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
	router.GET("/ws/agent", AgentWS(h, keyRepo, sessionRepo))

	server := httptest.NewServer(router)
	defer server.Close()

	ctx := context.Background()

	// Mallory tries to claim "CUSTOM01"
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws/agent?api_key=ak_mallory.mallorysecret", nil)
	require.NoError(t, err)

	// Send hello
	conn.Write(ctx, websocket.MessageText, []byte(`{"type":"hello","version":"1.0","agent_id":"mallory-agent"}`))
	conn.Read(ctx) // welcome

	// Try to register same code
	conn.Write(ctx, websocket.MessageText, []byte(`{"type":"register_share","share_url":"ocs://evil.com","code":"CUSTOM01"}`))
	_, data, _ := conn.Read(ctx)

	assert.Contains(t, string(data), "error")
	assert.Contains(t, string(data), "code already in use")
	conn.CloseNow()
}
