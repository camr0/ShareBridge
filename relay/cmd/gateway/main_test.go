package main

import (
	"testing"

	"sharebridge/relay/internal/limits"
)

// TestConfiguredLimitsReadsProductionEnvironment proves the gateway binary's
// actual configuration entry point reads the §14 operator environment,
// including a lowered global file-descriptor ceiling and the hello
// byte/timeout bounds the parser must honour.
func TestConfiguredLimitsReadsProductionEnvironment(t *testing.T) {
	t.Setenv(limits.EnvMaxStreamsGlobal, "123")
	t.Setenv(limits.EnvMaxStreamsPerSourceIP, "7")
	t.Setenv(limits.EnvMaxHelloBytes, "4096")
	t.Setenv(limits.EnvHelloTimeout, "750ms")

	config, err := configuredLimits()
	if err != nil {
		t.Fatalf("configuredLimits() = %v, want nil", err)
	}
	if config.MaxStreamsGlobal != 123 {
		t.Errorf("MaxStreamsGlobal = %d, want the environment value 123", config.MaxStreamsGlobal)
	}
	if config.MaxStreamsPerSourceIP != 7 {
		t.Errorf("MaxStreamsPerSourceIP = %d, want the environment value 7", config.MaxStreamsPerSourceIP)
	}
	if config.MaxHelloBytes != 4096 {
		t.Errorf("MaxHelloBytes = %d, want the environment value 4096", config.MaxHelloBytes)
	}
	if config.HelloTimeout.String() != "750ms" {
		t.Errorf("HelloTimeout = %v, want the environment value 750ms", config.HelloTimeout)
	}
}

// TestConfiguredLimitsFailsClosedOnInvalidEnvironment proves the binary
// refuses to start on a nonsensical operator value instead of running with a
// silently wrong bound.
func TestConfiguredLimitsFailsClosedOnInvalidEnvironment(t *testing.T) {
	t.Setenv(limits.EnvMaxStreamsGlobal, "0")
	if _, err := configuredLimits(); err == nil {
		t.Fatal("configuredLimits() with a zero global ceiling error = nil, want a fail-closed rejection")
	}
}
