package handler

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/pocketbase/dbx"
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
		authRecord := e.Auth
		if authRecord == nil {
			return e.JSON(http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		}

		var req CreateAPIKeyRequest
		if err := json.NewDecoder(e.Request.Body).Decode(&req); err != nil {
			// Body is optional, continue with empty label.
		}

		record, fullKey, createdAt, err := createAPIKeyRecord(app, authRecord.Id, req.Label)
		if err != nil {
			return e.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}

		return e.JSON(http.StatusCreated, CreateAPIKeyResponse{
			ID:        record.Id,
			Key:       fullKey,
			Label:     req.Label,
			CreatedAt: createdAt,
		})
	}
}

// RotateAPIKey creates a replacement key, transfers all sessions from the old
// key to the new key, immediately revokes the old key, and disconnects any
// live agent using it.
func RotateAPIKey(app core.App, sessionHub *hub.Hub) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		authRecord := e.Auth
		if authRecord == nil {
			return e.JSON(http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		}
		accountID := authRecord.Id

		oldKeyID := e.Request.PathValue("id")
		if oldKeyID == "" {
			return e.JSON(http.StatusBadRequest, map[string]string{"error": "key id required"})
		}

		oldKeyRecord, err := app.FindRecordById("api_keys", oldKeyID)
		if err != nil {
			return e.JSON(http.StatusNotFound, map[string]string{"error": "key not found"})
		}
		if !ownsAPIKey(oldKeyRecord, accountID) {
			return e.JSON(http.StatusForbidden, map[string]string{"error": "access denied"})
		}
		if !oldKeyRecord.GetBool("is_active") {
			return e.JSON(http.StatusBadRequest, map[string]string{"error": "key is already revoked"})
		}

		newLabel := oldKeyRecord.GetString("label")

		var newRecord *core.Record
		var newFullKey string
		var createdAt time.Time

		err = app.RunInTransaction(func(txApp core.App) error {
			var createErr error
			newRecord, newFullKey, createdAt, createErr = createAPIKeyRecord(txApp, accountID, newLabel)
			if createErr != nil {
				return createErr
			}

			_, updateErr := txApp.DB().
				NewQuery(`UPDATE sessions SET api_key_id = {:newKeyID} WHERE api_key_id = {:oldKeyID}`).
				Bind(dbx.Params{
					"newKeyID": newRecord.Id,
					"oldKeyID": oldKeyID,
				}).
				Execute()
			if updateErr != nil {
				return updateErr
			}

			oldKeyTxRecord, findErr := txApp.FindRecordById("api_keys", oldKeyID)
			if findErr != nil {
				return findErr
			}
			oldKeyTxRecord.Set("is_active", false)
			if saveErr := txApp.Save(oldKeyTxRecord); saveErr != nil {
				return saveErr
			}

			return nil
		})
		if err != nil {
			return e.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}

		sessionHub.CloseAgent(oldKeyID)

		return e.JSON(http.StatusCreated, CreateAPIKeyResponse{
			ID:        newRecord.Id,
			Key:       newFullKey,
			Label:     newLabel,
			CreatedAt: createdAt,
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
			"account_id.id = {:accountID}",
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

		if !ownsAPIKey(record, accountID) {
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

func createAPIKeyRecord(app core.App, accountID, label string) (*core.Record, string, time.Time, error) {
	col, err := app.FindCollectionByNameOrId("api_keys")
	if err != nil {
		return nil, "", time.Time{}, errors.New("failed to find collection")
	}

	record := core.NewRecord(col)
	record.Set("account_id", accountID)
	record.Set("label", label)
	record.Set("is_active", true)
	if err := app.Save(record); err != nil {
		return nil, "", time.Time{}, errors.New("failed to create key")
	}

	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		_ = app.Delete(record)
		return nil, "", time.Time{}, errors.New("failed to generate secret")
	}
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)

	fullKey := record.Id + "." + secret
	hash, err := bcrypt.GenerateFromPassword([]byte(fullKey), bcrypt.DefaultCost)
	if err != nil {
		_ = app.Delete(record)
		return nil, "", time.Time{}, errors.New("failed to hash key")
	}

	record.Set("key_hash", string(hash))
	if err := app.Save(record); err != nil {
		_ = app.Delete(record)
		return nil, "", time.Time{}, errors.New("failed to save key hash")
	}

	createdAt := time.Now()
	if created := record.GetDateTime("created"); !created.IsZero() {
		createdAt = created.Time()
	}

	return record, fullKey, createdAt, nil
}

func ownsAPIKey(record *core.Record, accountID string) bool {
	keyAccountID := ""
	if relRecord := record.ExpandedOne("account_id"); relRecord != nil {
		keyAccountID = relRecord.Id
	} else {
		keyAccountID = record.GetString("account_id")
	}
	return keyAccountID == accountID
}
