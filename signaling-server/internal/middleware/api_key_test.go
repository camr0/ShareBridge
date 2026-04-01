package middleware

import (
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	"opencloudshare/server/internal/db"
)

func TestAPIKeyAuth_ValidKey(t *testing.T) {
	tmpDir := t.TempDir()
	database, err := db.Open(filepath.Join(tmpDir, "test.db"))
	require.NoError(t, err)
	defer database.Close()

	repo := db.NewAPIKeyRepo(database)
	hash, _ := bcrypt.GenerateFromPassword([]byte("ak_live_abc123.secret456"), bcrypt.DefaultCost)
	repo.Create("ak_live_abc123", string(hash))

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(APIKeyAuth(repo))
	router.GET("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"api_key_id": c.GetString("api_key_id")})
	})

	req := httptest.NewRequest("GET", "/test?api_key=ak_live_abc123.secret456", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, 200, w.Code)
	assert.Contains(t, w.Body.String(), "ak_live_abc123")
}

func TestAPIKeyAuth_InvalidKey(t *testing.T) {
	tmpDir := t.TempDir()
	database, err := db.Open(filepath.Join(tmpDir, "test.db"))
	require.NoError(t, err)
	defer database.Close()

	repo := db.NewAPIKeyRepo(database)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(APIKeyAuth(repo))
	router.GET("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/test?api_key=invalid", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, 401, w.Code)
}

func TestAdminAuth_ValidToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(AdminAuth("secret-admin-token"))
	router.GET("/admin", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/admin", nil)
	req.Header.Set("Authorization", "Bearer secret-admin-token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, 200, w.Code)
}

func TestAdminAuth_InvalidToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(AdminAuth("secret-admin-token"))
	router.GET("/admin", func(c *gin.Context) {
		c.JSON(200, gin.H{"ok": true})
	})

	req := httptest.NewRequest("GET", "/admin", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	assert.Equal(t, 403, w.Code)
}
