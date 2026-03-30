package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/hub"
	"opencloudshare/server/internal/session"
)

type createSessionRequest struct {
	ShareURL string `json:"share_url" binding:"required"`
	TTL      string `json:"ttl"`
}

type createSessionResponse struct {
	Code      string `json:"code"`
	ExpiresAt string `json:"expires_at"`
}

func CreateSession(sessions *session.Manager, h *hub.Hub, authToken string) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := extractToken(c.GetHeader("Authorization"))
		if token != authToken {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		// In single-user mode the auth token is also the agent's connection key in the hub.
		if !h.AgentConnected(token) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "agent not connected — connect WebSocket first"})
			return
		}

		var req createSessionRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		ttl := 24 * time.Hour
		if req.TTL != "" {
			var err error
			ttl, err = time.ParseDuration(req.TTL)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid ttl: use Go duration format e.g. 24h"})
				return
			}
		}

		s, err := sessions.Create(token, req.ShareURL, ttl)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
			return
		}

		c.JSON(http.StatusCreated, createSessionResponse{
			Code:      s.ID,
			ExpiresAt: s.ExpiresAt.Format(time.RFC3339),
		})
	}
}

func extractToken(authHeader string) string {
	return strings.TrimPrefix(authHeader, "Bearer ")
}
