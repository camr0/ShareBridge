package migrations

import (
	"fmt"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() { m.Register(BoundAgentDiagnosticText, nil) }

// BoundAgentDiagnosticText is the T5-m1 forward migration (remediation R4
// item 2): it tightens the two UNBOUNDED §12 diagnostic TextFields that
// AddAgentsRelaySTUN added so the schema maxima match exactly what the writer
// tasks persist. Forward-only (§14): the old migration is never edited.
//
//   - agents.direct_status_reason carries ONLY the closed DirectStatusReason
//     enum (directpredicate.go; longest value "stun_not_public", 15 chars).
//     Max 64 keeps the audit field bounded while leaving headroom for future
//     enum codes (the enum's own extension rule: new codes stay bounded).
//   - agents.stun_observed_ip carries netip.Addr.Unmap().String() of the
//     observed UDP source; the maximal textual form (IPv6) is 45 chars.
//
// No stored value can violate the new bounds: every historical writer was
// already confined to these shapes (the closed enum and the address-string
// derivation), and PocketBase enforces TextField Max at the record-save
// layer, so no data migration or backfill is needed.
func BoundAgentDiagnosticText(app core.App) error {
	agentsCol, err := app.FindCollectionByNameOrId("agents")
	if err != nil {
		return err
	}

	bounds := []struct {
		field string
		max   int
	}{
		{field: "direct_status_reason", max: 64},
		{field: "stun_observed_ip", max: 45},
	}

	dirty := false
	for _, b := range bounds {
		f := agentsCol.Fields.GetByName(b.field)
		if f == nil {
			return fmt.Errorf("agents.%s missing: %s must run first", b.field, "AddAgentsRelaySTUN")
		}
		tf, ok := f.(*core.TextField)
		if !ok {
			return fmt.Errorf("agents.%s is %T, want *core.TextField", b.field, f)
		}
		if tf.Max != b.max {
			tf.Max = b.max
			dirty = true
		}
	}

	if !dirty {
		return nil
	}
	if err := app.Save(agentsCol); err != nil {
		return fmt.Errorf("save agents: %w", err)
	}
	return nil
}
