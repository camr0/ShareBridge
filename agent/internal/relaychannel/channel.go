// agent/internal/relaychannel/channel.go
package relaychannel

import (
	"context"
	"crypto/ecdh"
	"encoding/json"
	"fmt"
	"log"

	"github.com/coder/websocket"
	"sharebridge/agent/internal/noise"
)

type SecureRelayConfig struct {
	RelayURL      string
	RelayJWT      string
	StaticPrivate *ecdh.PrivateKey
}

type SecureRelayChannel struct {
	cfg   SecureRelayConfig
	conn  *websocket.Conn
	noise *noise.NoiseXX
	send  *noise.CipherState
	recv  *noise.CipherState

	onMessage func([]byte)
	onOpen    func()
	onClose   func()
}

func NewSecureRelayChannel(cfg SecureRelayConfig) (*SecureRelayChannel, error) {
	if cfg.RelayURL == "" || cfg.RelayJWT == "" || cfg.StaticPrivate == nil {
		return nil, fmt.Errorf("relaychannel: missing required config")
	}
	return &SecureRelayChannel{cfg: cfg}, nil
}

func (c *SecureRelayChannel) SetOnMessage(handler func([]byte)) { c.onMessage = handler }
func (c *SecureRelayChannel) SetOnOpen(handler func())          { c.onOpen = handler }
func (c *SecureRelayChannel) SetOnClose(handler func())         { c.onClose = handler }

func (c *SecureRelayChannel) Start(ctx context.Context) error {
	conn, _, err := websocket.Dial(ctx, c.cfg.RelayURL, nil)
	if err != nil {
		return fmt.Errorf("dial relay websocket: %w", err)
	}
	c.conn = conn
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
		if c.onClose != nil {
			c.onClose()
		}
	}()

	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		frame, _, err := DecodeOneFrame(data)
		if err != nil {
			c.conn.Close(websocket.StatusPolicyViolation, "invalid relay frame")
			return
		}
		if frame.Kind != FrameText && frame.Kind != FrameBinary {
			c.conn.Close(websocket.StatusPolicyViolation, "unexpected relay frame kind")
			return
		}
		plain, err := c.recv.Decrypt(nil, frame.Payload)
		if err != nil {
			c.conn.Close(websocket.StatusPolicyViolation, "relay decrypt failed")
			return
		}
		if c.onMessage != nil {
			c.onMessage(plain)
		}
	}
}

func (c *SecureRelayChannel) SendText(text string) error {
	return c.sendFrame(FrameText, []byte(text))
}

func (c *SecureRelayChannel) SendBinary(data []byte) error {
	return c.sendFrame(FrameBinary, data)
}

func (c *SecureRelayChannel) BufferedAmount() uint64 { return 0 }

func (c *SecureRelayChannel) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close(websocket.StatusNormalClosure, "relay closed")
}

func (c *SecureRelayChannel) sendFrame(kind byte, plaintext []byte) error {
	ciphertext, err := c.send.Encrypt(nil, plaintext)
	if err != nil {
		return err
	}
	frame, err := WriteFrame(kind, ciphertext)
	if err != nil {
		return err
	}
	return c.conn.Write(context.Background(), websocket.MessageBinary, frame)
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