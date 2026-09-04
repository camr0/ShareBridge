// Command gen regenerates the deterministic ClientHello record fixtures used
// by parser_test.go. Run it from the relay module root:
//
//	go run internal/clienthello/testdata/gen.go internal/clienthello/testdata
//
// Each fixture is a single realistic TLS handshake record modeled on a modern
// browser ClientHello (Chrome-shaped extension set). parser_test.go loads the
// hex files and fragments the handshake message across records to exercise
// the parser's fragmented and multi-record paths.
package main

import (
	"encoding/hex"
	"os"
	"path/filepath"
)

// relayOrigin matches the test origin used across ShareBridge agent tests.
const relayOrigin = "r7k2m9p4x6.v7q4km2x9pz6dn3w.sharebridgeusercontent.com"

func main() {
	if len(os.Args) != 2 {
		panic("usage: gen <output-dir>")
	}
	writeFixture(os.Args[1], "tls12-clienthello.hex", tls12Record())
	writeFixture(os.Args[1], "tls13-clienthello.hex", tls13Record())
}

func writeFixture(dir, name string, record []byte) {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(hex.EncodeToString(record)+"\n"), 0o644); err != nil {
		panic(err)
	}
}

// tls12Record builds a classic TLS 1.2 ClientHello: client_version 3.3, no
// supported_versions/key_share, and a TLS 1.2 extension set.
func tls12Record() []byte {
	extensions := concat(
		serverNameExtension(relayOrigin),
		extension(0xff01, []byte{0x00}), // renegotiation_info
		extension(0x0017, nil),          // extended_master_secret
		extension(0x000a, []byte{0x00, 0x06, 0x00, 0x1d, 0x00, 0x17, 0x00, 0x18}), // supported_groups
		extension(0x000b, []byte{0x01, 0x00}),                                     // ec_point_formats
		extension(0x0023, nil),                                                    // session_ticket
		extension(0x0016, nil),                                                    // encrypt_then_mac
		extension(0x000d, u16vec(0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601)), // signature_algorithms
		extension(0x0010, alpn("h2", "http/1.1")),
	)
	return handshakeRecord(
		[2]byte{3, 3},
		asciiVector("sharebridge-tls12-random-vector0"),
		[]uint16{0x1301, 0x1302, 0x1303, 0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9, 0xcca8, 0x00ff, 0x009c, 0x009d, 0xc027, 0xc013},
		asciiVector("sharebridge-tls12-session-id000"),
		extensions,
	)
}

// tls13Record builds a TLS 1.3 ClientHello: legacy client_version 3.3 with
// supported_versions 1.3, key_share, PSK modes, and a GREASE-shaped
// encrypted_client_hello extension alongside a visible server_name.
func tls13Record() []byte {
	extensions := concat(
		serverNameExtension(relayOrigin),
		extension(0x000a, []byte{0x00, 0x04, 0x00, 0x1d, 0x00, 0x17}),                             // supported_groups
		extension(0x000d, u16vec(0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601)), // signature_algorithms
		extension(0x002b, []byte{0x00, 0x04, 0x03, 0x04, 0x03, 0x03}),                             // supported_versions
		extension(0x002d, []byte{0x01, 0x01}),                                                     // psk_key_exchange_modes
		extension(0x0033, keyShareX25519("ShareBridgeKeyShareTestVector000")),
		extension(0x0010, alpn("h2", "http/1.1")),
		extension(0xfe0d, echGreaseBody()),
	)
	return handshakeRecord(
		[2]byte{3, 3},
		asciiVector("sharebridge-tls13-random-vector0"),
		[]uint16{0x8a8a, 0x1301, 0x1302, 0x1303},
		asciiVector("sharebridge-tls13-session-id000"),
		extensions,
	)
}

// handshakeRecord wraps a ClientHello into one full TLS record (handshake
// header included), mirroring the wire layout of RFC 8446 §4.1.2.
func handshakeRecord(helloVersion [2]byte, random []byte, cipherSuites []uint16, sessionID []byte, extensions []byte) []byte {
	body := []byte{helloVersion[0], helloVersion[1]}
	body = append(body, random...)
	body = append(body, byte(len(sessionID)))
	body = append(body, sessionID...)
	cipherSuitesLength := 2 * len(cipherSuites)
	body = append(body, byte(cipherSuitesLength>>8), byte(cipherSuitesLength))
	for _, suite := range cipherSuites {
		body = append(body, byte(suite>>8), byte(suite))
	}
	body = append(body, 0x01, 0x00) // compression_methods: null only
	if extensions != nil {
		body = append(body, byte(len(extensions)>>8), byte(len(extensions)))
		body = append(body, extensions...)
	}
	message := []byte{0x01, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}
	message = append(message, body...)
	return record(0x16, [2]byte{3, 1}, message)
}

func record(contentType byte, version [2]byte, body []byte) []byte {
	out := []byte{contentType, version[0], version[1], byte(len(body) >> 8), byte(len(body))}
	return append(out, body...)
}

func extension(extensionType uint16, body []byte) []byte {
	out := []byte{byte(extensionType >> 8), byte(extensionType), byte(len(body) >> 8), byte(len(body))}
	return append(out, body...)
}

func serverNameExtension(host string) []byte {
	entry := []byte{0x00, byte(len(host) >> 8), byte(len(host))} // host_name type + length
	entry = append(entry, host...)
	list := []byte{byte(len(entry) >> 8), byte(len(entry))}
	list = append(list, entry...)
	return extension(0x0000, list)
}

// keyShareX25519 builds the key_share extension body for one x25519 share.
func keyShareX25519(publicKey string) []byte {
	share := []byte{0x00, 0x1d, 0x00, byte(len(publicKey))} // group + key length
	share = append(share, publicKey...)
	list := []byte{byte(len(share) >> 8), byte(len(share))}
	return append(list, share...)
}

// echGreaseBody builds a realistic outer ECH extension payload (type outer,
// HKDF-SHA256, dummy HPKE enc + payload), as browsers send for GREASE.
func echGreaseBody() []byte {
	const encSecret = "ShareBridgeGreaseECHOuterEnc0000"
	const payload = "ShareBridgeECH00"
	body := []byte{0x00, 0x00, 0x01} // ECHClientHelloType outer + HpkeKdfId
	body = append(body, byte(len(encSecret)>>8), byte(len(encSecret)))
	body = append(body, encSecret...)
	body = append(body, 0x00, byte(len(payload)))
	body = append(body, payload...)
	return body
}

func alpn(protocols ...string) []byte {
	var list []byte
	for _, protocol := range protocols {
		list = append(list, byte(len(protocol)))
		list = append(list, protocol...)
	}
	out := []byte{byte(len(list) >> 8), byte(len(list))}
	return append(out, list...)
}

// u16vec builds a <T..> vector of 16-bit values (e.g. signature_algorithms).
func u16vec(values ...uint16) []byte {
	list := make([]byte, 0, 2*len(values))
	for _, value := range values {
		list = append(list, byte(value>>8), byte(value))
	}
	return append([]byte{byte(len(list) >> 8), byte(len(list))}, list...)
}

// asciiVector returns a 32-byte buffer seeded with an ASCII string and padded
// with 'Z', so test vectors stay readable in hex dumps.
func asciiVector(seed string) []byte {
	if len(seed) > 32 {
		panic("seed too long: " + seed)
	}
	out := make([]byte, 32)
	copy(out, seed)
	for offset := len(seed); offset < 32; offset++ {
		out[offset] = 'Z'
	}
	return out
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}
