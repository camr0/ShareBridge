package relay

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
)

const (
	FrameText   = 0x01
	FrameBinary = 0x02
	MaxFrameLen = 8 * 1024 * 1024
)

const ProtocolID = protocol.ID("/sharebridge/relay/1.0.0")

type Handler struct {
	Issuer       *Issuer
	JTIs         *JTIStore
	Agents       *AgentRegistry
	ACL          *CircuitACL
	AuthTTL      time.Duration
	CodeToAPIKey func(shareCode string) (apiKeyID string, ok bool)
}

func (h *Handler) Handle(s network.Stream) {
	defer s.Close()

	claims, err := readAndValidateJWT(s, h.Issuer, h.JTIs)
	if err != nil {
		log.Printf("relay auth: rejected from %s: %v", s.Conn().RemotePeer(), err)
		writeAuthResponse(s, err.Error())
		return
	}

	if !claims.RelayAllowed && !claims.DCUtRAllowed {
		writeAuthResponse(s, "relay and DCUtR both disabled in token")
		return
	}

	apiKeyID, ok := h.CodeToAPIKey(claims.ShareCode)
	if !ok {
		writeAuthResponse(s, "unknown share code")
		return
	}
	agentPeerID, ok := h.Agents.Lookup(apiKeyID)
	if !ok {
		writeAuthResponse(s, "agent not connected")
		return
	}

	browserPeerID := s.Conn().RemotePeer()
	h.ACL.Authorize(browserPeerID, agentPeerID, claims.ShareCode, apiKeyID, h.AuthTTL)

	writeAuthOK(s)
}

func readAndValidateJWT(s network.Stream, iss *Issuer, jtis *JTIStore) (*Claims, error) {
	_ = s.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer s.SetReadDeadline(time.Time{})

	header := make([]byte, 5)
	if _, err := io.ReadFull(s, header); err != nil {
		return nil, err
	}
	kind := header[0]
	if kind != FrameText {
		return nil, errors.New("expected text frame (kind 0x01)")
	}
	length := binary.BigEndian.Uint32(header[1:5])
	if length == 0 || length > MaxFrameLen {
		return nil, errors.New("invalid frame length")
	}

	buf := make([]byte, length)
	if _, err := io.ReadFull(s, buf); err != nil {
		return nil, err
	}

	var envelope struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(buf, &envelope); err != nil {
		return nil, errors.New("invalid JSON envelope")
	}
	if envelope.Type != "jwt" {
		return nil, errors.New("expected type 'jwt' in envelope")
	}
	if envelope.Token == "" {
		return nil, errors.New("empty JWT token")
	}

	claims, err := iss.Validate(envelope.Token)
	if err != nil {
		return nil, err
	}
	if !jtis.ConsumeOnce(claims.JTI) {
		return nil, errors.New("jti already used (replay)")
	}
	if claims.BrowserPeerID != "" {
		remote := s.Conn().RemotePeer()
		claimed, err := peer.Decode(claims.BrowserPeerID)
		if err != nil || claimed != remote {
			return nil, errors.New("browser_peer_id mismatch")
		}
	}
	return claims, nil
}

func writeAuthOK(s network.Stream) {
	payload, _ := json.Marshal(map[string]string{"type": "auth_ok"})
	writeFrame(s, FrameText, payload)
}

func writeAuthResponse(s network.Stream, msg string) {
	payload, _ := json.Marshal(map[string]string{"type": "error", "message": msg})
	writeFrame(s, FrameText, payload)
}

func writeFrame(s network.Stream, kind byte, payload []byte) {
	header := make([]byte, 5)
	header[0] = kind
	binary.BigEndian.PutUint32(header[1:5], uint32(len(payload)))
	s.Write(header)
	s.Write(payload)
}