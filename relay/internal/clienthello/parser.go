// Package clienthello extracts the exact server name from a TLS ClientHello
// without terminating TLS (spec §4.4, §8, §14, §16.1).
//
// The relay gateway is Layer 4 only: it peeks at the first bytes of a public
// connection to route by exact hostname, then replays those bytes untouched
// to the selected agent. Peek reads only the bounded ClientHello prefix,
// never invokes a TLS server stack, and fails closed on missing or ECH-hidden
// server names, malformed structure, oversized input, and read timeouts.
package clienthello

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// Bounds from spec §14 ("Resource Bounds and Operational Limits").
const (
	// MaxBufferedBytes caps the inspected prefix at 64 KiB. A ClientHello
	// that does not complete within this budget is rejected fail-closed.
	MaxBufferedBytes = 64 << 10
	// ReadTimeout is the overall ClientHello read deadline.
	ReadTimeout = 5 * time.Second
)

// Wire constants for the TLS record and handshake layers (RFC 8446 §5, §4).
const (
	contentTypeHandshake = 0x16

	maxRecordBody            = 1 << 14 // TLSPlaintext length MUST NOT exceed 2^14
	recordHeaderSize         = 5
	handshakeTypeClientHello = 0x01
	handshakeHeaderSize      = 4
	randomSize               = 32

	extensionTypeServerName           = 0x0000
	extensionTypeEncryptedClientHello = 0xFE0D
	serverNameTypeHost                = 0x00
)

// Failure classes. The gateway closes the connection generically on any of
// them (spec §8); the distinctions exist for logs and tests.
var (
	// ErrMalformed covers structurally invalid or truncated input.
	ErrMalformed = errors.New("clienthello: malformed ClientHello")
	// ErrSNIMissing covers hellos that carry no usable server name.
	ErrSNIMissing = errors.New("clienthello: no server name present")
	// ErrSNIHidden covers ECH outer hellos that hide the server name.
	ErrSNIHidden = errors.New("clienthello: server name hidden by encrypted client hello")
	// ErrTooLarge covers input exceeding the 64 KiB inspection budget.
	ErrTooLarge = errors.New("clienthello: ClientHello exceeds 64 KiB inspection budget")
	// ErrTimeout covers the 5-second read deadline expiring.
	ErrTimeout = errors.New("clienthello: ClientHello read timed out")
	// ErrInvalidBound covers a PeekWithBounds request whose byte budget lies
	// outside the parser's supported 1..MaxBufferedBytes range, or whose read
	// deadline is not positive. The §14 limits configuration rejects such
	// values at startup; the parser refuses them too rather than silently
	// falling back to a default (defense in depth).
	ErrInvalidBound = errors.New("clienthello: invalid inspection bound")
)

// Hello is a parsed ClientHello peek. Prefix is an owned copy of every byte
// the parser inspected, in stream order; the caller must replay it to the
// upstream agent before copying the remainder of the stream (spec §8, §16.1).
type Hello struct {
	SNI    string
	Prefix []byte
}

// scratchPool hands out one fixed-capacity array per parse: the first half
// accumulates the raw inspected prefix (replayed verbatim), the second half
// the defragmented handshake message (parsed, never leaves the process).
// Buffers are zeroed before reuse so no inspected bytes linger in pooled
// memory (spec §14: pooled and bounded buffers).
var scratchPool = sync.Pool{
	New: func() any {
		return new([2 * MaxBufferedBytes]byte)
	},
}

// Peek reads the ClientHello from conn within the spec §14 defaults (64 KiB
// inspected, 5 seconds overall) and returns the normalized exact SNI plus
// the inspected prefix.
//
// Peek never performs a TLS handshake and never writes to conn. On success
// it clears the read deadline so the caller can continue serving the
// connection. On failure the caller should close conn; Peek leaves unread
// bytes beyond the inspected prefix untouched.
func Peek(conn net.Conn) (*Hello, error) {
	return peek(conn, ReadTimeout)
}

// PeekWithBounds is Peek with operator-selected §14 bounds: it inspects at
// most maxBytes (1..MaxBufferedBytes) within at most readTimeout. The
// configured §14 limits drive the gateway through this entry point, so a
// lowered hello byte budget or read deadline is a real, enforced bound rather
// than an inert configuration field. A non-positive or over-budget value
// fails closed with ErrInvalidBound instead of silently using the default.
func PeekWithBounds(conn net.Conn, maxBytes int, readTimeout time.Duration) (*Hello, error) {
	if maxBytes <= 0 || maxBytes > MaxBufferedBytes {
		return nil, fmt.Errorf("clienthello: inspection budget %d outside 1..%d: %w", maxBytes, MaxBufferedBytes, ErrInvalidBound)
	}
	if readTimeout <= 0 {
		return nil, fmt.Errorf("clienthello: read deadline %v is not positive: %w", readTimeout, ErrInvalidBound)
	}
	return peekWithBounds(conn, maxBytes, readTimeout)
}

// peek runs the parser at the default §14 budget.
func peek(conn net.Conn, readTimeout time.Duration) (*Hello, error) {
	return peekWithBounds(conn, MaxBufferedBytes, readTimeout)
}

// peekWithBounds is the shared parse body. maxBytes is the inspected-prefix
// ceiling and drives the message budget proportionally, so a smaller operator
// budget cannot be circumvented by a large declared handshake length.
func peekWithBounds(conn net.Conn, maxBytes int, readTimeout time.Duration) (*Hello, error) {
	if err := conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
		return nil, fmt.Errorf("clienthello: set read deadline: %w", err)
	}
	defer func() {
		_ = conn.SetReadDeadline(time.Time{})
	}()

	scratch := scratchPool.Get().(*[2 * MaxBufferedBytes]byte)
	prefix := scratch[:0:maxBytes]
	message := scratch[MaxBufferedBytes : MaxBufferedBytes : 2*MaxBufferedBytes]
	maxMessage := maxBytes - recordHeaderSize + handshakeHeaderSize
	inspected, reassembled, err := readClientHello(conn, prefix, message, maxBytes, maxMessage)
	var hello *Hello
	if err == nil {
		hello, err = assembleHello(inspected, reassembled)
	}
	clear(scratch[:])
	scratchPool.Put(scratch)
	return hello, err
}

// assembleHello parses the defragmented message and copies the inspected
// prefix into an owned slice so the pooled scratch can be recycled at once.
func assembleHello(inspected, message []byte) (*Hello, error) {
	serverName, err := parseClientHello(message)
	if err != nil {
		return nil, err
	}
	prefix := make([]byte, len(inspected))
	copy(prefix, inspected)
	return &Hello{SNI: serverName, Prefix: prefix}, nil
}

// readClientHello reads whole TLS records from conn until the first handshake
// message — which must be a ClientHello — is complete. Every byte read is
// appended to prefix verbatim (headers included) so the caller can replay the
// exact stream; the message fragments are reassembled in order into message,
// stripping the per-record framing. The handshake header travels with the
// first record; later records contribute their full body. maxBytes bounds the
// inspected prefix and maxMessage the reassembled message, so the configured
// §14 budget is enforced at the read, before any large allocation.
func readClientHello(conn io.Reader, prefix, message []byte, maxBytes, maxMessage int) ([]byte, []byte, error) {
	messageLength := -1 // handshake message length from the header, once seen
	messageBytes := 0   // handshake message bytes received so far
	for {
		var err error
		prefix, err = readInto(conn, prefix, recordHeaderSize, maxBytes)
		if err != nil {
			return nil, nil, err
		}
		header := prefix[len(prefix)-recordHeaderSize:]
		if header[0] != contentTypeHandshake {
			return nil, nil, fmt.Errorf("clienthello: unexpected record type %#02x: %w", header[0], ErrMalformed)
		}
		if header[1] != 3 || header[2] > 3 {
			return nil, nil, fmt.Errorf("clienthello: unsupported record version %d.%d: %w", header[1], header[2], ErrMalformed)
		}
		recordLength := int(header[3])<<8 | int(header[4])
		if recordLength > maxRecordBody {
			return nil, nil, fmt.Errorf("clienthello: record body of %d bytes exceeds the TLS maximum of %d: %w", recordLength, maxRecordBody, ErrMalformed)
		}
		bodyStart := len(prefix)
		prefix, err = readInto(conn, prefix, recordLength, maxBytes)
		if err != nil {
			return nil, nil, err
		}
		body := prefix[bodyStart:]
		if messageLength < 0 {
			if len(body) < handshakeHeaderSize {
				return nil, nil, fmt.Errorf("clienthello: first record does not carry the handshake header: %w", ErrMalformed)
			}
			if body[0] != handshakeTypeClientHello {
				return nil, nil, fmt.Errorf("clienthello: unexpected handshake message type %#02x: %w", body[0], ErrMalformed)
			}
			messageLength = int(body[1])<<16 | int(body[2])<<8 | int(body[3])
			if messageLength == 0 {
				return nil, nil, fmt.Errorf("clienthello: empty handshake message: %w", ErrMalformed)
			}
			// Reject oversized declarations before reading toward them so a
			// hostile length can never drive allocation (spec §14).
			if messageLength > maxMessage-handshakeHeaderSize {
				return nil, nil, fmt.Errorf("clienthello: handshake message of %d bytes exceeds the %d byte inspection budget: %w", messageLength, maxMessage-handshakeHeaderSize, ErrTooLarge)
			}
			messageBytes = len(body) - handshakeHeaderSize
		} else {
			messageBytes += len(body)
		}
		if message, err = appendMessage(message, body, maxMessage); err != nil {
			return nil, nil, err
		}
		if messageBytes == messageLength {
			return prefix, message, nil
		}
		if messageBytes > messageLength {
			// Bytes beyond the declared hello end have no legitimate
			// identity before the first ServerHello; fail closed on them.
			return nil, nil, fmt.Errorf("clienthello: %d trailing bytes after the ClientHello message: %w", messageBytes-messageLength, ErrMalformed)
		}
	}
}

// appendMessage appends reassembled message bytes, refusing to grow past the
// fixed message budget. The capacity check keeps the pooled scratch array
// from ever being reallocated.
func appendMessage(message []byte, fragment []byte, maxMessage int) ([]byte, error) {
	if len(message)+len(fragment) > maxMessage || len(message)+len(fragment) > cap(message) {
		return nil, ErrTooLarge
	}
	return append(message, fragment...), nil
}

// readInto appends want more bytes from conn to inspected, refusing to grow
// past the configured inspection budget. The capacity check keeps a pooled
// buffer from ever being reallocated.
func readInto(conn io.Reader, inspected []byte, want, maxBytes int) ([]byte, error) {
	if want == 0 {
		return inspected, nil
	}
	if len(inspected)+want > maxBytes || len(inspected)+want > cap(inspected) {
		return nil, ErrTooLarge
	}
	target := inspected[:len(inspected)+want]
	if _, err := io.ReadFull(conn, target[len(inspected):]); err != nil {
		return nil, mapReadError(err)
	}
	return target, nil
}

func mapReadError(err error) error {
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return fmt.Errorf("clienthello: %w (%w)", ErrTimeout, os.ErrDeadlineExceeded)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return fmt.Errorf("clienthello: connection closed mid-hello: %w", ErrMalformed)
	default:
		return fmt.Errorf("clienthello: read: %w", err)
	}
}

// parseClientHello walks the reassembled handshake message and returns the
// normalized server name. Every structural inconsistency is an error rather
// than a guess, so the gateway never routes on ambiguous input (spec §8).
func parseClientHello(message []byte) (string, error) {
	hello := &helloReader{message: message}
	if err := hello.skip(handshakeHeaderSize, "handshake header"); err != nil {
		return "", err
	}
	clientVersion, err := hello.take(2, "client version")
	if err != nil {
		return "", err
	}
	if clientVersion[0] != 3 || clientVersion[1] > 3 {
		return "", fmt.Errorf("clienthello: unsupported client version %d.%d: %w", clientVersion[0], clientVersion[1], ErrMalformed)
	}
	if err := hello.skip(randomSize, "random"); err != nil {
		return "", err
	}
	if _, err := hello.readVector(1, "session_id"); err != nil {
		return "", err
	}
	cipherSuites, err := hello.readVector(2, "cipher_suites")
	if err != nil {
		return "", err
	}
	if len(cipherSuites) < 2 || len(cipherSuites)%2 != 0 {
		return "", fmt.Errorf("clienthello: cipher_suites vector of %d bytes is not a suite list: %w", len(cipherSuites), ErrMalformed)
	}
	compressionMethods, err := hello.readVector(1, "compression_methods")
	if err != nil {
		return "", err
	}
	if len(compressionMethods) < 1 {
		return "", fmt.Errorf("clienthello: compression_methods vector is empty: %w", ErrMalformed)
	}
	if hello.remaining() == 0 {
		return "", fmt.Errorf("clienthello: hello carries no extensions: %w", ErrSNIMissing)
	}
	extensions, err := hello.readVector(2, "extensions")
	if err != nil {
		return "", err
	}
	if hello.remaining() != 0 {
		return "", fmt.Errorf("clienthello: %d bytes after the extension block: %w", hello.remaining(), ErrMalformed)
	}

	var serverName string
	var haveServerName, haveEncryptedClientHello bool
	extensionReader := &helloReader{message: extensions}
	for extensionReader.remaining() > 0 {
		extensionType, err := extensionReader.readUint16("extension type")
		if err != nil {
			return "", err
		}
		extensionLength, err := extensionReader.readUint16("extension length")
		if err != nil {
			return "", err
		}
		extensionBody, err := extensionReader.take(extensionLength, "extension body")
		if err != nil {
			return "", err
		}
		switch extensionType {
		case extensionTypeServerName:
			if haveServerName {
				return "", fmt.Errorf("clienthello: duplicate server_name extension: %w", ErrMalformed)
			}
			haveServerName = true
			name, err := parseServerName(extensionBody)
			if err != nil {
				return "", err
			}
			serverName = name
		case extensionTypeEncryptedClientHello:
			if haveEncryptedClientHello {
				return "", fmt.Errorf("clienthello: duplicate encrypted_client_hello extension: %w", ErrMalformed)
			}
			haveEncryptedClientHello = true
		}
	}
	switch {
	case serverName != "":
		// An encrypted_client_hello extension alongside a visible server
		// name is the browser GREASE shape: the outer name is the real
		// public name and the agent's TLS stack arbitrates the rest.
		return serverName, nil
	case haveEncryptedClientHello:
		// No visible name and an ECH extension hides the real hostname;
		// the gateway must not guess it (spec §8).
		return "", ErrSNIHidden
	default:
		return "", ErrSNIMissing
	}
}

// parseServerName extracts the single host_name entry RFC 6066 §3 defines.
// Other name types, extra entries, and inconsistent lengths fail closed.
func parseServerName(body []byte) (string, error) {
	nameListReader := &helloReader{message: body}
	nameList, err := nameListReader.readVector(2, "server_name list")
	if err != nil {
		return "", err
	}
	entryReader := &helloReader{message: nameList}
	var host []byte
	for entryReader.remaining() > 0 {
		nameType, err := entryReader.readUint8("server name type")
		if err != nil {
			return "", err
		}
		hostName, err := entryReader.readVector(2, "host_name")
		if err != nil {
			return "", err
		}
		if nameType != serverNameTypeHost {
			return "", fmt.Errorf("clienthello: unsupported server name type %d: %w", nameType, ErrMalformed)
		}
		if host != nil {
			return "", fmt.Errorf("clienthello: multiple host_name entries: %w", ErrMalformed)
		}
		host = hostName
	}
	if len(host) == 0 {
		// Structurally valid, but no usable name: the caller decides between
		// a plainly missing name and one hidden by encrypted_client_hello.
		return "", nil
	}
	return normalizeServerName(host)
}

// normalizeServerName lowercases the non-empty wire host name, drops one
// trailing dot, and accepts only the lowercase DNS hostname character set
// used across ShareBridge (mirroring agent/internal/direct normalization).
func normalizeServerName(host []byte) (string, error) {
	name := strings.ToLower(string(host))
	name = strings.TrimSuffix(name, ".")
	if !validHostname(name) {
		return "", fmt.Errorf("clienthello: server name is not a valid hostname (%d bytes): %w", len(name), ErrMalformed)
	}
	return name, nil
}

// validHostname accepts only lowercase DNS labels (letters, digits, hyphen,
// dot) up to 253 bytes. Exact route matching downstream gates everything
// beyond this character set.
func validHostname(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for offset := 0; offset < len(name); offset++ {
		character := name[offset]
		switch {
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9', character == '-', character == '.':
		default:
			return false
		}
	}
	return true
}

// helloReader walks a parsed structure with explicit bounds checks. Every
// shortfall is reported as malformed instead of panicking or guessing.
type helloReader struct {
	message []byte
	offset  int
}

func (r *helloReader) remaining() int {
	return len(r.message) - r.offset
}

func (r *helloReader) need(count int, what string) error {
	if r.remaining() < count {
		return fmt.Errorf("clienthello: truncated %s: %w", what, ErrMalformed)
	}
	return nil
}

func (r *helloReader) skip(count int, what string) error {
	if err := r.need(count, what); err != nil {
		return err
	}
	r.offset += count
	return nil
}

func (r *helloReader) take(count int, what string) ([]byte, error) {
	if err := r.need(count, what); err != nil {
		return nil, err
	}
	taken := r.message[r.offset : r.offset+count]
	r.offset += count
	return taken, nil
}

func (r *helloReader) readUint8(what string) (int, error) {
	raw, err := r.take(1, what)
	if err != nil {
		return 0, err
	}
	return int(raw[0]), nil
}

func (r *helloReader) readUint16(what string) (int, error) {
	raw, err := r.take(2, what)
	if err != nil {
		return 0, err
	}
	return int(raw[0])<<8 | int(raw[1]), nil
}

// readVector reads a length-prefixed vector whose length field is
// lengthFieldBytes wide and returns the vector contents.
func (r *helloReader) readVector(lengthFieldBytes int, what string) ([]byte, error) {
	vectorLength, err := r.readUintN(lengthFieldBytes, what+" length")
	if err != nil {
		return nil, err
	}
	return r.take(vectorLength, what)
}

func (r *helloReader) readUintN(count int, what string) (int, error) {
	raw, err := r.take(count, what)
	if err != nil {
		return 0, err
	}
	value := 0
	for _, octet := range raw {
		value = value<<8 | int(octet)
	}
	return value, nil
}
