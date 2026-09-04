package clienthello

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const expectedSNI = "r7k2m9p4x6.v7q4km2x9pz6dn3w.sharebridgeusercontent.com"

// keyShareVector is the x25519 public key planted in the TLS 1.3 fixture so
// fragmentation tests can cut records inside key_share data.
const keyShareVector = "ShareBridgeKeyShareTestVector000"

var (
	testSessionID       = bytes.Repeat([]byte{0x11}, 32)
	defaultCipherSuites = []uint16{0x1301, 0x1302, 0x1303, 0xc02f}
)

// fixtureRecord loads a hex-encoded single-record ClientHello from testdata.
func fixtureRecord(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	record, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
	if len(record) <= recordHeaderSize || record[0] != contentTypeHandshake {
		t.Fatalf("fixture %s is not a handshake record", name)
	}
	return record
}

// peekInput feeds input into one end of an in-memory pipe and runs the parser
// on the other end. closeAfterWrite closes the writer side after the input is
// flushed, simulating a peer that hangs up mid-hello.
func peekInput(t *testing.T, input []byte, readTimeout time.Duration, closeAfterWrite bool) (*Hello, error) {
	t.Helper()
	gatewaySide, clientSide := net.Pipe()
	t.Cleanup(func() {
		gatewaySide.Close()
		clientSide.Close()
	})
	go func() {
		written := 0
		for written < len(input) {
			count, err := clientSide.Write(input[written:])
			if err != nil {
				return
			}
			written += count
		}
		if closeAfterWrite {
			clientSide.Close()
		}
	}()
	return peek(gatewaySide, readTimeout)
}

func cipherSuitesBody(suites []uint16) []byte {
	body := make([]byte, 0, 2*len(suites))
	for _, suite := range suites {
		body = append(body, byte(suite>>8), byte(suite))
	}
	return body
}

// buildClientHelloMessage assembles a complete handshake message (4-byte
// header included) from its wire fields.
func buildClientHelloMessage(clientVersion [2]byte, sessionID, cipherSuites, compressionMethods, extensionBlock []byte) []byte {
	body := []byte{clientVersion[0], clientVersion[1]}
	body = append(body, bytes.Repeat([]byte{0xA5}, randomSize)...) // random
	body = append(body, byte(len(sessionID)))
	body = append(body, sessionID...)
	body = append(body, byte(len(cipherSuites)>>8), byte(len(cipherSuites)))
	body = append(body, cipherSuites...)
	body = append(body, byte(len(compressionMethods)))
	body = append(body, compressionMethods...)
	if extensionBlock != nil {
		body = append(body, byte(len(extensionBlock)>>8), byte(len(extensionBlock)))
		body = append(body, extensionBlock...)
	}
	message := make([]byte, handshakeHeaderSize+len(body))
	message[0] = handshakeTypeClientHello
	message[1] = byte(len(body) >> 16)
	message[2] = byte(len(body) >> 8)
	message[3] = byte(len(body))
	copy(message[handshakeHeaderSize:], body)
	return message
}

func tlsRecord(contentType byte, version [2]byte, body []byte) []byte {
	record := make([]byte, recordHeaderSize+len(body))
	record[0] = contentType
	record[1] = version[0]
	record[2] = version[1]
	record[3] = byte(len(body) >> 8)
	record[4] = byte(len(body))
	copy(record[recordHeaderSize:], body)
	return record
}

// fragmentedHandshakeRecords wraps a complete handshake message into TLS
// handshake records, cutting the message at the given ascending offsets. The
// first fragment carries the 4-byte handshake header.
func fragmentedHandshakeRecords(version [2]byte, message []byte, splitOffsets ...int) []byte {
	var stream []byte
	start := 0
	for _, split := range splitOffsets {
		stream = append(stream, tlsRecord(contentTypeHandshake, version, message[start:split])...)
		start = split
	}
	return append(stream, tlsRecord(contentTypeHandshake, version, message[start:])...)
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

func helloExtension(extensionType uint16, body []byte) []byte {
	extension := make([]byte, 4+len(body))
	extension[0] = byte(extensionType >> 8)
	extension[1] = byte(extensionType)
	extension[2] = byte(len(body) >> 8)
	extension[3] = byte(len(body))
	copy(extension[4:], body)
	return extension
}

func hostNameEntry(nameType byte, host string) []byte {
	entry := []byte{nameType, byte(len(host) >> 8), byte(len(host))}
	return append(entry, host...)
}

func serverNameList(entries ...[]byte) []byte {
	total := 0
	for _, entry := range entries {
		total += len(entry)
	}
	list := []byte{byte(total >> 8), byte(total)}
	for _, entry := range entries {
		list = append(list, entry...)
	}
	return list
}

func serverNameExtension(host string) []byte {
	return helloExtension(extensionTypeServerName, serverNameList(hostNameEntry(serverNameTypeHost, host)))
}

// echExtension returns a realistic outer encrypted_client_hello extension.
func echExtension() []byte {
	const encSecret = "ShareBridgeGreaseECHOuterEnc0000"
	const payload = "ShareBridgeECH00"
	body := []byte{0x00, 0x00, 0x01} // outer + HpkeKdfId HKDF-SHA256
	body = append(body, byte(len(encSecret)>>8), byte(len(encSecret)))
	body = append(body, encSecret...)
	body = append(body, 0x00, byte(len(payload)))
	body = append(body, payload...)
	return helloExtension(extensionTypeEncryptedClientHello, body)
}

func TestParserFragmentedTLS12ClientHello(t *testing.T) {
	record := fixtureRecord(t, "tls12-clienthello.hex")
	message := record[recordHeaderSize:]
	nameOffset := bytes.Index(message, []byte(expectedSNI))
	if nameOffset < 0 {
		t.Fatalf("fixture does not contain %q", expectedSNI)
	}
	// Cut one byte before the host name so the 16-bit host_name length is
	// torn across the record boundary — the sharpest fragmentation case.
	split := nameOffset - 1
	input := fragmentedHandshakeRecords([2]byte{3, 1}, message, split)

	hello, err := peekInput(t, input, ReadTimeout, false)
	if err != nil {
		t.Fatalf("peek fragmented TLS 1.2 hello: %v", err)
	}
	if hello.SNI != expectedSNI {
		t.Fatalf("SNI = %q, want %q", hello.SNI, expectedSNI)
	}
	if !bytes.Equal(hello.Prefix, input) {
		t.Fatalf("prefix length %d does not match inspected input length %d", len(hello.Prefix), len(input))
	}

	t.Run("normalizes mixed case and trailing dot", func(t *testing.T) {
		wireName := "R7K2M9P4X6.V7Q4KM2X9PZ6DN3W.Sharebridgeusercontent.COM."
		message := buildClientHelloMessage([2]byte{3, 3}, nil, cipherSuitesBody(defaultCipherSuites), []byte{0}, serverNameExtension(wireName))
		nameOffset := bytes.Index(message, []byte(wireName))
		if nameOffset < 0 {
			t.Fatalf("built hello does not contain %q", wireName)
		}
		input := fragmentedHandshakeRecords([2]byte{3, 1}, message, nameOffset+10)

		hello, err := peekInput(t, input, ReadTimeout, false)
		if err != nil {
			t.Fatalf("peek: %v", err)
		}
		if hello.SNI != expectedSNI {
			t.Fatalf("normalized SNI = %q, want %q", hello.SNI, expectedSNI)
		}
	})
}

func TestParserFragmentedTLS13ClientHello(t *testing.T) {
	record := fixtureRecord(t, "tls13-clienthello.hex")
	message := record[recordHeaderSize:]
	nameOffset := bytes.Index(message, []byte(expectedSNI))
	keyShareOffset := bytes.Index(message, []byte(keyShareVector))
	if nameOffset < 0 || keyShareOffset < 0 {
		t.Fatalf("fixture does not contain the expected SNI and key share vectors")
	}
	if keyShareOffset <= nameOffset {
		t.Fatalf("fixture layout changed: key share at %d is not after the name at %d", keyShareOffset, nameOffset)
	}
	// Three records: one cut through the host_name length field, one in the
	// middle of the key share data.
	input := fragmentedHandshakeRecords([2]byte{3, 1}, message, nameOffset-1, keyShareOffset+10)

	hello, err := peekInput(t, input, ReadTimeout, false)
	if err != nil {
		t.Fatalf("peek fragmented TLS 1.3 hello: %v", err)
	}
	if hello.SNI != expectedSNI {
		t.Fatalf("SNI = %q, want %q", hello.SNI, expectedSNI)
	}
	if !bytes.Equal(hello.Prefix, input) {
		t.Fatalf("prefix length %d does not match inspected input length %d", len(hello.Prefix), len(input))
	}
}

func TestParserMultiRecordHello(t *testing.T) {
	record := fixtureRecord(t, "tls12-clienthello.hex")
	message := record[recordHeaderSize:]
	nameOffset := bytes.Index(message, []byte(expectedSNI))
	if nameOffset < 0 {
		t.Fatalf("fixture does not contain %q", expectedSNI)
	}
	// Six fragments across six records: a header-only first record (zero
	// fragment), a one-byte fragment, assorted bodies, a cut mid-hostname,
	// and a three-byte tail.
	splits := []int{handshakeHeaderSize, handshakeHeaderSize + 1, 64, nameOffset + 5, len(message) - 3}
	input := fragmentedHandshakeRecords([2]byte{3, 1}, message, splits...)

	hello, err := peekInput(t, input, ReadTimeout, false)
	if err != nil {
		t.Fatalf("peek multi-record hello: %v", err)
	}
	if hello.SNI != expectedSNI {
		t.Fatalf("SNI = %q, want %q", hello.SNI, expectedSNI)
	}
	if !bytes.Equal(hello.Prefix, input) {
		t.Fatalf("prefix of %d bytes does not equal the %d inspected input bytes", len(hello.Prefix), len(input))
	}
}

func TestParserReplaysInspectedPrefixExactly(t *testing.T) {
	tls12Record := fixtureRecord(t, "tls12-clienthello.hex")
	tls13Record := fixtureRecord(t, "tls13-clienthello.hex")
	tls13Message := tls13Record[recordHeaderSize:]
	tls13NameOffset := bytes.Index(tls13Message, []byte(expectedSNI))
	if tls13NameOffset < 0 {
		t.Fatalf("TLS 1.3 fixture does not contain %q", expectedSNI)
	}

	cases := []struct {
		name  string
		input []byte
	}{
		{"single record TLS 1.2", tls12Record},
		{"single record TLS 1.3", tls13Record},
		{"three record TLS 1.3", fragmentedHandshakeRecords([2]byte{3, 1}, tls13Message, tls13NameOffset-1)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			first, err := peekInput(t, testCase.input, ReadTimeout, false)
			if err != nil {
				t.Fatalf("peek: %v", err)
			}
			second, err := peekInput(t, testCase.input, ReadTimeout, false)
			if err != nil {
				t.Fatalf("second peek: %v", err)
			}
			// The prefix must be an owned copy: a second parse may reuse the
			// pooled scratch buffer without corrupting the first result.
			if !bytes.Equal(first.Prefix, testCase.input) {
				t.Fatalf("first prefix of %d bytes does not equal the %d input bytes", len(first.Prefix), len(testCase.input))
			}
			if !bytes.Equal(second.Prefix, testCase.input) {
				t.Fatalf("second prefix of %d bytes does not equal the %d input bytes", len(second.Prefix), len(testCase.input))
			}
			if &first.Prefix[0] == &second.Prefix[0] {
				t.Fatalf("both parses share one backing array; prefix is not owned")
			}
		})
	}
}

func TestParserRejectsMissingSNI(t *testing.T) {
	syntheticMessage := func(extensionBlock []byte) []byte {
		return buildClientHelloMessage([2]byte{3, 3}, testSessionID, cipherSuitesBody(defaultCipherSuites), []byte{0}, extensionBlock)
	}
	wrap := func(message []byte) []byte {
		return tlsRecord(contentTypeHandshake, [2]byte{3, 1}, message)
	}

	cases := []struct {
		name  string
		input []byte
	}{
		{"no extension block", wrap(syntheticMessage(nil))},
		{"extensions without server_name", wrap(syntheticMessage(helloExtension(0x000a, []byte{0x00, 0x04, 0x00, 0x1d, 0x00, 0x17})))},
		{"empty server_name list", wrap(syntheticMessage(helloExtension(extensionTypeServerName, []byte{0x00, 0x00})))},
		{"zero length host_name", wrap(syntheticMessage(helloExtension(extensionTypeServerName, serverNameList(hostNameEntry(serverNameTypeHost, "")))))},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := peekInput(t, testCase.input, ReadTimeout, false)
			if !errors.Is(err, ErrSNIMissing) {
				t.Fatalf("peek error = %v, want %v", err, ErrSNIMissing)
			}
		})
	}
}

func TestParserRejectsECHHiddenSNI(t *testing.T) {
	syntheticMessage := func(extensionBlock []byte) []byte {
		return buildClientHelloMessage([2]byte{3, 3}, testSessionID, cipherSuitesBody(defaultCipherSuites), []byte{0}, extensionBlock)
	}
	wrap := func(message []byte) []byte {
		return tlsRecord(contentTypeHandshake, [2]byte{3, 1}, message)
	}

	cases := []struct {
		name          string
		extensionBody []byte
		wantErr       error
	}{
		{"ech hides the server name", echExtension(), ErrSNIHidden},
		{"ech with empty server_name list", concat(echExtension(), helloExtension(extensionTypeServerName, []byte{0x00, 0x00})), ErrSNIHidden},
		{"grease ech with visible server name", concat(serverNameExtension(expectedSNI), echExtension()), nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := peekInput(t, wrap(syntheticMessage(testCase.extensionBody)), ReadTimeout, false)
			if testCase.wantErr == nil {
				if err != nil {
					t.Fatalf("peek error = %v, want success", err)
				}
				return
			}
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("peek error = %v, want %v", err, testCase.wantErr)
			}
		})
	}
}

func TestParserRejectsMalformedHello(t *testing.T) {
	syntheticMessage := func(extensionBlock []byte) []byte {
		return buildClientHelloMessage([2]byte{3, 3}, testSessionID, cipherSuitesBody(defaultCipherSuites), []byte{0}, extensionBlock)
	}
	wrap := func(message []byte) []byte {
		return tlsRecord(contentTypeHandshake, [2]byte{3, 1}, message)
	}

	serverName := syntheticMessage(serverNameExtension(expectedSNI))

	// A server_name list whose declared length exceeds its extension body.
	inconsistentList := hostNameEntry(serverNameTypeHost, expectedSNI)
	inconsistentList = append([]byte{byte((len(inconsistentList) + 1) >> 8), byte(len(inconsistentList) + 1)}, inconsistentList...)

	// Message field offsets with the synthetic hello layout above.
	sessionIDLengthOffset := handshakeHeaderSize + 2 + randomSize
	cipherSuitesLengthOffset := sessionIDLengthOffset + 1 + len(testSessionID)
	extensionLengthLowOffset := cipherSuitesLengthOffset + 2 + 2*len(defaultCipherSuites) + 1 + 1

	trailingAfterHello := bytes.Clone(serverName)
	declaredLength := len(trailingAfterHello) - handshakeHeaderSize - 10
	trailingAfterHello[1] = byte(declaredLength >> 16)
	trailingAfterHello[2] = byte(declaredLength >> 8)
	trailingAfterHello[3] = byte(declaredLength)

	oddCipherSuites := syntheticMessage(nil)
	oddCipherSuites[cipherSuitesLengthOffset+1] = 0x03 // odd body length

	cases := []struct {
		name            string
		input           []byte
		wantErr         error
		closeAfterWrite bool
	}{
		{"application data instead of handshake", tlsRecord(0x17, [2]byte{3, 1}, serverName), ErrMalformed, false},
		{"change cipher spec instead of handshake", tlsRecord(0x14, [2]byte{3, 1}, serverName), ErrMalformed, false},
		{"ssl2 record version", tlsRecord(contentTypeHandshake, [2]byte{2, 0}, serverName), ErrMalformed, false},
		{"unknown record version 3.4", tlsRecord(contentTypeHandshake, [2]byte{3, 4}, serverName), ErrMalformed, false},
		{"record body over tls maximum", tlsRecord(contentTypeHandshake, [2]byte{3, 1}, bytes.Repeat([]byte{0x01}, maxRecordBody+1)), ErrMalformed, false},
		{"first record shorter than handshake header", tlsRecord(contentTypeHandshake, [2]byte{3, 1}, []byte{0x01, 0x00}), ErrMalformed, false},
		{"server hello instead of client hello", func() []byte {
			message := bytes.Clone(serverName)
			message[0] = 0x02
			return wrap(message)
		}(), ErrMalformed, false},
		{"zero handshake length", func() []byte {
			message := bytes.Clone(serverName)
			message[1], message[2], message[3] = 0, 0, 0
			return wrap(message)
		}(), ErrMalformed, false},
		{"ssl2 client version", func() []byte {
			message := bytes.Clone(serverName)
			message[handshakeHeaderSize] = 2
			return wrap(message)
		}(), ErrMalformed, false},
		{"session_id length overruns hello", func() []byte {
			message := bytes.Clone(serverName)
			message[sessionIDLengthOffset] = 0xFF
			return wrap(message)
		}(), ErrMalformed, false},
		{"cipher_suites length overruns hello", func() []byte {
			message := bytes.Clone(serverName)
			message[cipherSuitesLengthOffset], message[cipherSuitesLengthOffset+1] = 0xFF, 0xFF
			return wrap(message)
		}(), ErrMalformed, false},
		{"odd cipher_suites length", oddCipherSuites, ErrMalformed, false},
		{"compression_methods empty", wrap(buildClientHelloMessage([2]byte{3, 3}, testSessionID, cipherSuitesBody(defaultCipherSuites), nil, serverNameExtension(expectedSNI))), ErrMalformed, false},
		{"extensions length leaves trailing bytes", func() []byte {
			message := bytes.Clone(serverName)
			message[extensionLengthLowOffset] -= 2
			return wrap(message)
		}(), ErrMalformed, false},
		{"extensions length overruns hello", func() []byte {
			message := bytes.Clone(serverName)
			message[extensionLengthLowOffset] += 2
			return wrap(message)
		}(), ErrMalformed, false},
		{"truncated extension header", wrap(syntheticMessage(append(serverNameExtension(expectedSNI), 0x00, 0x00, 0x00))), ErrMalformed, false},
		{"extension body overruns extension block", wrap(syntheticMessage([]byte{0x00, 0x66, 0xFF, 0xFF})), ErrMalformed, false},
		{"duplicate server_name extension", wrap(syntheticMessage(concat(serverNameExtension(expectedSNI), serverNameExtension(expectedSNI)))), ErrMalformed, false},
		{"server_name list length inconsistent", wrap(syntheticMessage(helloExtension(extensionTypeServerName, inconsistentList))), ErrMalformed, false},
		{"unsupported server name type", wrap(syntheticMessage(helloExtension(extensionTypeServerName, serverNameList(hostNameEntry(0x01, expectedSNI))))), ErrMalformed, false},
		{"multiple host_name entries", wrap(syntheticMessage(helloExtension(extensionTypeServerName, serverNameList(hostNameEntry(serverNameTypeHost, expectedSNI), hostNameEntry(serverNameTypeHost, "other.example.com"))))), ErrMalformed, false},
		{"trailing bytes after hello message", wrap(trailingAfterHello), ErrMalformed, false},
		{"underscore in server name", wrap(syntheticMessage(serverNameExtension("bad_host.example.com"))), ErrMalformed, false},
		{"non-ascii byte in server name", wrap(syntheticMessage(serverNameExtension("r7k2\xffbad.example.com"))), ErrMalformed, false},
		{"overlong server name", wrap(syntheticMessage(serverNameExtension(strings.Repeat("a", 254)))), ErrMalformed, false},
		{"connection closed mid-hello", fixtureRecord(t, "tls12-clienthello.hex")[:20], ErrMalformed, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := peekInput(t, testCase.input, ReadTimeout, testCase.closeAfterWrite)
			if !errors.Is(err, testCase.wantErr) {
				t.Fatalf("peek error = %v, want %v", err, testCase.wantErr)
			}
		})
	}
}

func TestParserRejectsOversize(t *testing.T) {
	t.Run("declared handshake length over budget", func(t *testing.T) {
		input := tlsRecord(contentTypeHandshake, [2]byte{3, 1}, []byte{handshakeTypeClientHello, 0x02, 0x00, 0x00, 0xDE, 0xAD})
		_, err := peekInput(t, input, ReadTimeout, false)
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("peek error = %v, want %v", err, ErrTooLarge)
		}
	})

	t.Run("inspection budget exhausted mid-hello", func(t *testing.T) {
		// A structurally valid hello padded to 65520 bytes with the RFC 7685
		// padding extension: it passes the declared-length check but the
		// record framing overhead pushes the inspected prefix past 64 KiB.
		const declaredMessageLength = 65520
		suites := cipherSuitesBody(defaultCipherSuites)
		overhead := handshakeHeaderSize + 2 + randomSize + 1 + 0 + 2 + len(suites) + 1 + 1 + 2 + 4
		paddingExtension := helloExtension(0x0015, bytes.Repeat([]byte{0x00}, declaredMessageLength-overhead))
		message := buildClientHelloMessage([2]byte{3, 3}, nil, suites, []byte{0}, paddingExtension)
		if len(message) != declaredMessageLength {
			t.Fatalf("built hello is %d bytes, want %d", len(message), declaredMessageLength)
		}
		input := fragmentedHandshakeRecords([2]byte{3, 1}, message, maxRecordBody, 2*maxRecordBody, 3*maxRecordBody)

		_, err := peekInput(t, input, ReadTimeout, false)
		if !errors.Is(err, ErrTooLarge) {
			t.Fatalf("peek error = %v, want %v", err, ErrTooLarge)
		}
	})
}

func TestParserTimesOut(t *testing.T) {
	t.Run("spec bounds", func(t *testing.T) {
		if ReadTimeout != 5*time.Second {
			t.Fatalf("ReadTimeout = %v, want 5s (spec §14)", ReadTimeout)
		}
		if MaxBufferedBytes != 64<<10 {
			t.Fatalf("MaxBufferedBytes = %d, want %d (spec §14)", MaxBufferedBytes, 64<<10)
		}
	})

	t.Run("silence times out", func(t *testing.T) {
		_, err := peekInput(t, nil, 50*time.Millisecond, false)
		if !errors.Is(err, ErrTimeout) || !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("peek error = %v, want %v", err, ErrTimeout)
		}
	})

	t.Run("stalled mid-hello times out", func(t *testing.T) {
		partial := fixtureRecord(t, "tls12-clienthello.hex")[:7]
		_, err := peekInput(t, partial, 50*time.Millisecond, false)
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("peek error = %v, want %v", err, ErrTimeout)
		}
	})
}

func TestParserAcceptsGoTLSClientHello(t *testing.T) {
	// Cross-check the parser against a real ClientHello produced by Go's
	// crypto/tls stack, received without any TLS termination.
	gatewaySide, clientSide := net.Pipe()
	t.Cleanup(func() {
		gatewaySide.Close()
		clientSide.Close()
	})
	tlsConn := tls.Client(clientSide, &tls.Config{ServerName: expectedSNI, InsecureSkipVerify: true})
	handshakeDone := make(chan error, 1)
	go func() {
		handshakeDone <- tlsConn.Handshake()
	}()

	hello, err := peek(gatewaySide, ReadTimeout)
	if err != nil {
		t.Fatalf("peek Go TLS hello: %v", err)
	}
	if hello.SNI != expectedSNI {
		t.Fatalf("SNI = %q, want %q", hello.SNI, expectedSNI)
	}
	if hello.Prefix[0] != contentTypeHandshake || hello.Prefix[5] != handshakeTypeClientHello {
		t.Fatalf("prefix does not start with a ClientHello record: %#x", hello.Prefix[:recordHeaderSize+1])
	}
	// The handshake goroutine stays blocked awaiting a ServerHello; cleanup
	// closes the pipe and the buffered channel absorbs the result.
	_ = handshakeDone
}
