package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() { m.Register(AddAgentEndpointStatus, nil) }

// AddAgentEndpointStatus adds the durable endpoint-escalation status fields to
// `agents` (M4 closeout batch 1, finding 3).
//
// A `report_endpoint` with status "close_failed" means the agent exhausted its
// on-demand mapping-deletion retries, so an owned mapping may still be live on
// the router. Before this field control accepted that report and dropped it, so
// the "visible escalation" had no production consumer. `endpoint_status`
// persists the escalation (cleared by a later report) and `endpoint_status_at`
// records when it was raised; both are optional, so absent/empty means "no
// outstanding escalation", never "available".
//
// Forward-only and idempotent: fields are added only when missing, matching the
// AddAgentsRelaySTUN pattern. Existing rows read the empty default.
func AddAgentEndpointStatus(app core.App) error {
	agentsCol, err := app.FindCollectionByNameOrId("agents")
	if err != nil {
		return err
	}

	newFields := []core.Field{
		&core.TextField{Name: "endpoint_status", Max: 32},
		&core.DateField{Name: "endpoint_status_at"},
	}

	dirty := false
	for _, field := range newFields {
		if agentsCol.Fields.GetByName(field.GetName()) != nil {
			continue
		}
		agentsCol.Fields.Add(field)
		dirty = true
	}
	if !dirty {
		return nil
	}
	if err := app.Save(agentsCol); err != nil {
		return fmt.Errorf("save agents: %w", err)
	}
	return nil
}
