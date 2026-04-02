package handler

import (
	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/config"
	"opencloudshare/server/internal/db"
	"opencloudshare/server/internal/hub"
)

func RegisterRoutes(r *gin.Engine, h *hub.Hub, cfg *config.Config, apiKeyRepo *db.APIKeyRepo, sessionRepo *db.SessionRepo) {

	// Public session lookup endpoint
	r.GET("/sessions/:code", GetSessionInfo(sessionRepo, h))

	// WebSocket endpoints
	r.GET("/ws/agent", AgentWS(h, apiKeyRepo, sessionRepo, cfg))
	r.GET("/ws/client", BrowserWS(h, sessionRepo, cfg))

	// Admin API routes (protected by auth token)
	admin := r.Group("/admin/api")
	admin.Use(AdminAuthMiddleware(cfg.AuthToken))
	{
		admin.POST("/keys", CreateAPIKey(apiKeyRepo))
		admin.GET("/keys", ListAPIKeys(apiKeyRepo))
		admin.DELETE("/keys/:id", RevokeAPIKey(apiKeyRepo))
	}

	// Static web files
	r.StaticFile("/", "./web/index.html")
	r.StaticFile("/app.js", "./web/app.js")
}

// AdminAuthMiddleware validates the admin auth token.
func AdminAuthMiddleware(authToken string) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.GetHeader("Authorization")
		if token == "" || token != "Bearer "+authToken {
			c.JSON(401, gin.H{"error": "unauthorized"})
			c.Abort()
			return
		}
		c.Next()
	}
}
