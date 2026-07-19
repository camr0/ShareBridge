// agent/internal/relaychannel/channel.go
package relaychannel

import (
	"context"
	"crypto/ecdh"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/coder/websocket"
	"sharebridge/agent/internal/multilane"
	"sharebridge/agent/internal/noise"
)

type SecureRelayConfig struct {
	RelayURL      string
	RelayJWT      string
	StaticPrivate *ecdh.PrivateKey
}

type SecureRelayChannel struct {
	cfg       SecureRelayConfig
	conn      *websocket.Conn
	noise     *noise.NoiseXX
	send      *noise.CipherState
	recv      *noise.CipherState
	scheduler *multilane.Scheduler
	endpoints map[multilane.Lane]*relayEndpoint
	closeOnce sync.Once

	onOpen  func()
	onClose func()
}

// relayWebSocketReadLimit matches the relay transport frame cap with room for
// framing and Noise overhead.
const relayWebSocketReadLimit = 10 * 1024 * 1024

const (
	noiseAEADOverheadBytes = 16
	// MaxRelayPayloadBytes leaves room for the encrypted lane byte and AES-GCM tag.
	MaxRelayPayloadBytes = MaxFramePayload - 1 - noiseAEADOverheadBytes
)

func NewSecureRelayChannel(cfg SecureRelayConfig) (*SecureRelayChannel, error) {
	if cfg.RelayURL == "" || cfg.RelayJWT == "" || cfg.StaticPrivate == nil {
		return nil, fmt.Errorf("relaychannel: missing required config")
	}
	c := &SecureRelayChannel{cfg: cfg, endpoints: make(map[multilane.Lane]*relayEndpoint)}
	for _, lane := range []multilane.Lane{multilane.LaneControl, multilane.LaneMedia, multilane.LaneBulk} {
		c.endpoints[lane] = &relayEndpoint{owner: c, lane: lane}
	}
	c.scheduler = multilane.NewScheduler(c.writeScheduled)
	return c, nil
}

// SetOnMessage is retained during migration as an alias for the control lane.
func (c *SecureRelayChannel) SetOnMessage(handler func([]byte)) {
	c.endpoints[multilane.LaneControl].SetOnMessage(handler)
}
func (c *SecureRelayChannel) SetOnOpen(handler func())  { c.onOpen = handler }
func (c *SecureRelayChannel) SetOnClose(handler func()) { c.onClose = handler }

func (c *SecureRelayChannel) Endpoint(lane multilane.Lane) multilane.Endpoint {
	endpoint, ok := c.endpoints[lane]
	if !ok {
		return nil
	}
	return endpoint
}

func (c *SecureRelayChannel) Start(ctx context.Context) error {
	started := false
	defer func() {
		if started {
			return
		}
		_ = c.scheduler.Close()
		if c.conn != nil {
			c.conn.CloseNow()
		}
		c.notifyClose()
	}()
	conn, _, err := websocket.Dial(ctx, c.cfg.RelayURL, nil)
	if err != nil {
		return fmt.Errorf("dial relay websocket: %w", err)
	}
	c.conn = conn
	c.conn.SetReadLimit(relayWebSocketReadLimit)
	log.Printf("relaychannel: connected to relay at %s", c.cfg.RelayURL)

	hello, _ := json.Marshal(map[string]string{"token": c.cfg.RelayJWT})
	if err := c.conn.Write(ctx, websocket.MessageText, hello); err != nil {
		c.conn.CloseNow()
		return fmt.Errorf("write relay hello: %w", err)
	}
	log.Printf("relaychannel: sent hello with JWT")

	nx, err := noise.NewResponder(c.cfg.StaticPrivate)
	if err != nil {
		c.conn.CloseNow()
		return fmt.Errorf("new responder: %w", err)
	}
	c.noise = nx
	log.Printf("relaychannel: created noise responder")

	if err := c.runHandshake(ctx); err != nil {
		c.conn.CloseNow()
		return err
	}
	log.Printf("relaychannel: handshake complete")

	go c.readLoop(context.Background())

	if c.onOpen != nil {
		c.onOpen()
	}
	started = true
	return nil
}

func (c *SecureRelayChannel) runHandshake(ctx context.Context) error {
	// Read msg1 from initiator (browser)
	_, msg1Frame, err := c.conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("read msg1: %w", err)
	}
	msg1, err := decodeFramePayload(msg1Frame, FrameHandshake)
	if err != nil {
		return err
	}
	if err := c.noise.ReadMessage1(msg1); err != nil {
		return fmt.Errorf("read Noise msg1: %w", err)
	}

	// Write msg2 as responder (agent)
	msg2, err := c.noise.WriteMessage2()
	if err != nil {
		return fmt.Errorf("write Noise msg2: %w", err)
	}
	msg2Frame, err := WriteFrame(FrameHandshake, msg2)
	if err != nil {
		return err
	}
	if err := c.conn.Write(ctx, websocket.MessageBinary, msg2Frame); err != nil {
		return fmt.Errorf("write msg2 frame: %w", err)
	}

	// Read msg3 from initiator
	_, msg3Frame, err := c.conn.Read(ctx)
	if err != nil {
		return fmt.Errorf("read msg3: %w", err)
	}
	msg3, err := decodeFramePayload(msg3Frame, FrameHandshake)
	if err != nil {
		return err
	}
	if err := c.noise.ReadMessage3(msg3); err != nil {
		return fmt.Errorf("read Noise msg3: %w", err)
	}

	c.send, c.recv = c.noise.Split()
	return nil
}

func (c *SecureRelayChannel) readLoop(ctx context.Context) {
	defer func() {
		_ = c.scheduler.Close()
		c.notifyClose()
	}()

	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			log.Printf("relaychannel: read loop ended: %v", err)
			return
		}
		frame, rest, err := DecodeOneFrame(data)
		if err != nil {
			log.Printf("relaychannel: invalid relay frame bytes=%d: %v", len(data), err)
			c.conn.Close(websocket.StatusPolicyViolation, "invalid relay frame")
			return
		}
		if len(rest) != 0 {
			log.Printf("relaychannel: trailing relay frame bytes=%d", len(rest))
			c.conn.Close(websocket.StatusPolicyViolation, "multiple relay frames in websocket message")
			return
		}
		if frame.Kind != FrameText && frame.Kind != FrameBinary {
			log.Printf("relaychannel: unexpected relay frame kind=0x%02x", frame.Kind)
			c.conn.Close(websocket.StatusPolicyViolation, "unexpected relay frame kind")
			return
		}
		plain, err := c.recv.Decrypt(nil, frame.Payload)
		if err != nil {
			log.Printf("relaychannel: decrypt failed frame_kind=0x%02x ciphertext_bytes=%d: %v", frame.Kind, len(frame.Payload), err)
			c.conn.Close(websocket.StatusPolicyViolation, "relay decrypt failed")
			return
		}
		lane, payload, err := multilane.DecodeEnvelope(plain)
		if err != nil {
			log.Printf("relaychannel: invalid encrypted lane envelope: %v", err)
			c.conn.Close(websocket.StatusPolicyViolation, "invalid encrypted lane")
			return
		}
		endpoint := c.endpoints[lane]
		if endpoint == nil {
			c.conn.Close(websocket.StatusPolicyViolation, "unknown encrypted lane")
			return
		}
		endpoint.deliver(payload)
	}
}

// SendText and SendBinary are control-lane compatibility shims for callers
// that have not yet been migrated to ChannelSet.
func (c *SecureRelayChannel) SendText(text string) error {
	return c.endpoints[multilane.LaneControl].SendText(text)
}

func (c *SecureRelayChannel) SendBinary(data []byte) error {
	return c.endpoints[multilane.LaneControl].SendBinary(data)
}

func (c *SecureRelayChannel) BufferedAmount() uint64 { return 0 }

func (c *SecureRelayChannel) Close() error {
	_ = c.scheduler.Close()
	if c.conn == nil {
		c.notifyClose()
		return nil
	}
	err := c.conn.Close(websocket.StatusNormalClosure, "relay closed")
	c.notifyClose()
	return err
}

func (c *SecureRelayChannel) writeScheduled(class multilane.TrafficClass, kind multilane.Kind, payload []byte) error {
	lane, err := multilane.LaneForClass(class)
	if err != nil {
		return err
	}
	plaintext, err := multilane.EncodeEnvelope(lane, payload)
	if err != nil {
		return err
	}
	if c.send == nil || c.conn == nil {
		return fmt.Errorf("relaychannel: channel is not open")
	}
	ciphertext, err := c.send.Encrypt(nil, plaintext)
	if err != nil {
		log.Printf("relaychannel: encrypt failed kind=0x%02x plaintext_bytes=%d: %v", kind, len(plaintext), err)
		c.abortTransport()
		return err
	}
	frame, err := WriteFrame(byte(kind), ciphertext)
	if err != nil {
		log.Printf("relaychannel: frame encode failed kind=0x%02x ciphertext_bytes=%d: %v", kind, len(ciphertext), err)
		c.abortTransport()
		return err
	}
	if err := c.conn.Write(context.Background(), websocket.MessageBinary, frame); err != nil {
		log.Printf("relaychannel: write failed kind=0x%02x plaintext_bytes=%d ciphertext_bytes=%d frame_bytes=%d: %v", kind, len(plaintext), len(ciphertext), len(frame), err)
		c.abortTransport()
		return err
	}
	return nil
}

func (c *SecureRelayChannel) abortTransport() {
	if c.conn != nil {
		c.conn.CloseNow()
	}
	c.notifyClose()
}

func (c *SecureRelayChannel) notifyClose() {
	c.closeOnce.Do(func() {
		if c.onClose != nil {
			c.onClose()
		}
	})
}

type relayEndpoint struct {
	owner     *SecureRelayChannel
	lane      multilane.Lane
	mu        sync.RWMutex
	onMessage func([]byte)
}

func (e *relayEndpoint) SendText(text string) error {
	return e.send(classForLane(e.lane), multilane.KindText, []byte(text))
}

func (e *relayEndpoint) SendBinary(data []byte) error {
	return e.send(classForLane(e.lane), multilane.KindBinary, data)
}

func (e *relayEndpoint) SendBinaryClass(class multilane.TrafficClass, data []byte) error {
	lane, err := multilane.LaneForClass(class)
	if err != nil {
		return err
	}
	if lane != e.lane {
		return fmt.Errorf("relaychannel: traffic class %d maps to lane %d, not endpoint lane %d", class, lane, e.lane)
	}
	return e.send(class, multilane.KindBinary, data)
}

func (e *relayEndpoint) send(class multilane.TrafficClass, kind multilane.Kind, payload []byte) error {
	if len(payload) > MaxRelayPayloadBytes {
		return fmt.Errorf("relaychannel: payload is %d bytes, maximum is %d", len(payload), MaxRelayPayloadBytes)
	}
	return e.owner.scheduler.Send(context.Background(), class, kind, payload)
}

func (e *relayEndpoint) BufferedAmount() uint64 { return 0 }

func (e *relayEndpoint) SetOnMessage(handler func([]byte)) {
	e.mu.Lock()
	e.onMessage = handler
	e.mu.Unlock()
}

func (e *relayEndpoint) deliver(payload []byte) {
	e.mu.RLock()
	handler := e.onMessage
	e.mu.RUnlock()
	if handler != nil {
		handler(append([]byte(nil), payload...))
	}
}

func classForLane(lane multilane.Lane) multilane.TrafficClass {
	switch lane {
	case multilane.LaneMedia:
		return multilane.ClassInteractiveMedia
	case multilane.LaneBulk:
		return multilane.ClassBulk
	default:
		return multilane.ClassControl
	}
}

func decodeFramePayload(raw []byte, wantKind byte) ([]byte, error) {
	frame, rest, err := DecodeOneFrame(raw)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("unexpected trailing frame bytes")
	}
	if frame.Kind != wantKind {
		return nil, fmt.Errorf("unexpected frame kind: got 0x%02x want 0x%02x", frame.Kind, wantKind)
	}
	return frame.Payload, nil
}
