package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"opencloudshare/server/internal/db"
)

type SessionInfoResponse struct {
	Code      string     `json:"code"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	IsActive  bool       `json:"is_active"`
}

// GetSessionInfo returns session info by code.
// Does NOT expose share_url for privacy.
func GetSessionInfo(sessionRepo *db.SessionRepo) gin.HandlerFunc {
	return func(c *gin.Context) {
		code := c.Param("code")
		if code == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "code required"})
			return
		}

		session, err := sessionRepo.GetByCode(code)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to lookup session"})
			return
		}
		if session == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "session not found"})
			return
		}

		// Determine if session is active
		isActive := true
		if session.ExpiresAt != nil && session.ExpiresAt.Before(time.Now()) {
			isActive = false
		}

		c.JSON(http.StatusOK, SessionInfoResponse{
			Code:      session.Code,
			ExpiresAt: session.ExpiresAt,
			IsActive:  isActive,
		})
	}
}
