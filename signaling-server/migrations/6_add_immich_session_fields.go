package migrations

import (
	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(AddImmichSessionFields, nil)
}

func AddImmichSessionFields(app core.App) error {
	sessionsCol, err := app.FindCollectionByNameOrId("sessions")
	if err != nil {
		return err
	}

	changed := false
	if sessionsCol.Fields.GetByName("share_type") == nil {
		sessionsCol.Fields.Add(&core.TextField{Name: "share_type", Required: false})
		changed = true
	}
	if sessionsCol.Fields.GetByName("is_password_protected") == nil {
		sessionsCol.Fields.Add(&core.BoolField{Name: "is_password_protected", Required: false})
		changed = true
	}
	if !changed {
		return nil
	}
	return app.Save(sessionsCol)
}
