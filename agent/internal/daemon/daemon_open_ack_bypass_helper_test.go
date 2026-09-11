package daemon

// M4 remediation round C, fix round 3: the "new daemon-package caller" fixture.
//
// A future contributor adding an open path would construct an OK ack somewhere
// other than ackOpenSuccess — possibly in a new file or helper like this one.
// These helpers deliberately build such acks WITHOUT the choke point's
// validation context, so the daemon seal tests can prove the transport refuses
// them. Test-only file: the production structural guard walks production files
// only, so these OK constructions do not (and must not) count as writers.

import "sharebridge/agent/internal/signaling"

// bypassOpenAck is an OK open_ack built the way the A1 bypass described it: a
// direct Status: "ok" construction with no validation context and no
// CommitOpenAck token. It is what a new daemon-package caller could write.
func bypassOpenAck(shareID, nonce string, seq uint64, port int) signaling.OpenAck {
	return signaling.OpenAck{
		ShareID:        shareID,
		Nonce:          nonce,
		Seq:            seq,
		GrantedPort:    port,
		PublicIP:       "203.0.113.7",
		WasAlreadyOpen: false,
		Status:         "ok",
	}
}

// withValidation is the ack the choke point builds: the port's committed
// granted port plus the direct-state generation the open was admitted under.
func withValidation(port int, epoch uint64) signaling.OpenAck {
	return signaling.OpenAck{
		ShareID:     "SHARE123",
		Nonce:       "nonce-validated",
		Seq:         1,
		GrantedPort: port,
		PublicIP:    "203.0.113.7",
		Status:      "ok",
		Validation:  &signaling.OpenAckValidation{Epoch: epoch},
	}
}
