package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"sharebridge/server/internal/db"
)

// APIKeyAuth extracts and validates API key from query parameter.
func APIKeyAuth(keyRepo *db.APIKeyRepo) gin.HandlerFunc {
	return func(c *gin.Context) {
		apiKey := c.Query("api_key")
		if apiKey == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "api_key required"})
			c.Abort()
			return
		}

		key, err := keyRepo.Validate(apiKey)
		if err != nil || key == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid api_key"})
			c.Abort()
			return
		}

		// Store key ID in context for handlers
		c.Set("api_key_id", key.ID)
		c.Next()
	}
}

// AdminAuth validates admin token from Authorization header.
func AdminAuth(adminToken string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if adminToken == "" {
			c.JSON(http.StatusForbidden, gin.H{"error": "admin endpoints disabled"})
			c.Abort()
			return
		}

		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "authorization required"})
			c.Abort()
			return
		}

		token := strings.TrimPrefix(auth, "Bearer ")
		if token != adminToken {
			c.JSON(http.StatusForbidden, gin.H{"error": "invalid admin token"})
			c.Abort()
			return
		}

		c.Next()
	}
}
