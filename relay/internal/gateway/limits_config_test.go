package gateway

import (
	"log/slog"
	"net"
	"testing"
	"time"

	"sharebridge/relay/internal/limits"
	"sharebridge/relay/internal/routes"
)

// TestGatewayUsesProductionLimitsConfig proves the production configuration
// path builds the server's limiter (and therefore its ClientHello parser
// bounds) from the operator's §14 config, including a lowered global
// file-descriptor ceiling. NewServer must not fall back to the defaults when
// a config is supplied.
func TestGatewayUsesProductionLimitsConfig(t *testing.T) {
	config := limits.DefaultConfig()
	config.MaxStreamsGlobal = 7
	config.MaxStreamsPerSourceIP = 3
	config.MaxStreamsPerAgent = 5
	config.MaxStreamsPerOrigin = 4
	config.MaxHelloBytes = 512
	config.HelloTimeout = 250 * time.Millisecond
	config.DialTimeout = 900 * time.Millisecond
	config.IdleTimeout = 90 * time.Second
	config.AbsoluteLifetime = 30 * time.Minute
	config.MaxTrackedAgents = 21

	server := NewServer(routes.NewTable(nil), NewStreams(),
		WithLogger(slog.New(slog.DiscardHandler)), WithLimitsConfig(config))

	effective := server.limits.Config()
	if effective.MaxStreamsGlobal != 7 {
		t.Errorf("effective MaxStreamsGlobal = %d, want the configured global/FD-budget ceiling 7", effective.MaxStreamsGlobal)
	}
	if effective.MaxHelloBytes != 512 || effective.HelloTimeout != 250*time.Millisecond {
		t.Errorf("effective hello bounds = (%d, %v), want the configured (512, 250ms)",
			effective.MaxHelloBytes, effective.HelloTimeout)
	}
	if effective.MaxStreamsPerSourceIP != 3 || effective.MaxStreamsPerAgent != 5 || effective.MaxStreamsPerOrigin != 4 {
		t.Errorf("effective per-IP/agent/origin = (%d, %d, %d), want (3, 5, 4)",
			effective.MaxStreamsPerSourceIP, effective.MaxStreamsPerAgent, effective.MaxStreamsPerOrigin)
	}
	if effective.DialTimeout != 900*time.Millisecond || effective.IdleTimeout != 90*time.Second || effective.AbsoluteLifetime != 30*time.Minute {
		t.Errorf("effective dial/idle/lifetime = (%v, %v, %v), want the configured values",
			effective.DialTimeout, effective.IdleTimeout, effective.AbsoluteLifetime)
	}
	if effective.MaxTrackedAgents != 21 {
		t.Errorf("effective MaxTrackedAgents = %d, want 21", effective.MaxTrackedAgents)
	}
	if server.parseHello == nil {
		t.Fatal("server.parseHello is nil; the default parser must be wired from the configured bounds")
	}
	if server.limits.Config().OnSaturation == nil {
		t.Fatal("the production limiter lost its saturation hook")
	}

	// Drive the parser built from this production config: the 104-byte fixture
	// must parse under the configured 512-byte ceiling, proving the wiring,
	// not just the struct fields.
	hello := clientHelloRecord(relayRouteAlpha)
	gatewaySide, clientSide := net.Pipe()
	t.Cleanup(func() { gatewaySide.Close(); clientSide.Close() })
	go func() { _, _ = clientSide.Write(hello) }()
	parsed, err := server.parseHello(gatewaySide)
	if err != nil {
		t.Fatalf("parseHello from the production config = %v, want nil", err)
	}
	if parsed.SNI != relayRouteAlpha {
		t.Fatalf("parsed SNI = %q, want %q", parsed.SNI, relayRouteAlpha)
	}
}
