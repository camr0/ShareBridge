package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
	"golang.org/x/crypto/bcrypt"
)

// APIKeyAuth extracts and validates API key from query parameter using PocketBase.
// The API key format is: <record_id>.<secret>
// Returns 401 if the key is missing, invalid, inactive, or the bcrypt comparison fails.
func APIKeyAuth(app core.App) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiKeyFull := r.URL.Query().Get("api_key")
			if apiKeyFull == "" {
				http.Error(w, `{"error":"api_key required"}`, http.StatusUnauthorized)
				return
			}

			// Parse <record_id>.<secret> format
			recordID, secret, ok := splitAPIKey(apiKeyFull)
			if !ok {
				http.Error(w, `{"error":"invalid api_key format"}`, http.StatusUnauthorized)
				return
			}

			// Look up the api_keys record by record_id
			record, err := app.FindRecordById("api_keys", recordID)
			if err != nil {
				http.Error(w, `{"error":"invalid api_key"}`, http.StatusUnauthorized)
				return
			}

			// Check is_active
			if !record.GetBool("is_active") {
				http.Error(w, `{"error":"api_key inactive"}`, http.StatusUnauthorized)
				return
			}

			// bcrypt compare secret against stored key_hash
			storedHash := record.GetString("key_hash")
			if err := bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(secret)); err != nil {
				http.Error(w, `{"error":"invalid api_key"}`, http.StatusUnauthorized)
				return
			}

			// Update last_used_at timestamp
			record.Set("last_used_at", types.NowDateTime())
			if err := app.Save(record); err != nil {
				// Log but don't fail the request - the key is valid
				// In production, you might want to log this error
				_ = err
			}

			// Get account_id from the relation field
			accountID := ""
			if relRecord := record.ExpandedOne("account_id"); relRecord != nil {
				accountID = relRecord.Id
			} else {
				// Fallback: get the raw value if expansion didn't work
				accountID = record.GetString("account_id")
			}

			// Store api_key_id and account_id in request context
			ctx := r.Context()
			ctx = withAPIKeyID(ctx, record.Id)
			ctx = withAccountID(ctx, accountID)

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// splitAPIKey parses the API key format <record_id>.<secret>
// Returns the recordID, secret, and true if parsing succeeds.
func splitAPIKey(fullKey string) (recordID, secret string, ok bool) {
	dotIdx := strings.LastIndex(fullKey, ".")
	if dotIdx == -1 || dotIdx == len(fullKey)-1 {
		return "", "", false
	}
	return fullKey[:dotIdx], fullKey[dotIdx+1:], true
}

// contextKey is a private type for context keys to avoid collisions
type contextKey string

const (
	apiKeyIDContextKey  contextKey = "api_key_id"
	accountIDContextKey contextKey = "account_id"
)

// withAPIKeyID returns a new context with the API key ID.
func withAPIKeyID(ctx context.Context, apiKeyID string) context.Context {
	return context.WithValue(ctx, apiKeyIDContextKey, apiKeyID)
}

// withAccountID returns a new context with the account ID.
func withAccountID(ctx context.Context, accountID string) context.Context {
	return context.WithValue(ctx, accountIDContextKey, accountID)
}

// GetAPIKeyID retrieves the API key ID from the request context.
// Returns empty string if not found.
func GetAPIKeyID(ctx context.Context) string {
	if id, ok := ctx.Value(apiKeyIDContextKey).(string); ok {
		return id
	}
	return ""
}

// GetAccountID retrieves the account ID from the request context.
// Returns empty string if not found.
func GetAccountID(ctx context.Context) string {
	if id, ok := ctx.Value(accountIDContextKey).(string); ok {
		return id
	}
	return ""
}
