package handler

import (
	"database/sql"

	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/config"
	"opencloudshare/server/internal/db"
	"opencloudshare/server/internal/hub"
	"opencloudshare/server/internal/session"
)

func RegisterRoutes(r *gin.Engine, sessions *session.Manager, h *hub.Hub, cfg *config.Config, database *sql.DB) {
	apiKeyRepo := db.NewAPIKeyRepo(database)
	sessionRepo := db.NewSessionRepo(database)

	r.POST("/api/v1/sessions", CreateSession(sessions, h, cfg.AuthToken))
	r.GET("/ws/agent", AgentWS(h, apiKeyRepo, sessionRepo))
	r.GET("/ws/client", BrowserWS(h, sessionRepo, cfg.STUNURL))
	r.StaticFile("/", "./web/index.html")
	r.StaticFile("/app.js", "./web/app.js")
}
