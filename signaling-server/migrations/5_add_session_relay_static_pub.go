package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(AddSessionRelayStaticPub, nil)
}

func AddSessionRelayStaticPub(app core.App) error {
	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	if err != nil {
		return err
	}

	if sessionsCol.Fields.GetByName("relay_static_pub") != nil {
		return nil
	}

	sessionsCol.Fields.Add(&core.TextField{
		Name:     "relay_static_pub",
		Required: false,
	})

	return app.Save(sessionsCol)
}
