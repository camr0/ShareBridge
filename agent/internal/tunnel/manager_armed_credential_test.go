package tunnel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hasReasonContaining reports whether any collected status report's reason
// contains substring. It lets a test observe a specific diagnostic (for
// example the armed-config write failure) without exporting manager state.
func (collector *statusCollector) hasReasonContaining(substring string) bool {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	for _, report := range collector.reports {
		if strings.Contains(report.Reason, substring) {
			return true
		}
	}
	return false
}

// TestManagerRecoversFromTransientArmedConfigWriteFailure pins the stuck state
// found in review: ApplyConfig publishes HasArmedCredential=true synchronously
// (so a sequential caller never re-requests a credential control just
// delivered), but the event loop's arm path returned without scheduling a
// retry when writeArmedConfig failed. The manager was then never armed in
// effect while HasArmedCredential kept reporting true, so the daemon's
// reconnect-based re-request was suppressed forever and frpc never started.
//
// The seam is the real filesystem: the config's parent is a regular file, so
// the first write fails closed; deleting the blocker clears the transient
// failure and the manager must retry on its own.
func TestManagerRecoversFromTransientArmedConfigWriteFailure(t *testing.T) {
	dataDirectory := t.TempDir()
	blockerPath := filepath.Join(dataDirectory, "blocked-parent")
	if err := os.WriteFile(blockerPath, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}
	settings := Settings{
		FRPCBinaryPath: "/opt/sharebridge/bin/frpc",
		ConfigPath:     filepath.Join(blockerPath, "frpc.toml"),
		TrustedCAFile:  filepath.Join(dataDirectory, "relay-ca.pem"),
		LocalTarget:    LocalTarget,
	}
	starter := &recordingStarter{}
	collector := &statusCollector{}
	manager := newTestManager(t, settings, starter, &fakeCredentialRequester{}, collector)

	if err := manager.ApplyConfig(validTestConfig()); err != nil {
		t.Fatalf("ApplyConfig() error = %v", err)
	}
	// Duplicate suppression must be unchanged: the credential is published
	// synchronously even though the write has not happened yet.
	if !manager.HasArmedCredential() {
		t.Fatal("ApplyConfig() did not publish the armed credential synchronously")
	}
	// Observe the first write failure: without a scheduled retry the manager
	// is now permanently un-armed in effect.
	waitForCondition(t, "first armed-config write failure", time.Second, func() bool {
		return collector.hasReasonContaining("write tunnel config")
	})
	if got := starter.startCount(); got != 0 {
		t.Fatalf("frpc started %d time(s) despite the write failure, want 0", got)
	}

	// Clear the transient failure; the manager must recover without any
	// external trigger.
	if err := os.Remove(blockerPath); err != nil {
		t.Fatalf("clear blocker: %v", err)
	}
	waitForCondition(t, "frpc start after the armed-config write failure clears", 2*time.Second, func() bool {
		return starter.startCount() == 1
	})
	if !manager.HasArmedCredential() {
		t.Fatal("recovered manager lost its armed credential")
	}
}
