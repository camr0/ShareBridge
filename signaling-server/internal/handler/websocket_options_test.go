package handler

import (
	"testing"

	"github.com/coder/websocket"
)

func TestWSAcceptOptions_DisablesCompression(t *testing.T) {
	opts := wsAcceptOptions()
	if opts == nil {
		t.Fatal("wsAcceptOptions returned nil")
	}
	if opts.CompressionMode != websocket.CompressionDisabled {
		t.Fatalf("CompressionMode = %v, want %v", opts.CompressionMode, websocket.CompressionDisabled)
	}
}
