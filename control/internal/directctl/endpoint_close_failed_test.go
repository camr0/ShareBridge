package directctl

// M4 closeout batch 1, finding 3 (control half): control accepted a
// "close_failed" endpoint report but did not persist its status, so the agent's
// documented escalation was dropped on the floor — the only consumer was the
// wire parser. These tests pin the production consumer: the status is
// persisted durably on the agent row and direct selection fails closed to relay
// until a later report clears it.

import (
	"context"
	"net/http"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

// closeFailedEscalationFixture seeds one direct-eligible agent/session pair
// (eligible STUN observation, matching endpoint, live WS, relay lease) so the
// close_failed escalation's effect on selection is observable.
func closeFailedEscalationFixture(t *testing.T, code string) (apiKeyID string, ctrl *Controller) {
	t.Helper()
	app, ctrl, _, view, _ := newSelectionController(t, true)
	apiKeyID = routeSession(t, app, code, nil)
	port := seedAgentFacts(t, app, apiKeyID, routeDirectIP)
	grantPresence(t, view, agentRecordID(t, app, apiKeyID), port)
	connectWS(t, ctrl, apiKeyID)
	installObservation(t, ctrl, apiKeyID, netip.MustParseAddr(routeDirectIP), true)
	return apiKeyID, ctrl
}

// TestReportEndpointPersistsCloseFailedEscalation asserts the durable record:
// a valid close_failed report (nonzero port) must be persisted with a timestamp
// instead of being accepted and dropped, and a later normal report must clear
// it.
func TestReportEndpointPersistsCloseFailedEscalation(t *testing.T) {
	apiKeyID, ctrl := closeFailedEscalationFixture(t, "cf10")

	ctrl.HandleReportEndpoint(context.Background(), nil, apiKeyID, routeDirectIP, 8443, "close_failed")

	rec, _, err := LoadOrCreateAgent(ctrl.app, apiKeyID)
	require.NoError(t, err)
	require.Equal(t, "close_failed", rec.GetString("endpoint_status"),
		"the close_failed escalation must be persisted, not silently dropped")
	require.False(t, rec.GetDateTime("endpoint_status_at").IsZero(),
		"the escalation must carry a durable timestamp")

	// A later report for the same endpoint clears the escalation.
	ctrl.HandleReportEndpoint(context.Background(), nil, apiKeyID, routeDirectIP, 8443, "")
	rec, _, err = LoadOrCreateAgent(ctrl.app, apiKeyID)
	require.NoError(t, err)
	require.Equal(t, "", rec.GetString("endpoint_status"),
		"a later report must clear the close_failed escalation")
}

// TestCloseFailedEscalationSuppressesDirectSelection pins the production
// consumer: while the agent reports that its owned mapping could not be
// released (a mapping may still be live on the router), selection must not
// advertise direct — it falls back to the live relay instead. The escalation
// suppresses only; a later successful report restores the direct interstitial.
func TestCloseFailedEscalationSuppressesDirectSelection(t *testing.T) {
	apiKeyID, ctrl := closeFailedEscalationFixture(t, "cf11")
	const code = "cf11"

	// Baseline: an eligible direct candidate gets the interstitial.
	assertSelection(t, ctrl, code, http.StatusFound, http.StatusOK, "")

	ctrl.HandleReportEndpoint(context.Background(), nil, apiKeyID, routeDirectIP, 8443, "close_failed")

	// The live relay lease carries the navigation while direct is suppressed.
	assertSelection(t, ctrl, code, http.StatusFound, http.StatusFound, routeRelayOriginFor(code))

	// Clearing the escalation restores the direct interstitial.
	ctrl.HandleReportEndpoint(context.Background(), nil, apiKeyID, routeDirectIP, 8443, "")
	assertSelection(t, ctrl, code, http.StatusFound, http.StatusOK, "")
}
