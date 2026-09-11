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

// TestEndpointReportOrderYieldsHealthyFinalState is the control-side half of
// the M4 closeout batch 1b A2 ordering contract. HandleReportEndpoint applies
// reports in arrival order and the LAST report wins, so the agent's reporter
// must deliver an endpoint's open report after the advisories that belong to
// the same open. Feeding the healthy arrival order (close_failed -> closed ->
// open) must leave the endpoint healthy (not close_failed, not port-zero);
// feeding the reordered order the prioritized openCh produced (open ->
// close_failed -> closed) demonstrably leaves it suppressed, which is why the
// reporter ordering is load-bearing.
func TestEndpointReportOrderYieldsHealthyFinalState(t *testing.T) {
	apiKeyID, ctrl := closeFailedEscalationFixture(t, "cf12")

	// Healthy arrival order: the lingering-mapping close failure, then its
	// successful deletion (port 0), then the new mapping's open report.
	ctrl.HandleReportEndpoint(context.Background(), nil, apiKeyID, routeDirectIP, 8443, "close_failed")
	ctrl.HandleReportEndpoint(context.Background(), nil, apiKeyID, routeDirectIP, 0, "")
	ctrl.HandleReportEndpoint(context.Background(), nil, apiKeyID, routeDirectIP, 8443, "")

	rec, _, err := LoadOrCreateAgent(ctrl.app, apiKeyID)
	require.NoError(t, err)
	require.Equal(t, "", rec.GetString("endpoint_status"),
		"a healthy open report must clear the close_failed escalation")
	require.Equal(t, 8443, rec.GetInt("endpoint_port"),
		"the healthy endpoint's granted port must win; it must not be zeroed by a stale closed advisory")

	// A genuine subsequent failure (the LAST report) still suppresses direct.
	ctrl.HandleReportEndpoint(context.Background(), nil, apiKeyID, routeDirectIP, 8443, "close_failed")
	rec, _, err = LoadOrCreateAgent(ctrl.app, apiKeyID)
	require.NoError(t, err)
	require.Equal(t, "close_failed", rec.GetString("endpoint_status"),
		"a genuine unresolved close_failed must still be persisted")
	assertSelection(t, ctrl, "cf12", http.StatusFound, http.StatusFound, routeRelayOriginFor("cf12"))
}

// TestEndpointReorderedReportSequenceSuppressesHealthyEndpoint documents the
// concrete regression the prioritized openCh produced: when control receives
// the open before the advisories that were queued before it, the final state is
// port-zero (direct suppressed) although the endpoint is healthy. This is the
// sequence the single-FIFO reporter fix prevents.
func TestEndpointReorderedReportSequenceSuppressesHealthyEndpoint(t *testing.T) {
	apiKeyID, ctrl := closeFailedEscalationFixture(t, "cf13")

	// The pre-fix priority-channel arrival order: the open overtakes the
	// close_failed/closed advisories that were queued before it.
	ctrl.HandleReportEndpoint(context.Background(), nil, apiKeyID, routeDirectIP, 8443, "")
	ctrl.HandleReportEndpoint(context.Background(), nil, apiKeyID, routeDirectIP, 8443, "close_failed")
	ctrl.HandleReportEndpoint(context.Background(), nil, apiKeyID, routeDirectIP, 0, "")

	rec, _, err := LoadOrCreateAgent(ctrl.app, apiKeyID)
	require.NoError(t, err)
	require.Equal(t, 0, rec.GetInt("endpoint_port"),
		"the stale closed advisory zeroes the healthy endpoint's port")
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
