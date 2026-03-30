package peer

import (
	"fmt"

	"github.com/pion/webrtc/v4"
)

// Peer manages a single WebRTC peer connection with a browser.
type Peer struct {
	pc             *webrtc.PeerConnection
	dc             *webrtc.DataChannel
	OnOpen         func()
	OnClosed       func()
	OnICECandidate func(init webrtc.ICECandidateInit)
}

// New creates a PeerConnection with the given ICE servers.
func New(iceServers []webrtc.ICEServer) (*Peer, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: iceServers,
	})
	if err != nil {
		return nil, fmt.Errorf("new peer connection: %w", err)
	}

	p := &Peer{pc: pc}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return // ICE gathering complete
		}
		if p.OnICECandidate != nil {
			p.OnICECandidate(c.ToJSON())
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			if p.OnClosed != nil {
				p.OnClosed()
			}
		}
	})

	return p, nil
}

// CreateOffer creates a DataChannel, generates an SDP offer, and sets it as
// the local description. Returns the SDP string to send to the browser.
func (p *Peer) CreateOffer() (string, error) {
	dc, err := p.pc.CreateDataChannel("data", nil)
	if err != nil {
		return "", fmt.Errorf("create data channel: %w", err)
	}
	p.dc = dc

	dc.OnOpen(func() {
		if p.OnOpen != nil {
			p.OnOpen()
		}
	})

	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("create offer: %w", err)
	}
	if err := p.pc.SetLocalDescription(offer); err != nil {
		return "", fmt.Errorf("set local description: %w", err)
	}
	return offer.SDP, nil
}

// SetAnswer applies the browser's SDP answer as the remote description.
func (p *Peer) SetAnswer(sdp string) error {
	return p.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  sdp,
	})
}

// AddICECandidate adds an ICE candidate received from the browser.
func (p *Peer) AddICECandidate(init webrtc.ICECandidateInit) error {
	return p.pc.AddICECandidate(init)
}

// Close shuts down the peer connection.
func (p *Peer) Close() error {
	return p.pc.Close()
}
