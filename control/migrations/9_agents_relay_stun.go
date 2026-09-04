package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() { m.Register(AddAgentsRelaySTUN, nil) }

// Agent-level direct-route diagnostic values (§12). These describe the last
// gateway observation for audit/UI only: route and transport eligibility are
// live predicates recomputed per request, so no field here — and deliberately
// no durable relay-online boolean — may be treated as routable truth.
const (
	agentDirectStatusUnknown       = "unknown"
	agentDirectStatusEligible      = "eligible"
	agentDirectStatusRelayFallback = "relay_fallback"
)

// AddAgentsRelaySTUN adds the §12 diagnostic fields to `agents` and a partial
// unique index so nonzero relay_port assignments never collide. Everything is
// optional: absent/zero values mean "not observed yet", never "available".
func AddAgentsRelaySTUN(app core.App) error {
	agentsCol, err := app.FindCollectionByNameOrId("agents")
	if err != nil {
		return err
	}

	newFields := []core.Field{
		&core.NumberField{Name: "relay_port", OnlyInt: true},
		&core.NumberField{Name: "relay_generation", OnlyInt: true},
		&core.DateField{Name: "relay_last_seen_at"},
		&core.TextField{Name: "stun_observed_ip"},
		&core.DateField{Name: "stun_observed_at"},
		&core.SelectField{
			Name:      "direct_status",
			MaxSelect: 1,
			Values: []string{
				agentDirectStatusUnknown,
				agentDirectStatusEligible,
				agentDirectStatusRelayFallback,
			},
		},
		&core.TextField{Name: "direct_status_reason"},
	}

	dirty := false
	for _, field := range newFields {
		if agentsCol.Fields.GetByName(field.GetName()) != nil {
			continue
		}
		agentsCol.Fields.Add(field)
		dirty = true
	}

	// Partial unique index: only nonzero ports participate, so unassigned
	// agents (relay_port 0) never conflict. The existing api_key_id and
	// namespace unique indexes are untouched.
	const relayPortIndex = "CREATE UNIQUE INDEX `idx_agents_relay_port` ON `{{COLLECTION}}` (`relay_port`) WHERE `relay_port` IS NOT NULL AND `relay_port` != 0"
	if agentsCol.GetIndex("idx_agents_relay_port") == "" {
		agentsCol.Indexes = append(agentsCol.Indexes, relayPortIndex)
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
