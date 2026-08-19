package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(AddRelayOnly, nil)
}

func AddRelayOnly(app core.App) error {
	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	if err != nil {
		return err
	}

	if sessionsCol.Fields.GetByName("relay_only") != nil {
		return nil
	}

	sessionsCol.Fields.Add(&core.BoolField{
		Name:     "relay_only",
		Required: false,
	})

	return app.Save(sessionsCol)
}
