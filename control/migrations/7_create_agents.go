package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() { m.Register(CreateAgents, nil) }

func CreateAgents(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("agents"); err == nil {
		return nil
	}

	apiKeysCol, err := app.FindCollectionByNameOrId("api_keys")
	if err != nil {
		return fmt.Errorf("api_keys not found: %w", err)
	}

	agentsCol := core.NewBaseCollection("agents")
	agentsCol.Fields.Add(
		&core.RelationField{Name: "api_key_id", CollectionId: apiKeysCol.Id, Required: true, CascadeDelete: true, MaxSelect: 1},
		&core.TextField{Name: "namespace", Required: true},
		&core.TextField{Name: "endpoint_ip"},
		&core.NumberField{Name: "endpoint_port"},
		&core.TextField{Name: "cert_status"},
		&core.TextField{Name: "cert_fingerprint"},
		&core.DateField{Name: "cert_expires_at"},
		&core.DateField{Name: "last_report_at"},
	)
	agentsCol.Indexes = []string{
		"CREATE UNIQUE INDEX `idx_agents_api_key_id` ON `{{COLLECTION}}` (`api_key_id`)",
		"CREATE UNIQUE INDEX `idx_agents_namespace` ON `{{COLLECTION}}` (`namespace`)",
	}
	if err := app.Save(agentsCol); err != nil {
		return fmt.Errorf("save agents: %w", err)
	}

	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	if err != nil {
		return fmt.Errorf("sessions not found: %w", err)
	}
	sessionsCol.Fields.Add(
		&core.TextField{Name: "origin"},
		&core.BoolField{Name: "is_active"},
	)
	sessionsCol.Indexes = append(sessionsCol.Indexes,
		"CREATE UNIQUE INDEX `idx_sessions_origin` ON `{{COLLECTION}}` (`origin`) WHERE `origin` IS NOT NULL AND `origin` != ''",
	)
	if err := app.Save(sessionsCol); err != nil {
		return fmt.Errorf("save sessions fields: %w", err)
	}

	// Backfill: PocketBase BoolField columns are `DEFAULT FALSE NOT NULL`, so
	// pre-existing rows read false. Mark them active so v1 sessions survive.
	existing, err := app.FindAllRecords("sessions")
	if err != nil {
		return err
	}
	for _, rec := range existing {
		rec.Set("is_active", true)
		if err := app.Save(rec); err != nil {
			return fmt.Errorf("backfill session %s: %w", rec.Id, err)
		}
	}
	return nil
}
