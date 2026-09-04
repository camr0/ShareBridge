// Package frptest verifies and shares the pinned FRP release artifacts that
// back the relay integration tests. The release pin lives in
// relay/frp/manifest.json; relay/scripts/fetch-frp.sh downloads and
// checksum-verifies that exact release into a local cache. No FRP binary is
// ever committed to the repository — release packaging bundles executables
// fetched and verified through this path.
package frptest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// frpReleaseManifest mirrors relay/frp/manifest.json, the committed pin record
// for the exact upstream FRP release this module is tested and shipped against.
type frpReleaseManifest struct {
	Version   string                     `json:"version"`
	PinStatus string                     `json:"pin_status"`
	Note      string                     `json:"note"`
	Artifacts map[string]frpArtifactPin `json:"artifacts"`
}

// frpArtifactPin is one platform's pinned upstream URL and SHA-256 digest.
type frpArtifactPin struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// supportedFRPPlatforms are the developer, CI, and release target platforms.
var supportedFRPPlatforms = []string{"darwin_arm64", "linux_amd64", "linux_arm64"}

// allowedFRPPinStatuses enumerates the ledger vocabulary for the release pin.
var allowedFRPPinStatuses = map[string]bool{
	"provisional-pending-user-approval": true,
	"approved":                          true,
}

var immutableReleasePattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func relayRootDir(t *testing.T) string {
	t.Helper()
	rootDir, resolveErr := filepath.Abs(filepath.Join("..", ".."))
	if resolveErr != nil {
		t.Fatalf("resolving relay root directory: %v", resolveErr)
	}
	return rootDir
}

func loadFRPManifest(t *testing.T) frpReleaseManifest {
	t.Helper()
	manifestPath := filepath.Join(relayRootDir(t), "frp", "manifest.json")
	manifestBytes, readErr := os.ReadFile(manifestPath)
	if readErr != nil {
		t.Fatalf("reading pinned FRP manifest %s: %v", manifestPath, readErr)
	}
	var manifest frpReleaseManifest
	if parseErr := json.Unmarshal(manifestBytes, &manifest); parseErr != nil {
		t.Fatalf("parsing pinned FRP manifest %s: %v", manifestPath, parseErr)
	}
	return manifest
}

// resolveFRPCacheRoot mirrors the fetch script's cache resolution:
// SHAREBRIDGE_FRP_CACHE when set, otherwise <relay-root>/.cache/frp.
func resolveFRPCacheRoot(t *testing.T) string {
	t.Helper()
	if cacheOverride := os.Getenv("SHAREBRIDGE_FRP_CACHE"); cacheOverride != "" {
		absoluteCache, resolveErr := filepath.Abs(cacheOverride)
		if resolveErr != nil {
			t.Fatalf("resolving SHAREBRIDGE_FRP_CACHE %s: %v", cacheOverride, resolveErr)
		}
		return absoluteCache
	}
	return filepath.Join(relayRootDir(t), ".cache", "frp")
}

// runFetchFRPScript executes the fetch script with the given cache override
// and returns its stdout, stderr, and run error.
func runFetchFRPScript(t *testing.T, cacheRootDir string) (string, string, error) {
	t.Helper()
	scriptPath := filepath.Join(relayRootDir(t), "scripts", "fetch-frp.sh")
	var stdoutBuffer, stderrBuffer bytes.Buffer
	fetchCommand := exec.Command(scriptPath)
	fetchCommand.Env = append(os.Environ(), "SHAREBRIDGE_FRP_CACHE="+cacheRootDir)
	fetchCommand.Stdout = &stdoutBuffer
	fetchCommand.Stderr = &stderrBuffer
	runErr := fetchCommand.Run()
	return stdoutBuffer.String(), stderrBuffer.String(), runErr
}

func sha256OfFile(t *testing.T, path string) string {
	t.Helper()
	fileBytes, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("reading %s for hashing: %v", path, readErr)
	}
	digest := sha256.Sum256(fileBytes)
	return hex.EncodeToString(digest[:])
}

func TestPinnedFRPArtifactsVerifyChecksums(t *testing.T) {
	manifest := loadFRPManifest(t)
	currentPlatformKey := runtime.GOOS + "_" + runtime.GOARCH
	currentArtifact, platformSupported := manifest.Artifacts[currentPlatformKey]
	if !platformSupported {
		t.Skipf("no pinned FRP artifact for %s; supported platforms: %v",
			currentPlatformKey, supportedFRPPlatforms)
	}

	t.Run("manifest names one immutable release with a recorded pin status", func(t *testing.T) {
		if !immutableReleasePattern.MatchString(manifest.Version) {
			t.Errorf("manifest version %q is not an immutable x.y.z release tag", manifest.Version)
		}
		if !allowedFRPPinStatuses[manifest.PinStatus] {
			t.Errorf("manifest pin_status %q is not one of the allowed values %+v",
				manifest.PinStatus, allowedFRPPinStatuses)
		}
	})

	t.Run("manifest supplies exact upstream URLs and SHA-256 for all supported platforms", func(t *testing.T) {
		if len(manifest.Artifacts) != len(supportedFRPPlatforms) {
			t.Errorf("manifest pins %d artifacts; want exactly %d for %v",
				len(manifest.Artifacts), len(supportedFRPPlatforms), supportedFRPPlatforms)
		}
		for _, platformKey := range supportedFRPPlatforms {
			artifactPin, present := manifest.Artifacts[platformKey]
			if !present {
				t.Errorf("manifest is missing a pin for platform %s", platformKey)
				continue
			}
			expectedURL := fmt.Sprintf(
				"https://github.com/fatedier/frp/releases/download/v%s/frp_%s_%s.tar.gz",
				manifest.Version, manifest.Version, platformKey)
			if artifactPin.URL != expectedURL {
				t.Errorf("platform %s URL %q is not the exact pinned release asset %q",
					platformKey, artifactPin.URL, expectedURL)
			}
			if !sha256HexPattern.MatchString(artifactPin.SHA256) {
				t.Errorf("platform %s SHA-256 %q is not 64 lowercase hex characters",
					platformKey, artifactPin.SHA256)
			}
		}
	})

	cacheRootDir := resolveFRPCacheRoot(t)
	expectedKeyDir := filepath.Join(cacheRootDir,
		fmt.Sprintf("v%s-sha256-%s", manifest.Version, currentArtifact.SHA256))
	tarballPath := filepath.Join(cacheRootDir, "downloads",
		fmt.Sprintf("frp_%s_%s.tar.gz", manifest.Version, currentPlatformKey))

	t.Run("fetch verifies and stages both executables in a version+digest-keyed cache", func(t *testing.T) {
		stdoutText, stderrText, runErr := runFetchFRPScript(t, cacheRootDir)
		if runErr != nil {
			t.Fatalf("fetch script failed: %v\nstderr:\n%s", runErr, stderrText)
		}
		if reportedKeyDir := strings.TrimSpace(stdoutText); reportedKeyDir != expectedKeyDir {
			t.Errorf("fetch script stdout %q does not report the version+digest-keyed cache directory %q",
				reportedKeyDir, expectedKeyDir)
		}
		for _, binaryName := range []string{"frps", "frpc"} {
			binaryPath := filepath.Join(expectedKeyDir, binaryName)
			binaryInfo, statErr := os.Stat(binaryPath)
			if statErr != nil {
				t.Errorf("expected executable %s in the keyed cache: %v", binaryPath, statErr)
				continue
			}
			if binaryInfo.Mode()&0o111 == 0 {
				t.Errorf("staged binary %s is not executable", binaryPath)
			}
		}
		verifiedMarkerPath := filepath.Join(expectedKeyDir, ".verified")
		if markerDigest, readErr := os.ReadFile(verifiedMarkerPath); readErr != nil {
			t.Errorf("reading verification marker %s: %v", verifiedMarkerPath, readErr)
		} else if strings.TrimSpace(string(markerDigest)) != currentArtifact.SHA256 {
			t.Errorf("verification marker records digest %q; want pinned %q",
				strings.TrimSpace(string(markerDigest)), currentArtifact.SHA256)
		}
	})

	t.Run("cached fetch does not re-download", func(t *testing.T) {
		tarballBefore, statErr := os.Stat(tarballPath)
		if statErr != nil {
			t.Fatalf("verified tarball %s missing before cached rerun: %v", tarballPath, statErr)
		}
		stdoutText, stderrText, runErr := runFetchFRPScript(t, cacheRootDir)
		if runErr != nil {
			t.Fatalf("cached fetch script failed: %v\nstderr:\n%s", runErr, stderrText)
		}
		if reportedKeyDir := strings.TrimSpace(stdoutText); reportedKeyDir != expectedKeyDir {
			t.Errorf("cached fetch stdout %q does not report %q", reportedKeyDir, expectedKeyDir)
		}
		tarballAfter, statErr := os.Stat(tarballPath)
		if statErr != nil {
			t.Fatalf("verified tarball %s missing after cached rerun: %v", tarballPath, statErr)
		}
		if !tarballAfter.ModTime().Equal(tarballBefore.ModTime()) {
			t.Errorf("cached rerun re-downloaded the tarball (modification time changed from %v to %v)",
				tarballBefore.ModTime(), tarballAfter.ModTime())
		}
	})

	t.Run("tampered artifact is rejected and removed", func(t *testing.T) {
		tamperCacheDir := t.TempDir()
		tamperDownloadsDir := filepath.Join(tamperCacheDir, "downloads")
		if mkdirErr := os.MkdirAll(tamperDownloadsDir, 0o755); mkdirErr != nil {
			t.Fatalf("creating tamper cache downloads directory: %v", mkdirErr)
		}
		tamperedTarballPath := filepath.Join(tamperDownloadsDir, filepath.Base(tarballPath))
		tarballBytes, readErr := os.ReadFile(tarballPath)
		if readErr != nil {
			t.Fatalf("reading verified tarball %s as tamper source: %v (wipe the whole cache directory and rerun)", tarballPath, readErr)
		}
		tamperedBytes := append([]byte(nil), tarballBytes...)
		flipIndex := len(tamperedBytes) / 2
		tamperedBytes[flipIndex] ^= 0xFF
		if writeErr := os.WriteFile(tamperedTarballPath, tamperedBytes, 0o644); writeErr != nil {
			t.Fatalf("writing tampered tarball: %v", writeErr)
		}
		if sha256OfFile(t, tamperedTarballPath) == currentArtifact.SHA256 {
			t.Fatal("tamper setup failed: flipped tarball still matches the pinned digest")
		}

		stdoutText, stderrText, runErr := runFetchFRPScript(t, tamperCacheDir)
		if runErr == nil {
			t.Fatalf("tampered artifact was accepted by the fetch script\nstdout:\n%s", stdoutText)
		}
		if !strings.Contains(stderrText, "mismatch") {
			t.Errorf("fetch script rejection does not report a checksum mismatch:\n%s", stderrText)
		}
		if _, statErr := os.Stat(tamperedTarballPath); !os.IsNotExist(statErr) {
			t.Errorf("tampered tarball %s was not removed after rejection", tamperedTarballPath)
		}
		tamperedKeyDir := filepath.Join(tamperCacheDir,
			fmt.Sprintf("v%s-sha256-%s", manifest.Version, currentArtifact.SHA256))
		for _, binaryName := range []string{"frps", "frpc"} {
			if _, statErr := os.Stat(filepath.Join(tamperedKeyDir, binaryName)); statErr == nil {
				t.Errorf("rejected artifact still staged %s into %s", binaryName, tamperedKeyDir)
			}
		}
	})

	t.Run("frps and frpc report the exact manifest version", func(t *testing.T) {
		for _, binaryName := range []string{"frps", "frpc"} {
			binaryPath := filepath.Join(expectedKeyDir, binaryName)
			versionOutput, runErr := exec.Command(binaryPath, "--version").Output()
			if runErr != nil {
				t.Fatalf("%s --version failed: %v", binaryName, runErr)
			}
			if reportedVersion := strings.TrimSpace(string(versionOutput)); reportedVersion != manifest.Version {
				t.Errorf("%s reports version %q; want the pinned manifest version %q",
					binaryName, reportedVersion, manifest.Version)
			}
		}
	})
}
