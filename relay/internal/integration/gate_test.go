package integration

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The §23.3 byte-preservation gate must be un-forgeable: the default
// `go test ./...` run may skip the real-FRP suite, but the documented gate
// invocation must FAIL CLOSED if the suite is not actually exercised. Env
// SHAREBRIDGE_FRP_GATE=required turns every skip path into a failure, verifies
// the pinned binaries at gate time, and requires every named gate case to
// execute to completion.
const (
	gateModeEnv      = "SHAREBRIDGE_FRP_GATE"
	gateModeRequired = "required"
)

// ---------------------------------------------------------------------------
// Pinned FRP artifact resolution and gate-time integrity verification
// ---------------------------------------------------------------------------

// frpArtifactPin is one platform's pinned upstream URL and SHA-256 digest.
type frpArtifactPin struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// frpManifest mirrors relay/frp/manifest.json.
type frpManifest struct {
	Version   string                    `json:"version"`
	Artifacts map[string]frpArtifactPin `json:"artifacts"`
}

var (
	errNoPinnedArtifact   = errors.New("no pinned FRP artifact")
	immutableVersionRE    = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	sha256HexRE           = regexp.MustCompile(`^[0-9a-f]{64}$`)
	maxExtractedFRPBinary = int64(256 << 20)
)

// pinnedArtifact resolves and validates one platform's manifest entry. It
// fails closed on a missing entry, a non-immutable version tag, a malformed
// digest, or a URL that does not name the exact pinned release asset.
func pinnedArtifact(manifest frpManifest, platformKey string) (frpArtifactPin, error) {
	pin, ok := manifest.Artifacts[platformKey]
	if !ok {
		keys := make([]string, 0, len(manifest.Artifacts))
		for key := range manifest.Artifacts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return frpArtifactPin{}, fmt.Errorf("%w for %s (manifest pins %v)", errNoPinnedArtifact, platformKey, keys)
	}
	if !immutableVersionRE.MatchString(manifest.Version) {
		return frpArtifactPin{}, fmt.Errorf("manifest version %q is not an immutable x.y.z release tag", manifest.Version)
	}
	if !sha256HexRE.MatchString(pin.SHA256) {
		return frpArtifactPin{}, fmt.Errorf("manifest SHA-256 for %s is not 64 lowercase hex characters: %q", platformKey, pin.SHA256)
	}
	expectedURL := fmt.Sprintf(
		"https://github.com/fatedier/frp/releases/download/v%s/frp_%s_%s.tar.gz",
		manifest.Version, manifest.Version, platformKey)
	if pin.URL != expectedURL {
		return frpArtifactPin{}, fmt.Errorf("manifest URL for %s is not the exact pinned release asset %q: %q", platformKey, expectedURL, pin.URL)
	}
	return pin, nil
}

var pinnedPlatformKey = runtime.GOOS + "_" + runtime.GOARCH

// loadPinnedManifest reads relay/frp/manifest.json relative to root.
func loadPinnedManifest(root string) (frpManifest, []byte, error) {
	manifestPath := filepath.Join(root, "frp", "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return frpManifest{}, nil, fmt.Errorf("read pinned FRP manifest %s: %w", manifestPath, err)
	}
	var manifest frpManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return frpManifest{}, nil, fmt.Errorf("parse pinned FRP manifest %s: %w", manifestPath, err)
	}
	return manifest, raw, nil
}

// resolveFRPCacheRootAt mirrors the fetch script: SHAREBRIDGE_FRP_CACHE when
// set, otherwise <root>/.cache/frp.
func resolveFRPCacheRootAt(root string) string {
	if override := os.Getenv("SHAREBRIDGE_FRP_CACHE"); override != "" {
		if absolute, err := filepath.Abs(override); err == nil {
			return absolute
		}
		return override
	}
	return filepath.Join(root, ".cache", "frp")
}

func sha256File(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// sha256FileHex is the testing.T-friendly wrapper used by tests.
func sha256FileHex(t *testing.T, path string) string {
	t.Helper()
	digest, err := sha256File(path)
	if err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return digest
}

// cacheArtifactDigests is the integrity evidence for one pinned cache entry.
type cacheArtifactDigests struct {
	TarballSHA256 string
	FrpsSHA256    string
	FrpcSHA256    string
}

// hashCacheArtifacts verifies a version+digest-keyed FRP cache without
// running the binaries: it re-hashes the tarball against the pinned digest,
// extracts frps/frpc from that verified tarball in memory, and requires the
// cached executables to hash identically. A complete-but-poisoned cache (the
// legacy `.verified` + executable-bits trust check) therefore fails closed.
func hashCacheArtifacts(cacheRoot, version, platformKey, expectedTarballSHA string) (cacheArtifactDigests, error) {
	var digests cacheArtifactDigests
	if !sha256HexRE.MatchString(expectedTarballSHA) {
		return digests, fmt.Errorf("pinned tarball digest %q is not 64 lowercase hex characters", expectedTarballSHA)
	}
	tarballPath := filepath.Join(cacheRoot, "downloads", fmt.Sprintf("frp_%s_%s.tar.gz", version, platformKey))
	info, err := os.Stat(tarballPath)
	if err != nil {
		return digests, fmt.Errorf("pinned FRP tarball %s is missing (run relay/scripts/fetch-frp.sh): %w", tarballPath, err)
	}
	if !info.Mode().IsRegular() {
		return digests, fmt.Errorf("pinned FRP tarball %s is not a regular file", tarballPath)
	}
	actualTarballSHA, err := sha256File(tarballPath)
	if err != nil {
		return digests, fmt.Errorf("hash pinned FRP tarball %s: %w", tarballPath, err)
	}
	if actualTarballSHA != expectedTarballSHA {
		return digests, fmt.Errorf("pinned FRP tarball %s has SHA-256 %s; manifest pins %s (fail closed)",
			tarballPath, actualTarballSHA, expectedTarballSHA)
	}
	digests.TarballSHA256 = actualTarballSHA

	extracted, err := extractFRPBinariesFromTarball(tarballPath)
	if err != nil {
		return digests, err
	}
	keyDir := filepath.Join(cacheRoot, fmt.Sprintf("v%s-sha256-%s", version, expectedTarballSHA))
	for _, name := range []string{"frps", "frpc"} {
		binaryPath := filepath.Join(keyDir, name)
		binaryInfo, err := os.Stat(binaryPath)
		if err != nil {
			return digests, fmt.Errorf("cached %s is missing from %s: %w", name, keyDir, err)
		}
		if binaryInfo.Mode()&0o111 == 0 {
			return digests, fmt.Errorf("cached %s at %s is not executable (mode %s)", name, binaryPath, binaryInfo.Mode())
		}
		cachedSHA, err := sha256File(binaryPath)
		if err != nil {
			return digests, fmt.Errorf("hash cached %s: %w", binaryPath, err)
		}
		extractedSHA := sha256Hex(extracted[name])
		if cachedSHA != extractedSHA {
			return digests, fmt.Errorf("cached %s at %s has SHA-256 %s but the verified tarball contains %s (poisoned cache: fail closed)",
				name, binaryPath, cachedSHA, extractedSHA)
		}
		if name == "frps" {
			digests.FrpsSHA256 = cachedSHA
		} else {
			digests.FrpcSHA256 = cachedSHA
		}
	}
	return digests, nil
}

// extractFRPBinariesFromTarball returns the frps/frpc contents of a verified
// FRP release tarball, rejecting duplicate or oversized entries.
func extractFRPBinariesFromTarball(tarballPath string) (map[string][]byte, error) {
	file, err := os.Open(tarballPath)
	if err != nil {
		return nil, fmt.Errorf("open FRP tarball %s: %w", tarballPath, err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("gunzip FRP tarball %s: %w", tarballPath, err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	found := make(map[string][]byte, 2)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read FRP tarball %s: %w", tarballPath, err)
		}
		name := filepath.Base(header.Name)
		if name != "frps" && name != "frpc" {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("FRP tarball %s entry %s is not a regular file", tarballPath, header.Name)
		}
		if _, duplicate := found[name]; duplicate {
			return nil, fmt.Errorf("FRP tarball %s contains duplicate %s entries", tarballPath, name)
		}
		if header.Size > maxExtractedFRPBinary {
			return nil, fmt.Errorf("FRP tarball %s entry %s is implausibly large (%d bytes)", tarballPath, header.Name, header.Size)
		}
		content, err := io.ReadAll(io.LimitReader(tarReader, maxExtractedFRPBinary+1))
		if err != nil {
			return nil, fmt.Errorf("extract %s from FRP tarball: %w", name, err)
		}
		found[name] = content
	}
	for _, name := range []string{"frps", "frpc"} {
		if _, ok := found[name]; !ok {
			return nil, fmt.Errorf("FRP tarball %s does not contain %s", tarballPath, name)
		}
	}
	return found, nil
}

// runBinaryVersion executes `<binary> --version` under a short budget.
func runBinaryVersion(binaryPath string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, binaryPath, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", binaryPath, err)
	}
	return strings.TrimSpace(string(output)), nil
}

// gateFRPEvidence is the machine-emitted §23.3 pin evidence. Every field is
// produced by the gate run itself; nothing in GATE-EVIDENCE.md is hand-typed
// beyond quoting this output.
type gateFRPEvidence struct {
	Version          string
	Platform         string
	ManifestSHA256   string
	ConfigSHA256     string
	TarballPath      string
	TarballSHA256    string
	FrpsPath         string
	FrpsSHA256       string
	FrpsVersion      string
	FrpcPath         string
	FrpcSHA256       string
	FrpcVersion      string
	CacheArtifactSum string
}

// verifyPinnedBinariesAtGate re-resolves the pinned manifest and re-hashes the
// cached tarball, the extracted binaries, and the staged executables at gate
// time, and validates the reported versions. Any mismatch, missing manifest
// entry, or missing/corrupt artifact is a hard error (fail closed).
func verifyPinnedBinariesAtGate(root string) (*gateFRPEvidence, error) {
	manifest, manifestBytes, err := loadPinnedManifest(root)
	if err != nil {
		return nil, err
	}
	pin, err := pinnedArtifact(manifest, pinnedPlatformKey)
	if err != nil {
		return nil, err
	}
	cacheRoot := resolveFRPCacheRootAt(root)
	digests, err := hashCacheArtifacts(cacheRoot, manifest.Version, pinnedPlatformKey, pin.SHA256)
	if err != nil {
		return nil, err
	}
	keyDir := filepath.Join(cacheRoot, fmt.Sprintf("v%s-sha256-%s", manifest.Version, pin.SHA256))
	frpsPath := filepath.Join(keyDir, "frps")
	frpcPath := filepath.Join(keyDir, "frpc")
	frpsVersion, err := runBinaryVersion(frpsPath)
	if err != nil {
		return nil, err
	}
	frpcVersion, err := runBinaryVersion(frpcPath)
	if err != nil {
		return nil, err
	}
	if frpsVersion != manifest.Version {
		return nil, fmt.Errorf("frps at %s reports version %q; manifest pins %q", frpsPath, frpsVersion, manifest.Version)
	}
	if frpcVersion != manifest.Version {
		return nil, fmt.Errorf("frpc at %s reports version %q; manifest pins %q", frpcPath, frpcVersion, manifest.Version)
	}
	configSHA, err := sha256File(filepath.Join(root, "config", "frps.toml"))
	if err != nil {
		return nil, fmt.Errorf("hash committed relay config: %w", err)
	}
	return &gateFRPEvidence{
		Version:          manifest.Version,
		Platform:         pinnedPlatformKey,
		ManifestSHA256:   sha256Hex(manifestBytes),
		ConfigSHA256:     configSHA,
		TarballPath:      filepath.Join(cacheRoot, "downloads", fmt.Sprintf("frp_%s_%s.tar.gz", manifest.Version, pinnedPlatformKey)),
		TarballSHA256:    digests.TarballSHA256,
		FrpsPath:         frpsPath,
		FrpsSHA256:       digests.FrpsSHA256,
		FrpsVersion:      frpsVersion,
		FrpcPath:         frpcPath,
		FrpcSHA256:       digests.FrpcSHA256,
		FrpcVersion:      frpcVersion,
		CacheArtifactSum: sha256Hex([]byte(digests.TarballSHA256 + "|" + digests.FrpsSHA256 + "|" + digests.FrpcSHA256)),
	}, nil
}

// emit writes the emittable gate evidence facts to w.
func (e *gateFRPEvidence) emit(w io.Writer) {
	fmt.Fprintf(w, "GATE(EVIDENCE) §23.3 required gate — pinned FRP preconditions verified\n")
	fmt.Fprintf(w, "GATE(EVIDENCE) frp.version=%s platform=%s\n", e.Version, e.Platform)
	fmt.Fprintf(w, "GATE(EVIDENCE) manifest.sha256=%s\n", e.ManifestSHA256)
	fmt.Fprintf(w, "GATE(EVIDENCE) config_frps_toml.sha256=%s\n", e.ConfigSHA256)
	fmt.Fprintf(w, "GATE(EVIDENCE) tarball.path=%s tarball.sha256=%s tarball_matches_manifest=yes\n", e.TarballPath, e.TarballSHA256)
	fmt.Fprintf(w, "GATE(EVIDENCE) frps.path=%s frps.sha256=%s frps.version=%s\n", e.FrpsPath, e.FrpsSHA256, e.FrpsVersion)
	fmt.Fprintf(w, "GATE(EVIDENCE) frpc.path=%s frpc.sha256=%s frpc.version=%s\n", e.FrpcPath, e.FrpcSHA256, e.FrpcVersion)
	fmt.Fprintf(w, "GATE(EVIDENCE) cache_artifact_digest_sum=%s\n", e.CacheArtifactSum)
}

// ---------------------------------------------------------------------------
// Required-mode gate case registry
// ---------------------------------------------------------------------------

// requiredGateCases are the named §23.3 gate cases that MUST execute in
// required mode. Absence, a skip, or an incomplete run is a gate failure.
// Every real-FRP case in failure_test.go and relay_test.go is listed here; an
// unregistered case would run (or silently skip) without the required-mode gate
// noticing, so TestFailureSuiteCasesAreGateRegistered guards this list against
// omission.
var requiredGateCases = []string{
	"TestRealFRPRelayEndToEndTLS12HTTP11",
	"TestRealFRPRelayEndToEndTLS13HTTP2",
	"TestRealFRPFragmentedClientHelloReplay",
	"TestRealFRPExactRouting",
	"TestRealFRPContentParity",
	"TestRelayPathNeverEmitsOpenSignal",
	"TestRealFRPRelayPresenceHeartbeatDelayAndExpiry",
	"TestRealFRPGatewayRestartServesNothingUntilSnapshotAndPresence",
	"TestRealFRPFRPSRestartClearsPresenceAndTerminatesEstablishedStreams",
	"TestRealFRPAgentHTTPSReplacementAndFRPCRestartDuringIdleAndActiveTransfers",
	"TestRealFRPStaleSessionReplayDoesNotClearAHealthyReplacementTunnel",
	"TestRealFRPDirectOriginFailureMidTransferRecoversThroughCanonicalRelayLink",
	"TestRealFRPControlSyncLossPastRouteLeaseKeepsEstablishedStream",
	"TestRealFRPRevokeDuringLongTransferClosesEstablishedStream",
	"TestRealFRPPresenceExpiryWithoutCloseProxy",
}

type gateCaseRecord struct {
	started   bool
	completed bool
	skipped   bool
	failed    bool
}

type gateCaseRegistry struct {
	mu      sync.Mutex
	records map[string]*gateCaseRecord
}

var gateCases = &gateCaseRegistry{records: make(map[string]*gateCaseRecord)}

func (r *gateCaseRegistry) start(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record := r.records[name]
	if record == nil {
		record = &gateCaseRecord{}
		r.records[name] = record
	}
	record.started = true
}

// finish records a case's completion. skipped/failed are sticky across
// repeated iterations (-count=N) so a skip or failure in any iteration is
// reported at gate exit.
func (r *gateCaseRegistry) finish(name string, skipped, failed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record := r.records[name]
	if record == nil {
		record = &gateCaseRecord{}
		r.records[name] = record
	}
	record.completed = true
	record.skipped = record.skipped || skipped
	record.failed = record.failed || failed
}

// violations reports every named required gate case that did not execute
// cleanly against the package's declared required set.
func (r *gateCaseRegistry) violations() []string {
	return r.violationsFor(requiredGateCases)
}

// violationsFor is the parameterized form of violations; it exists so the
// required-mode registry's missing/skipped/failed enforcement can be unit
// tested without mutating the package's declared required set.
func (r *gateCaseRegistry) violationsFor(required []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var violations []string
	for _, name := range required {
		record := r.records[name]
		switch {
		case record == nil || !record.started:
			violations = append(violations, name+": did not execute (never started)")
		case record.skipped:
			violations = append(violations, name+": skipped")
		case record.failed:
			violations = append(violations, name+": failed")
		case !record.completed:
			violations = append(violations, name+": did not complete")
		}
	}
	return violations
}

// beginGateCase registers a named gate case with the required-mode registry
// and returns the deferred completion recorder. Call it as the FIRST statement
// of every gate case (`defer beginGateCase(t)()`) so a skip or an early exit is
// visible at gate exit.
func beginGateCase(t *testing.T) func() {
	t.Helper()
	name := t.Name()
	gateCases.start(name)
	return func() { gateCases.finish(name, t.Skipped(), t.Failed()) }
}

// gateRequired reports whether the non-skippable gate mode is active.
func gateRequired() bool { return os.Getenv(gateModeEnv) == gateModeRequired }

// relayRootAbs resolves the relay module root from the package test working
// directory without a testing.T (for TestMain).
func relayRootAbs() (string, error) {
	workingDir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Abs(filepath.Join(workingDir, "..", ".."))
}

// TestMain enforces the §23.3 gate contract. Default mode keeps the suite
// skippable; required mode fails closed before running any case when the
// integration env is unset or the pinned artifacts are missing, unpinned,
// corrupted, or poisoned, and fails after running when any named gate case did
// not execute to completion.
func TestMain(m *testing.M) {
	mode := os.Getenv(gateModeEnv)
	if mode != "" && mode != gateModeRequired {
		fmt.Fprintf(os.Stderr, "FAIL: %s=%q is not supported; the only gate mode is %s=%s\n",
			gateModeEnv, mode, gateModeEnv, gateModeRequired)
		os.Exit(2)
	}
	required := mode == gateModeRequired
	if required {
		if os.Getenv(integrationEnv) != "1" {
			fmt.Fprintf(os.Stderr,
				"FAIL: required gate mode (%s=%s) needs %s=1; without it every named §23.3 case would SKIP and the gate would be forgeable by omission\n",
				gateModeEnv, gateModeRequired, integrationEnv)
			os.Exit(2)
		}
		root, err := relayRootAbs()
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: required gate mode could not resolve the relay root: %v\n", err)
			os.Exit(2)
		}
		evidence, err := verifyPinnedBinariesAtGate(root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: required gate mode preconditions not met (pinned FRP binaries are not present and integrity-checked): %v\n", err)
			os.Exit(2)
		}
		evidence.emit(os.Stdout)
	}

	code := m.Run()

	if required {
		violations := gateCases.violations()
		if len(violations) > 0 {
			fmt.Fprintf(os.Stderr, "FAIL: required gate mode: %d of %d named §23.3 gate case(s) did not execute cleanly:\n",
				len(violations), len(requiredGateCases))
			for _, violation := range violations {
				fmt.Fprintf(os.Stderr, "  - %s\n", violation)
			}
			os.Exit(2)
		}
		fmt.Fprintf(os.Stdout, "GATE(EVIDENCE) all %d named §23.3 gate cases executed to completion\n", len(requiredGateCases))
	}
	os.Exit(code)
}

// ---------------------------------------------------------------------------
// Gate-mode enforcement tests
// ---------------------------------------------------------------------------

// goTestSample is one subprocess `go test` invocation's combined output and
// exit code.
type goTestSample struct {
	output   string
	exitCode int
}

// runIntegrationGoTest runs `go test ./internal/integration` from the relay
// root with the given extra args and environment overrides. `clear` names the
// env vars removed from the inherited environment before `set` is applied, so
// a gate-mode probe can never be contaminated by the harness's own env.
func runIntegrationGoTest(t *testing.T, args []string, clear []string, set map[string]string) goTestSample {
	t.Helper()
	cmd := exec.Command("go", append([]string{"test", "./internal/integration"}, args...)...)
	cmd.Dir = relayRoot(t)
	clearSet := make(map[string]bool, len(clear))
	for _, key := range clear {
		clearSet[key] = true
	}
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if clearSet[key] {
			continue
		}
		env = append(env, entry)
	}
	for key, value := range set {
		env = append(env, key+"="+value)
	}
	cmd.Env = env
	combined, err := cmd.CombinedOutput()
	if err == nil {
		return goTestSample{output: string(combined), exitCode: 0}
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return goTestSample{output: string(combined), exitCode: exitErr.ExitCode()}
	}
	t.Fatalf("running go test: %v\n%s", err, combined)
	return goTestSample{}
}

// TestGateRequiredModeFailsClosedWithoutIntegrationEnv is the regression for
// the reviewed defect: before the gate existed, an unset
// SHAREBRIDGE_FRP_INTEGRATION made all seven named gate tests SKIP and the
// package exit 0, so §23.3 could be cited as GO without a single assertion
// running. Required mode must exit non-zero with a clear message; default mode
// must still skip (so plain `go test ./...` stays usable).
func TestGateRequiredModeFailsClosedWithoutIntegrationEnv(t *testing.T) {
	const probeCase = "TestRealFRPRelayEndToEndTLS12HTTP11"
	args := []string{"-run", "^" + probeCase + "$", "-count=1", "-v"}

	t.Run("required mode without the integration env fails closed", func(t *testing.T) {
		sample := runIntegrationGoTest(t, args, []string{integrationEnv}, map[string]string{
			gateModeEnv: gateModeRequired,
		})
		if sample.exitCode == 0 {
			t.Fatalf("required gate mode exited 0 with %s unset; the gate is forgeable by omission.\noutput:\n%s",
				integrationEnv, sample.output)
		}
		if !strings.Contains(sample.output, gateModeRequired) {
			t.Errorf("required-mode failure does not name %s=%s:\n%s", gateModeEnv, gateModeRequired, sample.output)
		}
		if !strings.Contains(sample.output, integrationEnv) {
			t.Errorf("required-mode failure does not name the missing %s:\n%s", integrationEnv, sample.output)
		}
		if strings.Contains(sample.output, "=== RUN") {
			t.Errorf("required-mode failure still started cases:\n%s", sample.output)
		}
	})

	t.Run("default mode without the integration env still skips", func(t *testing.T) {
		sample := runIntegrationGoTest(t, args, []string{integrationEnv, gateModeEnv}, nil)
		if sample.exitCode != 0 {
			t.Fatalf("default mode exited %d with %s unset; plain `go test ./...` must keep skipping.\noutput:\n%s",
				sample.exitCode, integrationEnv, sample.output)
		}
		if !strings.Contains(sample.output, "SKIP") {
			t.Errorf("default-mode run did not report a SKIP for %s:\n%s", probeCase, sample.output)
		}
	})

	t.Run("unsupported gate mode is rejected", func(t *testing.T) {
		sample := runIntegrationGoTest(t, args, []string{integrationEnv}, map[string]string{
			gateModeEnv: "1",
		})
		if sample.exitCode == 0 {
			t.Fatalf("%s=1 was accepted; only %s=%s is a supported gate mode.\noutput:\n%s",
				gateModeEnv, gateModeEnv, gateModeRequired, sample.output)
		}
	})
}

// task33GateCases are the failure/recovery cases this task added. They must be
// un-citable as green evidence without the integration env, exactly like the
// original §23.3 cases.
var task33GateCases = []string{
	"TestRealFRPGatewayRestartServesNothingUntilSnapshotAndPresence",
	"TestRealFRPFRPSRestartClearsPresenceAndTerminatesEstablishedStreams",
	"TestRealFRPAgentHTTPSReplacementAndFRPCRestartDuringIdleAndActiveTransfers",
	"TestRealFRPStaleSessionReplayDoesNotClearAHealthyReplacementTunnel",
	"TestRealFRPDirectOriginFailureMidTransferRecoversThroughCanonicalRelayLink",
	"TestRealFRPControlSyncLossPastRouteLeaseKeepsEstablishedStream",
	"TestRealFRPRevokeDuringLongTransferClosesEstablishedStream",
	"TestRealFRPPresenceExpiryWithoutCloseProxy",
}

// TestGateRequiredModeCoversTask33Cases is the negative control for the
// reviewed defect that the Task-33 cases were skippable-as-pass: with no
// integration env, required mode must fail closed BEFORE running any of them,
// so a green/skipped package result can never be cited as their evidence.
func TestGateRequiredModeCoversTask33Cases(t *testing.T) {
	sample := runIntegrationGoTest(t,
		[]string{"-run", "^(" + strings.Join(task33GateCases, "|") + ")$", "-count=1", "-v"},
		[]string{integrationEnv}, map[string]string{gateModeEnv: gateModeRequired})
	if sample.exitCode == 0 {
		t.Fatalf("required gate mode exited 0 with %s unset for the Task-33 cases; they could be cited as evidence without running.\noutput:\n%s",
			integrationEnv, sample.output)
	}
	if strings.Contains(sample.output, "=== RUN") {
		t.Errorf("required-mode failure still started Task-33 cases:\n%s", sample.output)
	}
	if !strings.Contains(sample.output, integrationEnv) {
		t.Errorf("required-mode failure does not name the missing %s:\n%s", integrationEnv, sample.output)
	}
}

// TestFailureSuiteCasesAreGateRegistered is the anti-omission guard: every
// real-FRP case defined in failure_test.go and relay_test.go must call
// beginGateCase and appear in requiredGateCases. Without this, a new case (or
// a dropped registration) could run or skip outside the required-mode gate and
// still be reported as evidence.
func TestFailureSuiteCasesAreGateRegistered(t *testing.T) {
	required := make(map[string]bool, len(requiredGateCases))
	for _, name := range requiredGateCases {
		required[name] = true
	}
	parsed := 0
	for _, path := range []string{"failure_test.go", "relay_test.go"} {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || !strings.HasPrefix(function.Name.Name, "Test") {
				continue
			}
			parsed++
			if !required[function.Name.Name] {
				t.Errorf("%s: %s is not registered in requiredGateCases; an unset env or a dropped case could be cited as evidence", path, function.Name.Name)
			}
			if !callsBeginGateCase(function) {
				t.Errorf("%s: %s does not call beginGateCase; a skip or early exit would be invisible at gate exit", path, function.Name.Name)
			}
		}
	}
	if parsed != len(requiredGateCases) {
		t.Fatalf("parsed %d real-FRP test functions but requiredGateCases lists %d", parsed, len(requiredGateCases))
	}
}

// callsBeginGateCase reports whether the function body contains a
// beginGateCase call (deferred or otherwise).
func callsBeginGateCase(function *ast.FuncDecl) bool {
	found := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if identifier, ok := call.Fun.(*ast.Ident); ok && identifier.Name == "beginGateCase" {
			found = true
		}
		return true
	})
	return found
}

// TestGateCaseRegistryFailsClosedOnMissingSkippedAndFailedCases is the
// in-process negative control for the required-mode registry: a case that
// never started, a skipped case, and a failed case are all violations, while a
// cleanly completed case is not.
func TestGateCaseRegistryFailsClosedOnMissingSkippedAndFailedCases(t *testing.T) {
	registry := &gateCaseRegistry{records: make(map[string]*gateCaseRecord)}
	registry.start("TestClean")
	registry.finish("TestClean", false, false)
	registry.start("TestSkipped")
	registry.finish("TestSkipped", true, false)
	registry.start("TestFailed")
	registry.finish("TestFailed", false, true)
	// TestMissing never starts.

	violations := registry.violationsFor([]string{"TestClean", "TestSkipped", "TestFailed", "TestMissing"})
	joined := strings.Join(violations, "\n")
	if len(violations) != 3 {
		t.Fatalf("violations = %v, want exactly 3 (skipped, failed, missing)", violations)
	}
	for _, want := range []string{"TestSkipped: skipped", "TestFailed: failed", "TestMissing: did not execute"} {
		if !strings.Contains(joined, want) {
			t.Errorf("violations %v missing %q", violations, want)
		}
	}
	if strings.Contains(joined, "TestClean") {
		t.Errorf("a cleanly completed case was reported as a violation: %v", violations)
	}
}

// poisonedFRPCache builds a cache directory that satisfies the fetch script's
// legacy trust check (a version+digest-keyed directory holding executables plus
// a matching .verified marker) while holding junk binaries and no verified
// tarball. Re-hashing the cached binaries against the pinned tarball must catch
// it.
func poisonedFRPCache(t *testing.T) string {
	t.Helper()
	manifest, _, err := loadPinnedManifest(relayRoot(t))
	if err != nil {
		t.Fatalf("load pinned FRP manifest: %v", err)
	}
	pin, err := pinnedArtifact(manifest, pinnedPlatformKey)
	if err != nil {
		t.Skipf("no pinned FRP artifact for %s: %v", pinnedPlatformKey, err)
	}
	cacheRoot := t.TempDir()
	keyDir := filepath.Join(cacheRoot, fmt.Sprintf("v%s-sha256-%s", manifest.Version, pin.SHA256))
	if err := os.MkdirAll(keyDir, 0o755); err != nil {
		t.Fatalf("create poisoned cache key dir: %v", err)
	}
	for _, name := range []string{"frps", "frpc"} {
		if err := os.WriteFile(filepath.Join(keyDir, name), []byte("poisoned-"+name), 0o755); err != nil {
			t.Fatalf("write poisoned %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(keyDir, ".verified"), []byte(pin.SHA256+"\n"), 0o644); err != nil {
		t.Fatalf("write poisoned .verified: %v", err)
	}
	return cacheRoot
}

// TestGateRequiredModeWithoutPinnedCacheFailsClosed proves the required-mode
// preconditions fail closed on a poisoned cache BEFORE any named case runs,
// instead of trusting the `.verified` marker and executable bits.
func TestGateRequiredModeWithoutPinnedCacheFailsClosed(t *testing.T) {
	const probeCase = "TestRealFRPFragmentedClientHelloReplay"
	sample := runIntegrationGoTest(t,
		[]string{"-run", "^" + probeCase + "$", "-count=1", "-v"},
		nil,
		map[string]string{
			integrationEnv:          "1",
			gateModeEnv:             gateModeRequired,
			"SHAREBRIDGE_FRP_CACHE": poisonedFRPCache(t),
		})
	if sample.exitCode == 0 {
		t.Fatalf("required gate mode exited 0 with a poisoned FRP cache; gate preconditions are not fail-closed.\noutput:\n%s",
			sample.output)
	}
	if strings.Contains(sample.output, "=== RUN") {
		t.Fatalf("required gate mode started running cases despite a poisoned cache; preconditions must fail before m.Run().\noutput:\n%s",
			sample.output)
	}
}

// ---------------------------------------------------------------------------
// Gate-time integrity unit tests
// ---------------------------------------------------------------------------

// TestGateIntegrityRejectsPoisonedCache is the regression for the reviewed
// cache-trust defect: a poisoned complete cache (executables present,
// `.verified` matching) must fail closed. The verifier re-hashes the tarball
// against the pinned digest and requires the cached executables to hash
// identically to the tarball's contents.
func TestGateIntegrityRejectsPoisonedCache(t *testing.T) {
	const version = "9.9.9"
	const platform = "test_arch"
	archiveRoot := "frp_" + version + "_" + platform
	realFRPS := []byte("real-frps-bytes")
	realFRPC := []byte("real-frpc-bytes")

	cacheRoot := t.TempDir()
	tarballPath := filepath.Join(cacheRoot, "downloads", "frp_"+version+"_"+platform+".tar.gz")
	writeSyntheticFRPTarball(t, tarballPath, map[string][]byte{
		archiveRoot + "/frps": realFRPS,
		archiveRoot + "/frpc": realFRPC,
	})
	tarballDigest := sha256FileHex(t, tarballPath)
	keyDir := filepath.Join(cacheRoot, "v"+version+"-sha256-"+tarballDigest)
	stageSyntheticBinary(t, keyDir, "frps", realFRPS)
	stageSyntheticBinary(t, keyDir, "frpc", realFRPC)

	if _, err := hashCacheArtifacts(cacheRoot, version, platform, tarballDigest); err != nil {
		t.Fatalf("intact synthetic cache rejected: %v", err)
	}

	t.Run("poisoned cached frps binary is rejected", func(t *testing.T) {
		stageSyntheticBinary(t, keyDir, "frps", []byte("poisoned-frps"))
		defer stageSyntheticBinary(t, keyDir, "frps", realFRPS)
		if _, err := hashCacheArtifacts(cacheRoot, version, platform, tarballDigest); err == nil {
			t.Fatal("poisoned cached frps binary was accepted")
		}
	})

	t.Run("poisoned cached frpc binary is rejected", func(t *testing.T) {
		stageSyntheticBinary(t, keyDir, "frpc", []byte("poisoned-frpc"))
		defer stageSyntheticBinary(t, keyDir, "frpc", realFRPC)
		if _, err := hashCacheArtifacts(cacheRoot, version, platform, tarballDigest); err == nil {
			t.Fatal("poisoned cached frpc binary was accepted")
		}
	})

	t.Run("tampered tarball is rejected", func(t *testing.T) {
		original, err := os.ReadFile(tarballPath)
		if err != nil {
			t.Fatalf("read tarball: %v", err)
		}
		tampered := append([]byte(nil), original...)
		tampered[len(tampered)/2] ^= 0xff
		if err := os.WriteFile(tarballPath, tampered, 0o644); err != nil {
			t.Fatalf("write tampered tarball: %v", err)
		}
		defer os.WriteFile(tarballPath, original, 0o644)
		if _, err := hashCacheArtifacts(cacheRoot, version, platform, tarballDigest); err == nil {
			t.Fatal("tampered tarball was accepted")
		}
	})

	t.Run("missing cached binary is rejected", func(t *testing.T) {
		if err := os.Rename(filepath.Join(keyDir, "frps"), filepath.Join(keyDir, "frps.bak")); err != nil {
			t.Fatalf("move frps aside: %v", err)
		}
		defer os.Rename(filepath.Join(keyDir, "frps.bak"), filepath.Join(keyDir, "frps"))
		if _, err := hashCacheArtifacts(cacheRoot, version, platform, tarballDigest); err == nil {
			t.Fatal("missing cached frps binary was accepted")
		}
	})

	t.Run("missing tarball is rejected", func(t *testing.T) {
		if err := os.Rename(tarballPath, tarballPath+".bak"); err != nil {
			t.Fatalf("move tarball aside: %v", err)
		}
		defer os.Rename(tarballPath+".bak", tarballPath)
		if _, err := hashCacheArtifacts(cacheRoot, version, platform, tarballDigest); err == nil {
			t.Fatal("missing tarball was accepted")
		}
	})
}

// TestPinnedArtifactResolutionFailsClosed proves the manifest lookup fails
// closed on a missing entry, a non-immutable URL, and a malformed digest.
func TestPinnedArtifactResolutionFailsClosed(t *testing.T) {
	goodDigest := strings.Repeat("a", 64)
	goodURL := "https://github.com/fatedier/frp/releases/download/v0.71.0/frp_0.71.0_darwin_arm64.tar.gz"
	manifest := frpManifest{
		Version: "0.71.0",
		Artifacts: map[string]frpArtifactPin{
			"darwin_arm64": {URL: goodURL, SHA256: goodDigest},
		},
	}
	if _, err := pinnedArtifact(manifest, "darwin_arm64"); err != nil {
		t.Fatalf("valid pinned artifact rejected: %v", err)
	}
	if _, err := pinnedArtifact(manifest, "plan9_mips"); err == nil {
		t.Fatal("missing manifest entry did not fail closed")
	}
	badURL := frpManifest{Version: "0.71.0", Artifacts: map[string]frpArtifactPin{
		"darwin_arm64": {URL: "https://example.com/latest.tar.gz", SHA256: goodDigest},
	}}
	if _, err := pinnedArtifact(badURL, "darwin_arm64"); err == nil {
		t.Fatal("non-pinned URL did not fail closed")
	}
	badDigest := frpManifest{Version: "0.71.0", Artifacts: map[string]frpArtifactPin{
		"darwin_arm64": {URL: goodURL, SHA256: "not-a-digest"},
	}}
	if _, err := pinnedArtifact(badDigest, "darwin_arm64"); err == nil {
		t.Fatal("malformed digest did not fail closed")
	}
	badVersion := frpManifest{Version: "latest", Artifacts: map[string]frpArtifactPin{
		"darwin_arm64": {URL: goodURL, SHA256: goodDigest},
	}}
	if _, err := pinnedArtifact(badVersion, "darwin_arm64"); err == nil {
		t.Fatal("non-immutable version did not fail closed")
	}
}

// writeSyntheticFRPTarball writes a deterministic .tar.gz with the given
// entry-name -> content mapping (test fixture for the cache verifier).
func writeSyntheticFRPTarball(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create tarball dir: %v", err)
	}
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create tarball: %v", err)
	}
	defer file.Close()
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		content := entries[name]
		header := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatalf("write tar header %s: %v", name, err)
		}
		if _, err := tarWriter.Write(content); err != nil {
			t.Fatalf("write tar body %s: %v", name, err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}
}

// stageSyntheticBinary writes one executable into a keyed cache directory.
func stageSyntheticBinary(t *testing.T, dir, name string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create cache key dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), content, 0o755); err != nil {
		t.Fatalf("stage %s: %v", name, err)
	}
}
