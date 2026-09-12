package controlsync

// M5 remediation round 2, Fix A2 — epoch-conditional watermark invalidation.
//
// The restart boundary has a second edge: a refusal for an ack the gateway
// sent for a SUPERSEDED epoch must not clear the watermark the gateway has
// since earned for the CURRENT epoch. The gateway's acks are synchronous, so
// this edge is reachable only when the applied epoch advances between reading
// the applied state and handling the refusal; the invalidation is therefore
// parameterised by the rejected epoch. This test pins that parameterisation
// directly: a refusal for epoch 8 must leave epoch 9's accepted watermark
// intact (the two sides still agree the epoch-9 ack exists), while a refusal
// for the applied epoch 9 must withdraw it (control recorded nothing).

import (
	"testing"
	"time"

	"sharebridge/relay/internal/routes"
)

func TestAckRefusalInvalidatesOnlyTheRejectedEpoch(t *testing.T) {
	certs := newSyncTestCertificates(t)
	_, client := newScriptedControl(t, certs)
	table := routes.NewTable(newReconcilePresence())
	applier := mustApplier(t, client, table, nil, time.Now)

	// The gateway has applied epoch 9 and holds an ACCEPTED ack for it.
	applier.setState(9, 5)
	applier.recordAck(9, 5)
	if !applier.ControlSyncHealthy() {
		t.Fatal("precondition: an accepted ack for the applied epoch must be healthy")
	}

	// A delayed refusal for the SUPERSEDED epoch 8 must not clear epoch 9's
	// watermark: control still holds the epoch-9 ack, so both sides agree it
	// exists.
	applier.invalidateAck(8)
	if !applier.ControlSyncHealthy() {
		t.Fatal("a refusal for epoch 8 must not withdraw epoch 9's acknowledged watermark")
	}

	// A refusal for the CURRENT epoch 9 does withdraw it: control recorded
	// nothing, so the gateway may not claim the ack exists.
	applier.invalidateAck(9)
	if applier.ControlSyncHealthy() {
		t.Fatal("a refusal for the applied epoch must withdraw the watermark")
	}
}
