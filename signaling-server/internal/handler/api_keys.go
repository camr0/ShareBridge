package handler

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"golang.org/x/crypto/bcrypt"
	"sharebridge/server/internal/hub"
)

// CreateAPIKeyRequest represents the request body for creating an API key
type CreateAPIKeyRequest struct {
	Label string `json:"label"`
}

// CreateAPIKeyResponse represents the response for creating an API key
type CreateAPIKeyResponse struct {
	ID        string    `json:"id"`
	Key       string    `json:"key"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
}

// APIKeyListItem represents a single API key in the list response
type APIKeyListItem struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	IsActive   bool       `json:"is_active"`
	CreatedAt  time.Time  `json:"created_at"`
}

// CreateAPIKey creates a new API key for the authenticated user.
// POST /api/keys
func CreateAPIKey(app core.App) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		// Extract account_id from authenticated user context
		authRecord := e.Auth
		if authRecord == nil {
			return e.JSON(http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		}
		accountID := authRecord.Id

		// Parse optional label from request body
		var req CreateAPIKeyRequest
		if err := json.NewDecoder(e.Request.Body).Decode(&req); err != nil {
			// Body is optional, continue with empty label
		}

		// Get the api_keys collection
		col, err := app.FindCollectionByNameOrId("api_keys")
		if err != nil {
			return e.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to find collection"})
		}

		// Create new record to get the ID
		record := core.NewRecord(col)
		record.Set("account_id", accountID)
		record.Set("label", req.Label)
		record.Set("is_active", true)

		// Save to get the record ID
		if err := app.Save(record); err != nil {
			return e.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to create key"})
		}

		// Generate 32-byte random secret (base64url encoded = 43 chars)
		secretBytes := make([]byte, 32)
		if _, err := rand.Read(secretBytes); err != nil {
			return e.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to generate secret"})
		}
		secret := base64.RawURLEncoding.EncodeToString(secretBytes)

		// Full key format: <record_id>.<secret>
		fullKey := record.Id + "." + secret

		// Generate bcrypt hash of the full key for storage
		hash, err := bcrypt.GenerateFromPassword([]byte(fullKey), bcrypt.DefaultCost)
		if err != nil {
			return e.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to hash key"})
		}

		// Update record with the hash
		record.Set("key_hash", string(hash))
		if err := app.Save(record); err != nil {
			return e.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to save key hash"})
		}

		// Return the full key (shown once only)
		return e.JSON(http.StatusCreated, CreateAPIKeyResponse{
			ID:        record.Id,
			Key:       fullKey,
			Label:     req.Label,
			CreatedAt: time.Now(),
		})
	}
}

// ListAPIKeys returns all API keys for the authenticated user.
// GET /api/keys
func ListAPIKeys(app core.App) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		// Extract account_id from authenticated user context
		authRecord := e.Auth
		if authRecord == nil {
			return e.JSON(http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		}
		accountID := authRecord.Id

		// Query all keys for this account, excluding key_hash
		records, err := app.FindRecordsByFilter(
			"api_keys",
			"account_id = {:accountID}",
			"-created", // sort by created desc
			100,        // limit
			0,          // offset
			map[string]any{"accountID": accountID},
		)
		if err != nil {
			return e.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to list keys"})
		}

		// Build response (excluding key_hash)
		keys := make([]APIKeyListItem, 0, len(records))
		for _, record := range records {
			item := APIKeyListItem{
				ID:       record.Id,
				Label:    record.GetString("label"),
				IsActive: record.GetBool("is_active"),
			}

			// Parse created timestamp
			createdAt := record.GetDateTime("created")
			if !createdAt.IsZero() {
				item.CreatedAt = createdAt.Time()
			}

			// Parse last_used_at (optional)
			lastUsedAt := record.GetDateTime("last_used_at")
			if !lastUsedAt.IsZero() {
				t := lastUsedAt.Time()
				item.LastUsedAt = &t
			}

			keys = append(keys, item)
		}

		return e.JSON(http.StatusOK, keys)
	}
}

// RevokeAPIKey marks an API key as inactive and disconnects any connected agent.
// DELETE /api/keys/:id
func RevokeAPIKey(app core.App, h *hub.Hub) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		// Extract account_id from authenticated user context
		authRecord := e.Auth
		if authRecord == nil {
			return e.JSON(http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		}
		accountID := authRecord.Id

		// Get the key ID from URL parameter
		keyID := e.Request.PathValue("id")
		if keyID == "" {
			return e.JSON(http.StatusBadRequest, map[string]string{"error": "key id required"})
		}

		// Find the key record
		record, err := app.FindRecordById("api_keys", keyID)
		if err != nil {
			return e.JSON(http.StatusNotFound, map[string]string{"error": "key not found"})
		}

		// Verify ownership - check account_id matches
		keyAccountID := ""
		if relRecord := record.ExpandedOne("account_id"); relRecord != nil {
			keyAccountID = relRecord.Id
		} else {
			keyAccountID = record.GetString("account_id")
		}

		if keyAccountID != accountID {
			return e.JSON(http.StatusForbidden, map[string]string{"error": "access denied"})
		}

		// Mark key as inactive
		record.Set("is_active", false)
		if err := app.Save(record); err != nil {
			return e.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to revoke key"})
		}

		// Immediately disconnect any agent using this API key
		h.CloseAgent(keyID)

		return e.JSON(http.StatusOK, map[string]string{"message": "key revoked"})
	}
}
