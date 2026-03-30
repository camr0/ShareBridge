package handler

import (
	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/config"
	"opencloudshare/server/internal/hub"
	"opencloudshare/server/internal/session"
)

func RegisterRoutes(r *gin.Engine, sessions *session.Manager, h *hub.Hub, cfg *config.Config) {
	// routes added in subsequent tasks
}
