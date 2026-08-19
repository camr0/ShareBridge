package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(AddAPIKeyTimestamps, nil)
}

// AddAPIKeyTimestamps backfills the api_keys collection schema for data dirs
// created before the created/updated system fields were added.
func AddAPIKeyTimestamps(app core.App) error {
	apiKeysCol, err := app.FindCollectionByNameOrId("api_keys")
	if err != nil {
		return nil
	}

	changed := false

	if apiKeysCol.Fields.GetByName("created") == nil {
		apiKeysCol.Fields.Add(&core.AutodateField{
			Name:     "created",
			System:   true,
			OnCreate: true,
		})
		changed = true
	}

	if apiKeysCol.Fields.GetByName("updated") == nil {
		apiKeysCol.Fields.Add(&core.AutodateField{
			Name:     "updated",
			System:   true,
			OnCreate: true,
			OnUpdate: true,
		})
		changed = true
	}

	if !changed {
		return nil
	}

	return app.Save(apiKeysCol)
}
