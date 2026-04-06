package migrations

import (
	"fmt"
	"strings"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(CreateCollections, nil)
}

// CreateCollections creates the api_keys and sessions collections.
// Exported so tests can call it directly without relying on m.Register.
func CreateCollections(app core.App) error {
	// Check if collections already exist (idempotent)
	if _, err := app.FindCollectionByNameOrId("api_keys"); err == nil {
		// api_keys collection already exists, skip
		return nil
	}

	// Find the users collection to get its ID for the relation
	usersCol, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return fmt.Errorf("users collection not found: %w", err)
	}

	// --- api_keys collection ---
	apiKeysCol := core.NewBaseCollection("api_keys")
	apiKeysCol.Fields.Add(
		&core.RelationField{
			Name:          "account_id",
			CollectionId:  usersCol.Id,
			Required:      true,
			CascadeDelete: true,
			MaxSelect:     1,
		},
		&core.TextField{
			Name:     "key_hash",
			Required: false, // set after record ID is known; never exposed via API
		},
		&core.TextField{
			Name: "label",
		},
		&core.DateField{
			Name: "last_used_at",
		},
		&core.BoolField{
			Name: "is_active",
		},
	)

	// API rules: only owning user can read/edit their own keys
	// @request.auth.id refers to the authenticated user's ID
	apiKeysRule := "@request.auth.id != '' && account_id.id = @request.auth.id"
	apiKeysCol.ListRule = &apiKeysRule
	apiKeysCol.ViewRule = &apiKeysRule
	apiKeysCol.CreateRule = &apiKeysRule
	apiKeysCol.UpdateRule = &apiKeysRule
	apiKeysCol.DeleteRule = &apiKeysRule

	if err := app.Save(apiKeysCol); err != nil {
		return err
	}

	// Reload to get the api_keys collection with its ID
	apiKeysCol, err = app.FindCollectionByNameOrId("api_keys")
	if err != nil {
		return fmt.Errorf("failed to reload api_keys collection: %w", err)
	}

	// --- sessions collection ---
	sessionsCol := core.NewBaseCollection("sessions")
	sessionsCol.Fields.Add(
		&core.TextField{
			Name:     "code",
			Required: true,
		},
		&core.RelationField{
			Name:          "api_key_id",
			CollectionId:  apiKeysCol.Id,
			Required:      true,
			CascadeDelete: true,
			MaxSelect:     1,
		},
		&core.TextField{
			Name: "agent_id",
		},
		&core.DateField{
			Name: "expires_at",
		},
		&core.AutodateField{
			Name:     "updated",
			OnUpdate: true,
		},
	)
	sessionsCol.Indexes = []string{
		"CREATE UNIQUE INDEX `idx_sessions_code` ON `{{COLLECTION}}` (`code`)",
	}

	// Sessions are internal server state and must not be exposed through the
	// PocketBase collection REST API. Leaving the rules nil keeps access limited
	// to server-side Go code and PocketBase superusers only.
	sessionsCol.ListRule = nil
	sessionsCol.ViewRule = nil
	sessionsCol.CreateRule = nil
	sessionsCol.UpdateRule = nil
	sessionsCol.DeleteRule = nil
	if err := app.Save(sessionsCol); err != nil {
		// Check if it's a "collection already exists" error
		if strings.Contains(err.Error(), "already exists") {
			return nil
		}
		return err
	}
	return nil
}
