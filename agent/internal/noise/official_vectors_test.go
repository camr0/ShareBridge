package noise

import (
	"bufio"
	"bytes"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type officialNoiseVector struct {
	Name          string `json:"name"`
	Pattern       string `json:"pattern"`
	DH            string `json:"dh"`
	Cipher        string `json:"cipher"`
	Hash          string `json:"hash"`
	InitPrologue  string `json:"init_prologue"`
	InitStatic    string `json:"init_static"`
	InitEphemeral string `json:"init_ephemeral"`
	RespPrologue  string `json:"resp_prologue"`
	RespStatic    string `json:"resp_static"`
	RespEphemeral string `json:"resp_ephemeral"`
	Messages      []struct {
		Payload    string `json:"payload"`
		Ciphertext string `json:"ciphertext"`
	} `json:"messages"`
}

type officialNoiseDocument struct {
	Vectors []officialNoiseVector `json:"vectors"`
}

type nistP256Case struct {
	Count  int
	QCAVSx string
	QCAVSy string
	DIUT   string
	QIUTx  string
	QIUTy  string
	ZIUT   string
}

func TestNISTP256Vectors(t *testing.T) {
	cases := loadNISTP256Cases(t)
	if len(cases) == 0 {
		t.Fatal("expected at least one P-256 case")
	}

	for _, tc := range cases {
		tc := tc
		t.Run("count_"+strconv.Itoa(tc.Count), func(t *testing.T) {
			priv := privateKeyFromHex(t, tc.DIUT)
			pub := mustImportP256PublicKey(t, tc.QCAVSx, tc.QCAVSy)

			wantPub := append([]byte{0x04}, mustDecodeHex(t, tc.QIUTx)...)
			wantPub = append(wantPub, mustDecodeHex(t, tc.QIUTy)...)
			if !bytes.Equal(priv.PublicKey().Bytes(), wantPub) {
				t.Fatalf("public key mismatch:\n got  %x\n want %x", priv.PublicKey().Bytes(), wantPub)
			}

			shared, err := dhP256(priv, pub)
			if err != nil {
				t.Fatalf("dhP256: %v", err)
			}
			if got, want := hex.EncodeToString(shared), strings.ToLower(tc.ZIUT); got != want {
				t.Fatalf("shared secret mismatch:\n got  %s\n want %s", got, want)
			}
		})
	}
}

func TestOfficialNoiseVectorXX25519AESGCM(t *testing.T) {
	vector := loadOfficialNoiseXXVector(t)

	initStatic := mustX25519PrivateKey(t, vector.InitStatic)
	initEphemeral := mustX25519PrivateKey(t, vector.InitEphemeral)
	respStatic := mustX25519PrivateKey(t, vector.RespStatic)
	respEphemeral := mustX25519PrivateKey(t, vector.RespEphemeral)
	wantInitStaticPub := initStatic.PublicKey().Bytes()
	wantRespStaticPub := respStatic.PublicKey().Bytes()

	initState := newOfficialSymmetricState("Noise_XX_25519_AESGCM_SHA256")
	respState := newOfficialSymmetricState("Noise_XX_25519_AESGCM_SHA256")

	prologue := mustDecodeHex(t, vector.InitPrologue)
	if !bytes.Equal(prologue, mustDecodeHex(t, vector.RespPrologue)) {
		t.Fatal("init/resp prologues differ")
	}
	initState.mixHash(prologue)
	respState.mixHash(prologue)

	msg1Payload := mustDecodeHex(t, vector.Messages[0].Payload)
	msg1 := make([]byte, 0, 32+len(msg1Payload))
	msg1 = append(msg1, initEphemeral.PublicKey().Bytes()...)
	initState.mixHash(initEphemeral.PublicKey().Bytes())
	msg1Ciphertext, err := initState.encryptAndHash(msg1Payload)
	if err != nil {
		t.Fatalf("msg1 encryptAndHash: %v", err)
	}
	msg1 = append(msg1, msg1Ciphertext...)
	assertHexEqual(t, "msg1", msg1, vector.Messages[0].Ciphertext)

	respState.mixHash(msg1[:32])
	msg1Plaintext, err := respState.decryptAndHash(msg1[32:])
	if err != nil {
		t.Fatalf("msg1 decryptAndHash: %v", err)
	}
	if !bytes.Equal(msg1Plaintext, msg1Payload) {
		t.Fatalf("msg1 payload mismatch:\n got  %x\n want %x", msg1Plaintext, msg1Payload)
	}

	msg2Payload := mustDecodeHex(t, vector.Messages[1].Payload)
	msg2 := make([]byte, 0, len(mustDecodeHex(t, vector.Messages[1].Ciphertext)))
	msg2 = append(msg2, respEphemeral.PublicKey().Bytes()...)
	respState.mixHash(respEphemeral.PublicKey().Bytes())

	ee, err := x25519DH(respEphemeral, initEphemeral.PublicKey())
	if err != nil {
		t.Fatalf("msg2 ee: %v", err)
	}
	respState.mixKey(ee)

	msg2Static, err := respState.encryptAndHash(respStatic.PublicKey().Bytes())
	if err != nil {
		t.Fatalf("msg2 encrypt static: %v", err)
	}
	msg2 = append(msg2, msg2Static...)

	es, err := x25519DH(respStatic, initEphemeral.PublicKey())
	if err != nil {
		t.Fatalf("msg2 es: %v", err)
	}
	respState.mixKey(es)

	msg2PayloadCiphertext, err := respState.encryptAndHash(msg2Payload)
	if err != nil {
		t.Fatalf("msg2 encrypt payload: %v", err)
	}
	msg2 = append(msg2, msg2PayloadCiphertext...)
	assertHexEqual(t, "msg2", msg2, vector.Messages[1].Ciphertext)

	respEphemeralPub, err := ecdh.X25519().NewPublicKey(msg2[:32])
	if err != nil {
		t.Fatalf("import responder ephemeral: %v", err)
	}
	initState.mixHash(msg2[:32])
	eeInit, err := x25519DH(initEphemeral, respEphemeralPub)
	if err != nil {
		t.Fatalf("msg2 ee init: %v", err)
	}
	initState.mixKey(eeInit)

	respStaticPub, err := initState.decryptAndHash(msg2[32:80])
	if err != nil {
		t.Fatalf("msg2 decrypt static: %v", err)
	}
	if !bytes.Equal(respStaticPub, wantRespStaticPub) {
		t.Fatalf("msg2 responder static mismatch:\n got  %x\n want %x", respStaticPub, wantRespStaticPub)
	}

	esInitPub, err := ecdh.X25519().NewPublicKey(respStaticPub)
	if err != nil {
		t.Fatalf("import responder static: %v", err)
	}
	esInit, err := x25519DH(initEphemeral, esInitPub)
	if err != nil {
		t.Fatalf("msg2 es init: %v", err)
	}
	initState.mixKey(esInit)

	msg2Plaintext, err := initState.decryptAndHash(msg2[80:])
	if err != nil {
		t.Fatalf("msg2 decrypt payload: %v", err)
	}
	if !bytes.Equal(msg2Plaintext, msg2Payload) {
		t.Fatalf("msg2 payload mismatch:\n got  %x\n want %x", msg2Plaintext, msg2Payload)
	}

	msg3Payload := mustDecodeHex(t, vector.Messages[2].Payload)
	msg3 := make([]byte, 0, len(mustDecodeHex(t, vector.Messages[2].Ciphertext)))
	msg3Static, err := initState.encryptAndHash(wantInitStaticPub)
	if err != nil {
		t.Fatalf("msg3 encrypt static: %v", err)
	}
	msg3 = append(msg3, msg3Static...)

	se, err := x25519DH(initStatic, respEphemeral.PublicKey())
	if err != nil {
		t.Fatalf("msg3 se: %v", err)
	}
	initState.mixKey(se)

	msg3PayloadCiphertext, err := initState.encryptAndHash(msg3Payload)
	if err != nil {
		t.Fatalf("msg3 encrypt payload: %v", err)
	}
	msg3 = append(msg3, msg3PayloadCiphertext...)
	assertHexEqual(t, "msg3", msg3, vector.Messages[2].Ciphertext)

	initStaticPub, err := respState.decryptAndHash(msg3[:48])
	if err != nil {
		t.Fatalf("msg3 decrypt static: %v", err)
	}
	if !bytes.Equal(initStaticPub, wantInitStaticPub) {
		t.Fatalf("msg3 initiator static mismatch:\n got  %x\n want %x", initStaticPub, wantInitStaticPub)
	}

	seRespPub, err := ecdh.X25519().NewPublicKey(initStaticPub)
	if err != nil {
		t.Fatalf("import initiator static: %v", err)
	}
	seResp, err := x25519DH(respEphemeral, seRespPub)
	if err != nil {
		t.Fatalf("msg3 se resp: %v", err)
	}
	respState.mixKey(seResp)

	msg3Plaintext, err := respState.decryptAndHash(msg3[48:])
	if err != nil {
		t.Fatalf("msg3 decrypt payload: %v", err)
	}
	if !bytes.Equal(msg3Plaintext, msg3Payload) {
		t.Fatalf("msg3 payload mismatch:\n got  %x\n want %x", msg3Plaintext, msg3Payload)
	}

	iC1, iC2 := initState.split()
	rC1, rC2 := respState.split()

	transportPayload1 := mustDecodeHex(t, vector.Messages[3].Payload)
	transportCiphertext1, err := rC2.Encrypt(nil, transportPayload1)
	if err != nil {
		t.Fatalf("transport1 encrypt: %v", err)
	}
	assertHexEqual(t, "transport1", transportCiphertext1, vector.Messages[3].Ciphertext)
	transportPlaintext1, err := iC2.Decrypt(nil, transportCiphertext1)
	if err != nil {
		t.Fatalf("transport1 decrypt: %v", err)
	}
	if !bytes.Equal(transportPlaintext1, transportPayload1) {
		t.Fatalf("transport1 payload mismatch:\n got  %x\n want %x", transportPlaintext1, transportPayload1)
	}

	transportPayload2 := mustDecodeHex(t, vector.Messages[4].Payload)
	transportCiphertext2, err := iC1.Encrypt(nil, transportPayload2)
	if err != nil {
		t.Fatalf("transport2 encrypt: %v", err)
	}
	assertHexEqual(t, "transport2", transportCiphertext2, vector.Messages[4].Ciphertext)
	transportPlaintext2, err := rC1.Decrypt(nil, transportCiphertext2)
	if err != nil {
		t.Fatalf("transport2 decrypt: %v", err)
	}
	if !bytes.Equal(transportPlaintext2, transportPayload2) {
		t.Fatalf("transport2 payload mismatch:\n got  %x\n want %x", transportPlaintext2, transportPayload2)
	}

	transportPayload3 := mustDecodeHex(t, vector.Messages[5].Payload)
	transportCiphertext3, err := rC2.Encrypt(nil, transportPayload3)
	if err != nil {
		t.Fatalf("transport3 encrypt: %v", err)
	}
	assertHexEqual(t, "transport3", transportCiphertext3, vector.Messages[5].Ciphertext)
	transportPlaintext3, err := iC2.Decrypt(nil, transportCiphertext3)
	if err != nil {
		t.Fatalf("transport3 decrypt: %v", err)
	}
	if !bytes.Equal(transportPlaintext3, transportPayload3) {
		t.Fatalf("transport3 payload mismatch:\n got  %x\n want %x", transportPlaintext3, transportPayload3)
	}
}

type officialSymmetricState struct {
	h  [32]byte
	ck [32]byte
	cs *CipherState
}

func newOfficialSymmetricState(protocol string) *officialSymmetricState {
	h, ck := initializeWithProtocolName(protocol)
	return &officialSymmetricState{h: h, ck: ck}
}

func (s *officialSymmetricState) mixHash(data []byte) {
	s.h = mixHash(s.h, data)
}

func (s *officialSymmetricState) mixKey(ikm []byte) {
	var key [32]byte
	s.ck, key = hkdf2(s.ck, ikm)
	s.cs = newCipherState(key)
}

func (s *officialSymmetricState) encryptAndHash(plaintext []byte) ([]byte, error) {
	if s.cs == nil {
		out := append([]byte(nil), plaintext...)
		s.mixHash(out)
		return out, nil
	}
	ct, err := s.cs.Encrypt(s.h[:], plaintext)
	if err != nil {
		return nil, err
	}
	s.mixHash(ct)
	return ct, nil
}

func (s *officialSymmetricState) decryptAndHash(ciphertext []byte) ([]byte, error) {
	if s.cs == nil {
		out := append([]byte(nil), ciphertext...)
		s.mixHash(out)
		return out, nil
	}
	pt, err := s.cs.Decrypt(s.h[:], ciphertext)
	if err != nil {
		return nil, err
	}
	s.mixHash(ciphertext)
	return pt, nil
}

func (s *officialSymmetricState) split() (*CipherState, *CipherState) {
	k1, k2 := splitKeys(s.ck)
	return newCipherState(k1), newCipherState(k2)
}

func initializeWithProtocolName(protocol string) (h, ck [32]byte) {
	if len(protocol) <= len(h) {
		copy(h[:], protocol)
		ck = h
		return
	}
	sum := sha256.Sum256([]byte(protocol))
	h = sum
	ck = sum
	return
}

func loadOfficialNoiseXXVector(t *testing.T) officialNoiseVector {
	t.Helper()
	raw, err := os.ReadFile(externalFixturePath("cacophony.txt"))
	if err != nil {
		t.Fatalf("read cacophony.txt: %v", err)
	}
	var doc officialNoiseDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal cacophony.txt: %v", err)
	}
	for _, vector := range doc.Vectors {
		if vector.Name == "Noise_XX_25519_AESGCM_SHA256" {
			return vector
		}
	}
	t.Fatal("Noise_XX_25519_AESGCM_SHA256 not found in cacophony.txt")
	return officialNoiseVector{}
}

func loadNISTP256Cases(t *testing.T) []nistP256Case {
	t.Helper()
	file, err := os.Open(externalFixturePath("KAS_ECC_CDH_PrimitiveTest.txt"))
	if err != nil {
		t.Fatalf("open NIST vector file: %v", err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	var (
		inSection bool
		current   *nistP256Case
		cases     []nistP256Case
	)

	flush := func() {
		if current != nil {
			cases = append(cases, *current)
			current = nil
		}
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "[P-256]":
			inSection = true
			continue
		case inSection && strings.HasPrefix(line, "[") && line != "[P-256]":
			flush()
			return cases
		case !inSection || line == "" || strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "COUNT = "):
			flush()
			count, err := strconv.Atoi(strings.TrimPrefix(line, "COUNT = "))
			if err != nil {
				t.Fatalf("parse COUNT: %v", err)
			}
			current = &nistP256Case{Count: count}
		default:
			key, value, ok := strings.Cut(line, " = ")
			if !ok || current == nil {
				continue
			}
			switch key {
			case "QCAVSx":
				current.QCAVSx = value
			case "QCAVSy":
				current.QCAVSy = value
			case "dIUT":
				current.DIUT = value
			case "QIUTx":
				current.QIUTx = value
			case "QIUTy":
				current.QIUTy = value
			case "ZIUT":
				current.ZIUT = value
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan NIST vector file: %v", err)
	}
	flush()
	return cases
}

func externalFixturePath(name string) string {
	return filepath.Join("..", "..", "..", "testdata", "external", name)
}

func mustImportP256PublicKey(t *testing.T, xHex, yHex string) *ecdh.PublicKey {
	t.Helper()
	raw := append([]byte{0x04}, mustDecodeHex(t, xHex)...)
	raw = append(raw, mustDecodeHex(t, yHex)...)
	pub, err := importPublicKey(raw)
	if err != nil {
		t.Fatalf("import P-256 public key: %v", err)
	}
	return pub
}

func mustX25519PrivateKey(t *testing.T, hexString string) *ecdh.PrivateKey {
	t.Helper()
	raw := mustDecodeHex(t, hexString)
	key, err := ecdh.X25519().NewPrivateKey(raw)
	if err != nil {
		t.Fatalf("import X25519 private key: %v", err)
	}
	return key
}

func x25519DH(priv *ecdh.PrivateKey, pub *ecdh.PublicKey) ([]byte, error) {
	return priv.ECDH(pub)
}

func mustDecodeHex(t *testing.T, hexString string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(hexString)
	if err != nil {
		t.Fatalf("decode hex %q: %v", hexString, err)
	}
	return raw
}

func assertHexEqual(t *testing.T, label string, got []byte, wantHex string) {
	t.Helper()
	want := mustDecodeHex(t, wantHex)
	if !bytes.Equal(got, want) {
		t.Fatalf("%s mismatch:\n got  %x\n want %x", label, got, want)
	}
}
