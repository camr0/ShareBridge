package stun

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/stun/v3"
)

// Fixed test material. The challenge ID and secret mirror the control
// listener's issuance sizes (control/internal/stun: 16-byte ID hex-encoded,
// 32-byte one-use secret); the receipt mirrors its 16-byte HMAC truncation.
var (
	testChallengeIDBytes = []byte{
		0x5b, 0x1e, 0x8f, 0x22, 0xa9, 0x4c, 0x03, 0xd7,
		0x6b, 0xf0, 0x91, 0x38, 0x2e, 0xaa, 0x47, 0x5c,
	}
	testSecret = []byte{
		0x11, 0x2a, 0x43, 0x5c, 0x75, 0x8e, 0xa7, 0xc0,
		0xd9, 0xf2, 0x0b, 0x24, 0x3d, 0x56, 0x6f, 0x88,
		0xa1, 0xba, 0xd3, 0xec, 0x05, 0x1e, 0x37, 0x50,
		0x69, 0x82, 0x9b, 0xb4, 0xcd, 0xe6, 0xff, 0x18,
	}
	testReceipt = []byte{
		0xde, 0xad, 0xbe, 0xef, 0x01, 0x23, 0x45, 0x67,
		0x89, 0xab, 0xcd, 0xef, 0xfe, 0xdc, 0xba, 0x98,
	}
)

func testChallengeID() string    { return hex.EncodeToString(testChallengeIDBytes) }
func testSecretHex() string      { return hex.EncodeToString(testSecret) }
func testChallengeField() string { return testChallengeID() + "." + testSecretHex() }

// startFakeControlListener binds one loopback UDP socket and answers the
// first request through the respond callback, replicating the construction
// order of control/internal/stun's success path (header, XOR-MAPPED-ADDRESS,
// MAPPED-ADDRESS, receipt attribute, MESSAGE-INTEGRITY last so the HMAC
// covers exactly the wire bytes). It records every datagram on the returned
// channel: the client must produce exactly one request per challenge, so any
// second datagram is a failure signal. The respond callback must not touch
// *testing.T (it runs on a goroutine).
func startFakeControlListener(t *testing.T, respond func(conn *net.UDPConn, request []byte, src *net.UDPAddr)) (*net.UDPAddr, <-chan []byte) {
	t.Helper()
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen fake control STUN listener: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	requests := make(chan []byte, 4)
	go func() {
		buffer := make([]byte, 1500)
		n, src, err := listener.ReadFromUDP(buffer)
		if err != nil {
			return
		}
		request := append([]byte(nil), buffer[:n]...)
		select {
		case requests <- request:
		default:
		}
		if respond != nil {
			respond(listener, request, src)
		}
		// The client sends exactly one Binding request per challenge; a
		// second datagram is recorded so tests can fail on it.
		_ = listener.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if extra, _, err := listener.ReadFromUDP(buffer); err == nil {
			select {
			case requests <- append([]byte(nil), buffer[:extra]...):
			default:
			}
		}
	}()
	return listener.LocalAddr().(*net.UDPAddr), requests
}

// buildControlSuccessResponse mirrors control/internal/stun's sendSuccess:
// attributes are added before MESSAGE-INTEGRITY and the message is never
// re-encoded afterwards, so the response integrity covers the receipt.
func buildControlSuccessResponse(transactionID [stun.TransactionIDSize]byte, src *net.UDPAddr, secret, receipt []byte) []byte {
	response := stun.New()
	response.Type = stun.BindingSuccess
	response.TransactionID = transactionID
	response.WriteHeader()
	ip := net.IP(src.IP.To4())
	if err := (stun.XORMappedAddress{IP: ip, Port: src.Port}).AddTo(response); err != nil {
		return nil
	}
	mappedAddr := stun.MappedAddress{IP: ip, Port: src.Port}
	if err := mappedAddr.AddTo(response); err != nil {
		return nil
	}
	response.Add(AttrReceipt, receipt)
	if err := stun.NewShortTermIntegrity(string(secret)).AddTo(response); err != nil {
		return nil
	}
	return response.Raw
}

// parseRequest verifies a recorded Binding request the way the real listener
// does: Binding method, USERNAME equal to the challenge ID, and
// MESSAGE-INTEGRITY keyed with the one-use secret. It returns the request's
// transaction ID so tests can assert the echo.
func parseRequest(t *testing.T, request []byte, challengeID string, secret []byte) [stun.TransactionIDSize]byte {
	t.Helper()
	if !stun.IsMessage(request) {
		t.Fatalf("recorded request is not a STUN message")
	}
	message := stun.New()
	message.Raw = request
	if err := message.Decode(); err != nil {
		t.Fatalf("decode recorded request: %v", err)
	}
	if message.Type != stun.BindingRequest {
		t.Fatalf("request method = %v, want Binding request", message.Type)
	}
	var username stun.Username
	if err := username.GetFrom(message); err != nil {
		t.Fatalf("request USERNAME missing: %v", err)
	}
	if string(username) != challengeID {
		t.Fatalf("request USERNAME = %q, want challenge ID %q", string(username), challengeID)
	}
	if !message.Contains(stun.AttrMessageIntegrity) {
		t.Fatalf("request carries no MESSAGE-INTEGRITY")
	}
	if err := stun.NewShortTermIntegrity(string(secret)).Check(message); err != nil {
		t.Fatalf("request MESSAGE-INTEGRITY invalid for the one-use secret: %v", err)
	}
	return message.TransactionID
}

// assertExactlyOneRequest drains the recorded requests after the stray-read
// window has passed and asserts exactly one matching Binding request arrived.
func assertExactlyOneRequest(t *testing.T, requests <-chan []byte, challengeID string, secret []byte) [stun.TransactionIDSize]byte {
	t.Helper()
	time.Sleep(400 * time.Millisecond) // longer than the listener stray-read window
	if len(requests) != 1 {
		t.Fatalf("request count = %d, want exactly 1 (one Binding request per challenge)", len(requests))
	}
	return parseRequest(t, <-requests, challengeID, secret)
}

func TestClientSendsIntegrityProtectedBindingAndEchoesReceipt(t *testing.T) {
	serverAddr, requests := startFakeControlListener(t, func(conn *net.UDPConn, request []byte, src *net.UDPAddr) {
		message := stun.New()
		message.Raw = request
		if err := message.Decode(); err != nil {
			return
		}
		response := buildControlSuccessResponse(message.TransactionID, src, testSecret, testReceipt)
		if response == nil {
			return
		}
		_, _ = conn.WriteToUDP(response, src)
	})

	now := time.Now()
	challenge, err := ParseChallenge(
		ChallengeVersion,
		testChallengeField(),
		serverAddr.String(),
		now.Add(30*time.Second),
		now,
	)
	if err != nil {
		t.Fatalf("ParseChallenge: %v", err)
	}

	client := NewClient()
	result, err := client.Exchange(context.Background(), challenge)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}

	transactionID := assertExactlyOneRequest(t, requests, testChallengeID(), testSecret)
	if result.ChallengeID != testChallengeID() {
		t.Fatalf("result challenge = %q, want %q (the §11.1 stun_result challenge echo)", result.ChallengeID, testChallengeID())
	}
	if want := hex.EncodeToString(transactionID[:]); result.TransactionID != want {
		t.Fatalf("result transaction_id = %q, want %q", result.TransactionID, want)
	}
	if len(result.TransactionID) != 2*stun.TransactionIDSize {
		t.Fatalf("transaction_id length = %d, want %d hex characters", len(result.TransactionID), 2*stun.TransactionIDSize)
	}
	if !bytes.Equal(result.Receipt, testReceipt) {
		t.Fatalf("result receipt = %x, want %x (opaque bytes echoed unmodified)", result.Receipt, testReceipt)
	}
}

func TestClientRejectsWrongTransactionIntegrityAndExpiredChallenge(t *testing.T) {
	const fastTimeout = 300 * time.Millisecond

	// expireAt returns a challenge expiry and the matching single clock
	// reading used at parse time.
	expireAt := func(lifetime time.Duration) (time.Time, time.Time) {
		now := time.Now()
		return now.Add(lifetime), now
	}

	t.Run("wrong_transaction_id_is_never_accepted", func(t *testing.T) {
		serverAddr, requests := startFakeControlListener(t, func(conn *net.UDPConn, request []byte, src *net.UDPAddr) {
			message := stun.New()
			message.Raw = request
			if err := message.Decode(); err != nil {
				return
			}
			stranger := message.TransactionID
			stranger[0] ^= 0xff
			response := buildControlSuccessResponse(stranger, src, testSecret, testReceipt)
			if response == nil {
				return
			}
			_, _ = conn.WriteToUDP(response, src)
		})
		expiry, now := expireAt(30 * time.Second)
		challenge, err := ParseChallenge(ChallengeVersion, testChallengeField(), serverAddr.String(), expiry, now)
		if err != nil {
			t.Fatalf("ParseChallenge: %v", err)
		}
		client := NewClient(withResponseTimeout(fastTimeout))
		_, err = client.Exchange(context.Background(), challenge)
		if !errors.Is(err, ErrResponseTimeout) {
			t.Fatalf("Exchange error = %v, want ErrResponseTimeout (mismatched transaction must never be accepted)", err)
		}
		assertExactlyOneRequest(t, requests, testChallengeID(), testSecret)
	})

	t.Run("wrong_integrity_is_never_accepted", func(t *testing.T) {
		serverAddr, requests := startFakeControlListener(t, func(conn *net.UDPConn, request []byte, src *net.UDPAddr) {
			message := stun.New()
			message.Raw = request
			if err := message.Decode(); err != nil {
				return
			}
			attackerSecret := append([]byte(nil), testSecret...)
			attackerSecret[0] ^= 0xff
			response := buildControlSuccessResponse(message.TransactionID, src, attackerSecret, testReceipt)
			if response == nil {
				return
			}
			_, _ = conn.WriteToUDP(response, src)
		})
		expiry, now := expireAt(30 * time.Second)
		challenge, err := ParseChallenge(ChallengeVersion, testChallengeField(), serverAddr.String(), expiry, now)
		if err != nil {
			t.Fatalf("ParseChallenge: %v", err)
		}
		client := NewClient(withResponseTimeout(fastTimeout))
		_, err = client.Exchange(context.Background(), challenge)
		if !errors.Is(err, ErrResponseTimeout) {
			t.Fatalf("Exchange error = %v, want ErrResponseTimeout (integrity failure must never be accepted)", err)
		}
		assertExactlyOneRequest(t, requests, testChallengeID(), testSecret)
	})

	t.Run("missing_integrity_is_never_accepted", func(t *testing.T) {
		serverAddr, requests := startFakeControlListener(t, func(conn *net.UDPConn, request []byte, src *net.UDPAddr) {
			message := stun.New()
			message.Raw = request
			if err := message.Decode(); err != nil {
				return
			}
			response := stun.New()
			response.Type = stun.BindingSuccess
			response.TransactionID = message.TransactionID
			response.WriteHeader()
			ip := net.IP(src.IP.To4())
			if err := (stun.XORMappedAddress{IP: ip, Port: src.Port}).AddTo(response); err != nil {
				return
			}
			response.Add(AttrReceipt, testReceipt)
			_, _ = conn.WriteToUDP(response.Raw, src)
		})
		expiry, now := expireAt(30 * time.Second)
		challenge, err := ParseChallenge(ChallengeVersion, testChallengeField(), serverAddr.String(), expiry, now)
		if err != nil {
			t.Fatalf("ParseChallenge: %v", err)
		}
		client := NewClient(withResponseTimeout(fastTimeout))
		_, err = client.Exchange(context.Background(), challenge)
		if !errors.Is(err, ErrResponseTimeout) {
			t.Fatalf("Exchange error = %v, want ErrResponseTimeout (unauthenticated response must never be accepted)", err)
		}
		assertExactlyOneRequest(t, requests, testChallengeID(), testSecret)
	})

	t.Run("receipt_missing_or_oversized_is_never_accepted", func(t *testing.T) {
		oversized := bytes.Repeat([]byte{0xaa}, maxReceiptSize+1)
		for name, receipt := range map[string][]byte{
			"missing":   nil,
			"oversized": oversized,
		} {
			t.Run(name, func(t *testing.T) {
				serverAddr, requests := startFakeControlListener(t, func(conn *net.UDPConn, request []byte, src *net.UDPAddr) {
					message := stun.New()
					message.Raw = request
					if err := message.Decode(); err != nil {
						return
					}
					response := stun.New()
					response.Type = stun.BindingSuccess
					response.TransactionID = message.TransactionID
					response.WriteHeader()
					ip := net.IP(src.IP.To4())
					_ = (stun.XORMappedAddress{IP: ip, Port: src.Port}).AddTo(response)
					if receipt != nil {
						response.Add(AttrReceipt, receipt)
					}
					_ = stun.NewShortTermIntegrity(string(testSecret)).AddTo(response)
					_, _ = conn.WriteToUDP(response.Raw, src)
				})
				expiry, now := expireAt(30 * time.Second)
				challenge, err := ParseChallenge(ChallengeVersion, testChallengeField(), serverAddr.String(), expiry, now)
				if err != nil {
					t.Fatalf("ParseChallenge: %v", err)
				}
				client := NewClient(withResponseTimeout(fastTimeout))
				_, err = client.Exchange(context.Background(), challenge)
				if !errors.Is(err, ErrResponseTimeout) {
					t.Fatalf("Exchange error = %v, want ErrResponseTimeout (response without a bounded receipt is not an observation)", err)
				}
				assertExactlyOneRequest(t, requests, testChallengeID(), testSecret)
			})
		}
	})

	t.Run("binding_error_response_rejects", func(t *testing.T) {
		serverAddr, requests := startFakeControlListener(t, func(conn *net.UDPConn, request []byte, src *net.UDPAddr) {
			message := stun.New()
			message.Raw = request
			if err := message.Decode(); err != nil {
				return
			}
			response := stun.New()
			response.Type = stun.BindingError
			response.TransactionID = message.TransactionID
			response.WriteHeader()
			if err := (stun.ErrorCodeAttribute{Code: stun.CodeUnauthorized, Reason: []byte("Unauthorized")}).AddTo(response); err != nil {
				return
			}
			_, _ = conn.WriteToUDP(response.Raw, src)
		})
		expiry, now := expireAt(30 * time.Second)
		challenge, err := ParseChallenge(ChallengeVersion, testChallengeField(), serverAddr.String(), expiry, now)
		if err != nil {
			t.Fatalf("ParseChallenge: %v", err)
		}
		client := NewClient()
		_, err = client.Exchange(context.Background(), challenge)
		if !errors.Is(err, ErrChallengeRejected) {
			t.Fatalf("Exchange error = %v, want ErrChallengeRejected (burned or expired credential)", err)
		}
		assertExactlyOneRequest(t, requests, testChallengeID(), testSecret)
	})

	// The remaining cases must drop the challenge BEFORE any network I/O:
	// expired or invalid challenges never produce a request.
	t.Run("expired_challenge_makes_no_request", func(t *testing.T) {
		sent := &atomicCounter{}
		serverAddr, _ := startFakeControlListener(t, func(conn *net.UDPConn, request []byte, src *net.UDPAddr) {
			sent.add(1)
		})
		expiry, now := expireAt(-time.Second)
		challenge, err := ParseChallenge(ChallengeVersion, testChallengeField(), serverAddr.String(), expiry, now)
		if !errors.Is(err, ErrChallengeExpired) {
			t.Fatalf("ParseChallenge error = %v, want ErrChallengeExpired", err)
		}
		if challenge.id != "" || challenge.secret != nil {
			t.Fatalf("expired challenge must not yield usable credential material")
		}
		// A direct Exchange with a Challenge that expired after parsing must
		// also fail closed (single clock reading, no request).
		expiry, now = expireAt(30 * time.Second)
		challenge, err = ParseChallenge(ChallengeVersion, testChallengeField(), serverAddr.String(), expiry, now)
		if err != nil {
			t.Fatalf("ParseChallenge: %v", err)
		}
		client := NewClient(withNow(func() time.Time { return now.Add(2 * time.Minute) }))
		if _, err := client.Exchange(context.Background(), challenge); !errors.Is(err, ErrChallengeExpired) {
			t.Fatalf("Exchange error = %v, want ErrChallengeExpired", err)
		}
		time.Sleep(100 * time.Millisecond)
		if got := sent.load(); got != 0 {
			t.Fatalf("request count = %d, want 0 (expired challenge must fail closed with no request)", got)
		}
	})

	t.Run("unsupported_version_makes_no_request", func(t *testing.T) {
		sent := &atomicCounter{}
		serverAddr, _ := startFakeControlListener(t, func(conn *net.UDPConn, request []byte, src *net.UDPAddr) {
			sent.add(1)
		})
		expiry, now := expireAt(30 * time.Second)
		if _, err := ParseChallenge(ChallengeVersion+1, testChallengeField(), serverAddr.String(), expiry, now); !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("ParseChallenge error = %v, want ErrUnsupportedVersion", err)
		}
		if got := sent.load(); got != 0 {
			t.Fatalf("request count = %d, want 0", got)
		}
	})

	t.Run("malformed_challenge_makes_no_request", func(t *testing.T) {
		sent := &atomicCounter{}
		serverAddr, _ := startFakeControlListener(t, func(conn *net.UDPConn, request []byte, src *net.UDPAddr) {
			sent.add(1)
		})
		expiry, now := expireAt(30 * time.Second)
		malformed := map[string]string{
			"no separator":    testChallengeID() + testSecretHex(),
			"two separators":  testChallengeField() + "." + testSecretHex(),
			"short id":        "abc." + testSecretHex(),
			"non-hex id":      strings.Repeat("zz", 16) + "." + testSecretHex(),
			"short secret":    testChallengeID() + "." + testSecretHex()[1:],
			"empty challenge": "",
			"oversize":        strings.Repeat("a", maxChallengeFieldLength+1),
		}
		for name, field := range malformed {
			if _, err := ParseChallenge(ChallengeVersion, field, serverAddr.String(), expiry, now); !errors.Is(err, ErrMalformedChallenge) {
				t.Fatalf("%s: ParseChallenge error = %v, want ErrMalformedChallenge", name, err)
			}
		}
		if _, err := ParseChallenge(ChallengeVersion, testChallengeField(), "", expiry, now); err == nil {
			t.Fatalf("empty server must be rejected")
		}
		if _, err := ParseChallenge(ChallengeVersion, testChallengeField(), "control.example.net:0", expiry, now); err == nil {
			t.Fatalf("server port 0 must be rejected")
		}
		if _, err := ParseChallenge(ChallengeVersion, testChallengeField(), "control.example.net", expiry, now); err == nil {
			t.Fatalf("server without port must be rejected")
		}
		if got := sent.load(); got != 0 {
			t.Fatalf("request count = %d, want 0 (invalid challenge must fail closed with no request)", got)
		}
	})
}

// atomicCounter is a tiny concurrency-safe counter for the no-request
// assertions (avoids importing sync/atomic spelling noise at every call site).
type atomicCounter struct {
	value atomic.Int32
}

func (counter *atomicCounter) add(delta int32) { counter.value.Add(delta) }
func (counter *atomicCounter) load() int32     { return counter.value.Load() }
