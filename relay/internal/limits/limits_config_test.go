package limits

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"sharebridge/relay/internal/clienthello"
)

// environmentFrom builds an EnvironmentLookup from a static map.
func environmentFrom(values map[string]string) EnvironmentLookup {
	return func(name string) (string, bool) {
		value, ok := values[name]
		return value, ok
	}
}

// TestLimitsConfigFromEnvironmentReadsEveryDocumentedBound proves every §14
// operator-tunable bound has a production configuration input and that the
// environment value wins over the default. The global ceiling is set below
// the §14 default so a lowered host file-descriptor budget is deployable.
func TestLimitsConfigFromEnvironmentReadsEveryDocumentedBound(t *testing.T) {
	values := map[string]string{
		EnvMaxStreamsPerSourceIP: "8",
		EnvMaxStreamsPerOrigin:   "9",
		EnvMaxStreamsPerAgent:    "10",
		EnvMaxStreamsGlobal:      "11",
		EnvMaxHelloBytes:         "1234",
		EnvHelloTimeout:          "1500ms",
		EnvDialTimeout:           "1750ms",
		EnvIdleTimeout:           "2m",
		EnvAbsoluteLifetime:      "3h",
		EnvMaxTrackedAgents:      "12",
	}
	config, err := ConfigFromEnvironment(environmentFrom(values))
	if err != nil {
		t.Fatalf("ConfigFromEnvironment(%v) = %v, want nil", values, err)
	}

	integerChecks := []struct {
		name string
		got  int
		want int
	}{
		{"MaxStreamsPerSourceIP", config.MaxStreamsPerSourceIP, 8},
		{"MaxStreamsPerOrigin", config.MaxStreamsPerOrigin, 9},
		{"MaxStreamsPerAgent", config.MaxStreamsPerAgent, 10},
		{"MaxStreamsGlobal", config.MaxStreamsGlobal, 11},
		{"MaxHelloBytes", config.MaxHelloBytes, 1234},
		{"MaxTrackedAgents", config.MaxTrackedAgents, 12},
	}
	for _, check := range integerChecks {
		if check.got != check.want {
			t.Errorf("configured %s = %d, want the environment value %d", check.name, check.got, check.want)
		}
	}

	durationChecks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"HelloTimeout", config.HelloTimeout, 1500 * time.Millisecond},
		{"DialTimeout", config.DialTimeout, 1750 * time.Millisecond},
		{"IdleTimeout", config.IdleTimeout, 2 * time.Minute},
		{"AbsoluteLifetime", config.AbsoluteLifetime, 3 * time.Hour},
	}
	for _, check := range durationChecks {
		if check.got != check.want {
			t.Errorf("configured %s = %v, want the environment value %v", check.name, check.got, check.want)
		}
	}

	// The §14 "exactly 1 proxy per agent" rule is a product invariant, not an
	// operator-tunable limit.
	if config.ProxiesPerAgent != 1 {
		t.Errorf("configured ProxiesPerAgent = %d, want the fixed §14 value 1", config.ProxiesPerAgent)
	}
}

// TestLimitsConfigFromEnvironmentDefaultsWhenUnset pins that an empty
// environment yields exactly the documented §14 defaults: configuration only
// overrides, it never silently relaxes an unset bound.
func TestLimitsConfigFromEnvironmentDefaultsWhenUnset(t *testing.T) {
	config, err := ConfigFromEnvironment(environmentFrom(nil))
	if err != nil {
		t.Fatalf("ConfigFromEnvironment(empty) = %v, want nil", err)
	}
	if !reflect.DeepEqual(config, DefaultConfig()) {
		t.Fatalf("empty environment config = %+v, want the §14 defaults %+v", config, DefaultConfig())
	}
}

// TestLimitsConfigFromEnvironmentFailsClosedOnInvalidValues proves a
// nonsensical or over-ceiling operator value refuses startup instead of
// silently running with a wrong bound. Every rejection must return the zero
// configuration, never a partially-applied one.
func TestLimitsConfigFromEnvironmentFailsClosedOnInvalidValues(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{"per-source-IP zero", EnvMaxStreamsPerSourceIP, "0"},
		{"per-origin negative", EnvMaxStreamsPerOrigin, "-1"},
		{"per-agent not a number", EnvMaxStreamsPerAgent, "many"},
		{"global zero", EnvMaxStreamsGlobal, "0"},
		{"global over the §14 ceiling", EnvMaxStreamsGlobal, strconv.Itoa(DefaultMaxStreamsGlobal + 1)},
		{"hello bytes zero", EnvMaxHelloBytes, "0"},
		{"hello bytes over the parser budget", EnvMaxHelloBytes, strconv.Itoa(clienthello.MaxBufferedBytes + 1)},
		{"hello timeout zero", EnvHelloTimeout, "0s"},
		{"hello timeout negative", EnvHelloTimeout, "-1s"},
		{"hello timeout not a duration", EnvHelloTimeout, "soon"},
		{"dial timeout zero", EnvDialTimeout, "0"},
		{"idle timeout negative", EnvIdleTimeout, "-5m"},
		{"absolute lifetime zero", EnvAbsoluteLifetime, "0"},
		{"tracked agents zero", EnvMaxTrackedAgents, "0"},
		{"tracked agents negative", EnvMaxTrackedAgents, "-2"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config, err := ConfigFromEnvironment(environmentFrom(map[string]string{testCase.key: testCase.value}))
			if err == nil {
				t.Fatalf("ConfigFromEnvironment(%s=%q) = %+v, want a fail-closed rejection", testCase.key, testCase.value, config)
			}
			if !reflect.DeepEqual(config, Config{}) {
				t.Fatalf("rejected config = %+v, want the zero Config so startup cannot use a partial bound", config)
			}
		})
	}
}

// TestLimitsConfigTrackedAgentCeilingIsHard pins the audit-#6 hard ceiling:
// the persistent per-agent byte map is the one map whose size is not bounded
// by current concurrency over process lifetime, so the operator surface must
// not be effectively unbounded. Any value at or below the ceiling is
// accepted; anything above it fails closed with an error naming the variable
// and the ceiling.
func TestLimitsConfigTrackedAgentCeilingIsHard(t *testing.T) {
	for _, value := range []int{1, DefaultMaxTrackedAgents - 1, DefaultMaxTrackedAgents} {
		config, err := ConfigFromEnvironment(environmentFrom(map[string]string{EnvMaxTrackedAgents: strconv.Itoa(value)}))
		if err != nil {
			t.Fatalf("ConfigFromEnvironment(%s=%d) = %v, want it accepted", EnvMaxTrackedAgents, value, err)
		}
		if config.MaxTrackedAgents != value {
			t.Fatalf("MaxTrackedAgents = %d, want the configured %d", config.MaxTrackedAgents, value)
		}
	}

	config, err := ConfigFromEnvironment(environmentFrom(map[string]string{EnvMaxTrackedAgents: strconv.Itoa(DefaultMaxTrackedAgents + 1)}))
	if err == nil {
		t.Fatalf("ConfigFromEnvironment(%s=%d) = %+v, want a fail-closed rejection above the ceiling %d",
			EnvMaxTrackedAgents, DefaultMaxTrackedAgents+1, config, DefaultMaxTrackedAgents)
	}
	if !reflect.DeepEqual(config, Config{}) {
		t.Fatalf("rejected config = %+v, want the zero Config so startup cannot use a partial bound", config)
	}
	if !strings.Contains(err.Error(), EnvMaxTrackedAgents) || !strings.Contains(err.Error(), strconv.Itoa(DefaultMaxTrackedAgents)) {
		t.Fatalf("ceiling error %q must name %s and the ceiling %d", err, EnvMaxTrackedAgents, DefaultMaxTrackedAgents)
	}
}
