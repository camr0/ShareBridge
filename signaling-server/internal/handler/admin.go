package handler

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"opencloudshare/server/internal/db"
)

type CreateAPIKeyResponse struct {
	ID      string `json:"id"`
	FullKey string `json:"full_key"`
}

type APIKeyListItem struct {
	ID         string     `json:"id"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	IsActive   bool       `json:"is_active"`
}

// CreateAPIKey generates a new API key and returns its ID and full key.
// The full_key is only shown once at creation time.
func CreateAPIKey(keyRepo *db.APIKeyRepo) gin.HandlerFunc {
	return func(c *gin.Context) {
		keyID := db.GenerateKeyID()
		secret := db.GenerateSecret()
		fullKey := keyID + "." + secret

		// Hash the full key for storage
		hash, err := bcrypt.GenerateFromPassword([]byte(fullKey), bcrypt.DefaultCost)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate key"})
			return
		}

		// Store the key hash
		if err := keyRepo.Create(keyID, string(hash)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to store key"})
			return
		}

		c.JSON(http.StatusCreated, CreateAPIKeyResponse{
			ID:      keyID,
			FullKey: fullKey,
		})
	}
}

// ListAPIKeys lists all API keys (without hashes).
func ListAPIKeys(keyRepo *db.APIKeyRepo) gin.HandlerFunc {
	return func(c *gin.Context) {
		keys, err := keyRepo.List()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list keys"})
			return
		}

		// Transform to response format (exclude key_hash)
		items := make([]APIKeyListItem, len(keys))
		for i, k := range keys {
			items[i] = APIKeyListItem{
				ID:         k.ID,
				CreatedAt:  k.CreatedAt,
				LastUsedAt: k.LastUsedAt,
				IsActive:   k.IsActive,
			}
		}

		c.JSON(http.StatusOK, gin.H{"keys": items})
	}
}

// RevokeAPIKey marks an API key as inactive.
func RevokeAPIKey(keyRepo *db.APIKeyRepo) gin.HandlerFunc {
	return func(c *gin.Context) {
		keyID := c.Param("id")
		if keyID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "key id required"})
			return
		}

		// Validate key ID format (should start with ak_)
		if !strings.HasPrefix(keyID, "ak_") {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid key id format"})
			return
		}

		found, err := keyRepo.Revoke(keyID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to revoke key"})
			return
		}
		if !found {
			c.JSON(http.StatusNotFound, gin.H{"error": "key not found"})
			return
		}

		c.JSON(http.StatusOK, gin.H{"message": "key revoked"})
	}
}
