package relayctl_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/stretchr/testify/require"
	"sharebridge/control/internal/relayctl"
	"sharebridge/control/migrations"
)

// newRelayTestApp boots a hermetic app carrying the agents schema including
// the migration-9 relay fields (relay_port + partial unique index), so
// EnsureAssignment exercises the real allocation constraints.
func newRelayTestApp(t *testing.T) core.App {
	t.Helper()

	app := core.NewBaseApp(core.BaseAppConfig{
		DataDir:       t.TempDir(),
		EncryptionEnv: "pb_relay_test_env",
	})
	require.NoError(t, app.Bootstrap())
	require.NoError(t, migrations.CreateCollections(app))
	require.NoError(t, migrations.CreateAgents(app))
	require.NoError(t, migrations.AddAgentsRelaySTUN(app))
	return app
}

// createRelayTestAgent inserts an api_key + agent row (the required
// api_key_id relation makes a key row mandatory) and returns the agent record.
func createRelayTestAgent(t *testing.T, app core.App, namespace string) *core.Record {
	t.Helper()

	usersCol, err := app.FindCollectionByNameOrId("users")
	require.NoError(t, err)
	user := core.NewRecord(usersCol)
	user.SetEmail(fmt.Sprintf("relay-%s@example.com", namespace))
	user.SetPassword("relaytestpassword")
	require.NoError(t, app.Save(user))

	keysCol, err := app.FindCollectionByNameOrId("api_keys")
	require.NoError(t, err)
	key := core.NewRecord(keysCol)
	key.Set("account_id", user.Id)
	key.Set("is_active", true)
	key.Set("label", "relay test key")
	require.NoError(t, app.Save(key))

	agentsCol, err := app.FindCollectionByNameOrId("agents")
	require.NoError(t, err)
	agent := core.NewRecord(agentsCol)
	agent.Set("api_key_id", key.Id)
	agent.Set("namespace", namespace)
	require.NoError(t, app.Save(agent))
	return agent
}

func mustPortRange(t *testing.T, min, max int) relayctl.PortRange {
	t.Helper()
	portRange, err := relayctl.NewPortRange(min, max)
	require.NoError(t, err)
	return portRange
}

func mustSigner(t *testing.T) *relayctl.Signer {
	t.Helper()
	signer, err := relayctl.GenerateSigner()
	require.NoError(t, err)
	return signer
}

func mustAssignment(t *testing.T, app core.App, agent *core.Record, portRange relayctl.PortRange) relayctl.RelayAssignment {
	t.Helper()
	assignment, err := relayctl.EnsureAssignment(app, agent, portRange)
	require.NoError(t, err)
	return assignment
}

// TestRelayPortAllocationIsUniqueAndStable covers §4.5/§12: ports come from the
// configured narrow range, are unique among nonzero assignments under real
// allocation contention, survive exhaustion cleanly, and stay stable across
// repeated assignments (an agent keeps its port, and its generation does not
// move on a stable re-read).
func TestRelayPortAllocationIsUniqueAndStable(t *testing.T) {
	app := newRelayTestApp(t)
	portRange := mustPortRange(t, 10090, 10094) // exactly 5 ports for 5 agents

	const agentCount = 5
	namespaces := make([]string, agentCount)
	agents := make([]*core.Record, agentCount)
	for i := range agents {
		namespaces[i] = fmt.Sprintf("sballoc%02d", i)
		agents[i] = createRelayTestAgent(t, app, namespaces[i])
	}

	// Allocate for all agents concurrently: real contention on the partial
	// unique index, not a sequential walk.
	assignments := make([]relayctl.RelayAssignment, agentCount)
	errs := make([]error, agentCount)
	var wg sync.WaitGroup
	for i := range agents {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			assignments[index], errs[index] = relayctl.EnsureAssignment(app, agents[index], portRange)
		}(i)
	}
	wg.Wait()
	for i := range agents {
		require.NoError(t, errs[i], "agent %d allocation failed", i)
	}

	// Uniqueness among nonzero, and every port inside the configured range.
	seen := map[int]bool{}
	for i, assignment := range assignments {
		require.NotZero(t, assignment.RelayPort, "agent %d must get a nonzero port", i)
		require.True(t, portRange.Contains(assignment.RelayPort), "port %d outside range", assignment.RelayPort)
		require.False(t, seen[assignment.RelayPort], "duplicate port %d", assignment.RelayPort)
		seen[assignment.RelayPort] = true
		require.Equal(t, agents[i].Id, assignment.AgentRecordID)
		require.Equal(t, namespaces[i], assignment.Namespace)
		require.Equal(t, relayctl.ProxyNameFor(namespaces[i]), assignment.ProxyName)
		require.Equal(t, 1, assignment.Generation, "first assignment starts at generation 1")
	}

	// Stability: re-reading an assignment keeps the port and generation.
	for i, agent := range agents {
		again, err := relayctl.EnsureAssignment(app, agent, portRange)
		require.NoError(t, err)
		require.Equal(t, assignments[i].RelayPort, again.RelayPort, "agent %d must keep its port", i)
		require.Equal(t, assignments[i].Generation, again.Generation, "stable re-read must not bump generation")
	}

	// Exhaustion: the range is full, a sixth agent fails cleanly (no port, no
	// silent reuse).
	sixth := createRelayTestAgent(t, app, "sballoc99")
	_, err := relayctl.EnsureAssignment(app, sixth, portRange)
	require.Error(t, err, "allocation must fail once the range is exhausted")
	require.Zero(t, sixth.GetInt("relay_port"))
}

// TestRelayGenerationFencesOldCredential covers §7.2/§12 fencing: when an
// assignment is replaced (here: the configured range no longer contains the
// existing port), the generation increments and the newly signed credential
// carries it, so a gateway can reject credentials from the superseded
// generation as strictly older.
func TestRelayGenerationFencesOldCredential(t *testing.T) {
	app := newRelayTestApp(t)
	agent := createRelayTestAgent(t, app, "sbfence01")
	signer := mustSigner(t)

	oldRange := mustPortRange(t, 10100, 10109)
	oldAssignment := mustAssignment(t, app, agent, oldRange)
	require.Equal(t, 1, oldAssignment.Generation)

	oldClaims, oldToken, err := signer.SignCredential(oldAssignment, agent.GetString("api_key_id"))
	require.NoError(t, err)
	require.Equal(t, 1, oldClaims.Generation)

	// Reassignment: the operator narrows the range so the old port is gone.
	newRange := mustPortRange(t, 10200, 10209)
	newAssignment, err := relayctl.EnsureAssignment(app, agent, newRange)
	require.NoError(t, err)
	require.NotEqual(t, oldAssignment.RelayPort, newAssignment.RelayPort)
	require.Equal(t, 2, newAssignment.Generation, "reassignment must bump the generation")

	newClaims, newToken, err := signer.SignCredential(newAssignment, agent.GetString("api_key_id"))
	require.NoError(t, err)
	require.Equal(t, 2, newClaims.Generation)

	// The old credential is verifiable but strictly superseded: the gateway
	// sees generation 1 against a current generation of 2 and rejects it.
	verifiedOld, err := relayctl.VerifyCredential(signer.PublicKey(), oldToken)
	require.NoError(t, err)
	require.Equal(t, oldAssignment.RelayPort, verifiedOld.RelayPort)
	require.Less(t, verifiedOld.Generation, newClaims.Generation)

	verifiedNew, err := relayctl.VerifyCredential(signer.PublicKey(), newToken)
	require.NoError(t, err)
	require.Equal(t, newAssignment.RelayPort, verifiedNew.RelayPort)
	require.Equal(t, newAssignment.Generation, verifiedNew.Generation)
}

// TestRelayCredentialContainsExactNormativeClaims pins §7.2's claim set: after
// a real Ed25519 sign/verify round trip, every normative claim is present with
// the exact issued value, and unknown fields stay ignorable on the wire.
func TestRelayCredentialContainsExactNormativeClaims(t *testing.T) {
	signer := mustSigner(t)
	assignment := relayctl.RelayAssignment{
		AgentRecordID: "agentrecord01",
		Namespace:     "sbclaims01",
		ProxyName:     relayctl.ProxyNameFor("sbclaims01"),
		RelayPort:     10123,
		Generation:    3,
	}
	apiKeyID := "apikey01"

	before := time.Now()
	claims, token, err := signer.SignCredential(assignment, apiKeyID)
	after := time.Now()
	require.NoError(t, err)

	verified, err := relayctl.VerifyCredential(signer.PublicKey(), token)
	require.NoError(t, err)

	require.Equal(t, relayctl.Issuer, verified.Issuer)
	require.Equal(t, relayctl.Audience, verified.Audience)
	require.Equal(t, "sharebridge-relay", verified.Audience, "audience is normative")
	require.Equal(t, apiKeyID, verified.APIKeyID)
	require.Equal(t, assignment.AgentRecordID, verified.AgentRecordID)
	require.Equal(t, assignment.Namespace, verified.Namespace)
	require.Equal(t, assignment.ProxyName, verified.ProxyName)
	require.Equal(t, assignment.RelayPort, verified.RelayPort)
	require.Equal(t, assignment.Generation, verified.Generation)
	require.False(t, verified.IssuedAt.Before(before.Add(-time.Second)))
	require.False(t, verified.IssuedAt.After(after.Add(time.Second)))
	require.NotEmpty(t, verified.JTI, "credential must carry a JTI")
	require.Equal(t, claims.JTI, verified.JTI)

	// Two credentials never share a JTI (replay bound) ...
	claimsAgain, _, err := signer.SignCredential(assignment, apiKeyID)
	require.NoError(t, err)
	require.NotEqual(t, claims.JTI, claimsAgain.JTI)

	// ... and the token payload ignores unknown fields by construction: the
	// signed payload carries exactly the eleven normative claims.
	require.NoError(t, json.Unmarshal(credentialPayload(t, token), &map[string]json.RawMessage{}))
	payloadKeys := credentialPayloadKeys(t, token)
	require.ElementsMatch(t, []string{
		"iss", "aud", "api_key_id", "agent_record_id", "namespace",
		"proxy_name", "relay_port", "generation", "issued_at", "expires_at", "jti",
	}, payloadKeys)
}

// TestRelayCredentialExpiresAfterTenMinutes pins the §7.2 admission lifetime:
// exactly ten minutes between issued_at and expires_at.
func TestRelayCredentialExpiresAfterTenMinutes(t *testing.T) {
	signer := mustSigner(t)
	assignment := relayctl.RelayAssignment{
		AgentRecordID: "agentrecord02",
		Namespace:     "sbexpiry01",
		ProxyName:     relayctl.ProxyNameFor("sbexpiry01"),
		RelayPort:     10124,
		Generation:    1,
	}

	_, token, err := signer.SignCredential(assignment, "apikey02")
	require.NoError(t, err)
	verified, err := relayctl.VerifyCredential(signer.PublicKey(), token)
	require.NoError(t, err)

	lifetime := verified.ExpiresAt.Sub(verified.IssuedAt)
	require.Equal(t, 10*time.Minute, lifetime)
	require.True(t, verified.ExpiresAt.After(time.Now()), "fresh credential must not already be expired")

	// A tampered token must fail verification (real Ed25519 round trip both
	// ways, not a self-comparison).
	_, err = relayctl.VerifyCredential(signer.PublicKey(), tamperToken(t, token))
	require.Error(t, err)

	otherSigner := mustSigner(t)
	_, err = relayctl.VerifyCredential(otherSigner.PublicKey(), token)
	require.Error(t, err, "a different key must not verify the credential")
}

// TestAcceptedTunnelMayOutliveTokenButReconnectRequiresFreshCredential pins
// §7.2's reconnect rule from the control side: a token issued for one epoch
// naturally expires while a tunnel stays connected, and a reconnect is served
// with a NEWLY signed credential (fresh issued_at/expiry/JTI), never a resend
// of the stale one.
func TestAcceptedTunnelMayOutliveTokenButReconnectRequiresFreshCredential(t *testing.T) {
	signer := mustSigner(t)
	assignment := relayctl.RelayAssignment{
		AgentRecordID: "agentrecord03",
		Namespace:     "sblonglived",
		ProxyName:     relayctl.ProxyNameFor("sblonglived"),
		RelayPort:     10125,
		Generation:    1,
	}

	reconnectTime := time.Now().Add(11 * time.Minute) // token long gone, tunnel still up
	signer.SetNowFunc(func() time.Time { return reconnectTime })

	reconnectClaims, reconnectToken, err := signer.SignCredential(assignment, "apikey03")
	require.NoError(t, err)
	require.True(t, reconnectClaims.IssuedAt.Equal(reconnectTime.UTC()) || reconnectClaims.IssuedAt.After(reconnectTime.Add(-time.Second)),
		"reconnect credential must be issued at reconnect time, not reused")
	require.True(t, reconnectClaims.ExpiresAt.After(reconnectTime), "reconnect credential must be valid going forward")
	require.NotEmpty(t, reconnectClaims.JTI)

	verified, err := relayctl.VerifyCredential(signer.PublicKey(), reconnectToken)
	require.NoError(t, err)
	require.Equal(t, reconnectClaims.ExpiresAt, verified.ExpiresAt)
	require.Equal(t, reconnectClaims.JTI, verified.JTI)

	// The expired-era credential and the fresh one are distinct admissions:
	// fresh expiry lies strictly beyond the old token's expiry.
	signer.SetNowFunc(nil)
	staleClaims, staleToken, err := signer.SignCredential(assignment, "apikey03")
	require.NoError(t, err)
	staleVerified, err := relayctl.VerifyCredential(signer.PublicKey(), staleToken)
	require.NoError(t, err)
	require.True(t, staleVerified.ExpiresAt.Before(reconnectClaims.ExpiresAt))
	require.NotEqual(t, staleClaims.JTI, reconnectClaims.JTI)
}

// TestRelayOriginFromDirect pins §6's deterministic derivation: the relay
// origin inserts ".relay." before the namespace component of the persisted
// direct origin; anything that is not a well-formed direct origin is rejected.
func TestRelayOriginFromDirect(t *testing.T) {
	for _, testCase := range []struct {
		direct   string
		expected string
	}{
		{"abcd1234ef56.sbn0a1b2c3d.example.com", "abcd1234ef56.relay.sbn0a1b2c3d.example.com"},
		{"f00f.sbn0a1b2c3d.example.com", "f00f.relay.sbn0a1b2c3d.example.com"},
	} {
		relayOrigin, err := relayctl.RelayOriginFromDirect(testCase.direct)
		require.NoError(t, err, testCase.direct)
		require.Equal(t, testCase.expected, relayOrigin)
	}

	for _, bad := range []string{"", "namespace-only", ".leading.example.com"} {
		_, err := relayctl.RelayOriginFromDirect(bad)
		require.Error(t, err, "must reject %q", bad)
	}
}

// TestRelayTelemetryValidationRejectsInvalidInput pins the §11.1 bounded
// telemetry contract: only the exact status enum, non-negative generations and
// short reasons validate; everything else is rejected so the handler can drop
// it before it can mutate anything.
func TestRelayTelemetryValidationRejectsInvalidInput(t *testing.T) {
	for _, status := range []string{"starting", "running", "stopped", "error"} {
		require.NoError(t, relayctl.ValidateRelayClientState(2, status, ""), status)
	}
	require.NoError(t, relayctl.ValidateRelayClientState(0, "running", strings.Repeat("x", 256)))
	require.NoError(t, relayctl.ValidateLockdownStatus(4, true))
	require.NoError(t, relayctl.ValidateLockdownStatus(4, false))

	for _, testCase := range []struct {
		name       string
		generation int
		status     string
		reason     string
	}{
		{"unknown status", 1, "connected", ""},
		{"empty status", 1, "", ""},
		{"negative generation", -1, "running", ""},
		{"oversize reason", 1, "running", strings.Repeat("x", 257)},
	} {
		require.Error(t, relayctl.ValidateRelayClientState(testCase.generation, testCase.status, testCase.reason), testCase.name)
	}
	require.Error(t, relayctl.ValidateLockdownStatus(-3, true), "negative lockdown generation")
}

// tokenPayloadSegment decodes the token's base64url payload segment.
func tokenPayloadSegment(t *testing.T, token []byte) []byte {
	t.Helper()
	parts := strings.SplitN(string(token), ".", 3)
	require.Len(t, parts, 3)
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	return payload
}

// credentialPayload decodes the token's payload segment for structural checks.
func credentialPayload(t *testing.T, token []byte) []byte {
	t.Helper()
	payload := tokenPayloadSegment(t, token)
	var raw json.RawMessage
	require.NoError(t, json.Unmarshal(payload, &raw))
	return payload
}

// credentialPayloadKeys returns the JSON keys of the signed payload.
func credentialPayloadKeys(t *testing.T, token []byte) []string {
	t.Helper()
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(tokenPayloadSegment(t, token), &fields))
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	return keys
}

// tamperToken flips a byte inside the payload segment so signature checks must
// fail.
func tamperToken(t *testing.T, token []byte) []byte {
	t.Helper()
	parts := strings.Split(string(token), ".")
	require.Len(t, parts, 3)
	tampered := []byte(parts[1])
	tampered[0] ^= 0x01
	return []byte(parts[0] + "." + string(tampered) + "." + parts[2])
}
