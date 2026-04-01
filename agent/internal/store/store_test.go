package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// TestNew_CreatesMissingDir verifies that New() creates the data directory
// if it does not exist.
func TestNew_CreatesMissingDir(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "nonexistent", "subdir")

	t.Setenv("OPENCLOUDSHARE_DATA_DIR", dataDir)

	_, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	// Verify directory was created
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		t.Errorf("Expected directory %s to be created, but it does not exist", dataDir)
	}
}

// TestNew_MalformedJSON_Fatal verifies that New() returns an error when
// sessions.json exists but contains invalid JSON.
func TestNew_MalformedJSON_Fatal(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("OPENCLOUDSHARE_DATA_DIR", tmpDir)

	// Create malformed JSON file
	sessionsFile := filepath.Join(tmpDir, "sessions.json")
	malformedJSON := "{ invalid json content"
	if err := os.WriteFile(sessionsFile, []byte(malformedJSON), 0600); err != nil {
		t.Fatalf("Failed to create malformed sessions.json: %v", err)
	}

	_, err := New()
	if err == nil {
		t.Error("Expected New() to return error for malformed JSON, but got nil")
	}
}

// TestGetCode_MissingFile verifies that GetCode returns empty string when
// sessions.json does not exist.
func TestGetCode_MissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("OPENCLOUDSHARE_DATA_DIR", tmpDir)

	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	code := store.GetCode("https://example.com/share/abc")
	if code != "" {
		t.Errorf("Expected empty code for missing file, got %q", code)
	}
}

// TestSetCode_GetCode_RoundTrip verifies that SetCode and GetCode work
// together to store and retrieve the same value.
func TestSetCode_GetCode_RoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("OPENCLOUDSHARE_DATA_DIR", tmpDir)

	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	shareURL := "https://example.com/share/abc"
	expectedCode := "xyz123"

	if err := store.SetCode(shareURL, expectedCode); err != nil {
		t.Fatalf("SetCode failed: %v", err)
	}

	gotCode := store.GetCode(shareURL)
	if gotCode != expectedCode {
		t.Errorf("GetCode returned %q, expected %q", gotCode, expectedCode)
	}
}

// TestSetCode_MultipleURLs verifies that different shareURLs are stored
// independently and can both be retrieved.
func TestSetCode_MultipleURLs(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("OPENCLOUDSHARE_DATA_DIR", tmpDir)

	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	url1 := "https://example.com/share/first"
	url2 := "https://example.com/share/second"
	code1 := "code111"
	code2 := "code222"

	if err := store.SetCode(url1, code1); err != nil {
		t.Fatalf("SetCode for url1 failed: %v", err)
	}
	if err := store.SetCode(url2, code2); err != nil {
		t.Fatalf("SetCode for url2 failed: %v", err)
	}

	gotCode1 := store.GetCode(url1)
	gotCode2 := store.GetCode(url2)

	if gotCode1 != code1 {
		t.Errorf("GetCode(url1) returned %q, expected %q", gotCode1, code1)
	}
	if gotCode2 != code2 {
		t.Errorf("GetCode(url2) returned %q, expected %q", gotCode2, code2)
	}
}

// TestGetDownloadCount_MissingFile verifies that GetDownloadCount returns 0
// when sessions.json does not exist.
func TestGetDownloadCount_MissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("OPENCLOUDSHARE_DATA_DIR", tmpDir)

	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	count := store.GetDownloadCount("https://example.com/share/abc")
	if count != 0 {
		t.Errorf("Expected download count 0 for missing file, got %d", count)
	}
}

// TestIncrementDownloadCount verifies that IncrementDownloadCount correctly
// increments and returns the count from 0->1 and then 1->2.
func TestIncrementDownloadCount(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("OPENCLOUDSHARE_DATA_DIR", tmpDir)

	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	shareURL := "https://example.com/share/abc"

	// First increment: 0 -> 1
	count, err := store.IncrementDownloadCount(shareURL)
	if err != nil {
		t.Fatalf("First IncrementDownloadCount failed: %v", err)
	}
	if count != 1 {
		t.Errorf("First increment returned %d, expected 1", count)
	}

	// Second increment: 1 -> 2
	count, err = store.IncrementDownloadCount(shareURL)
	if err != nil {
		t.Fatalf("Second IncrementDownloadCount failed: %v", err)
	}
	if count != 2 {
		t.Errorf("Second increment returned %d, expected 2", count)
	}

	// Verify GetDownloadCount also returns 2
	gotCount := store.GetDownloadCount(shareURL)
	if gotCount != 2 {
		t.Errorf("GetDownloadCount returned %d, expected 2", gotCount)
	}
}

// TestSetCode_FilePermissions verifies that sessions.json is created with
// 0600 permissions (readable and writable only by owner).
func TestSetCode_FilePermissions(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("OPENCLOUDSHARE_DATA_DIR", tmpDir)

	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	// Trigger file creation by setting a code
	if err := store.SetCode("https://example.com/share/abc", "testcode"); err != nil {
		t.Fatalf("SetCode failed: %v", err)
	}

	sessionsFile := filepath.Join(tmpDir, "sessions.json")
	info, err := os.Stat(sessionsFile)
	if err != nil {
		t.Fatalf("Failed to stat sessions.json: %v", err)
	}

	// Check permissions - should be 0600
	mode := info.Mode().Perm()
	expectedMode := os.FileMode(0600)
	if mode != expectedMode {
		t.Errorf("sessions.json has permissions %o, expected %o", mode, expectedMode)
	}
}

// TestGetCode_MidRunCorruption verifies that GetCode returns empty string
// when the file becomes corrupted after the store was created (mid-run).
// This should not panic.
func TestGetCode_MidRunCorruption(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("OPENCLOUDSHARE_DATA_DIR", tmpDir)

	// Create store and set a code
	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	shareURL := "https://example.com/share/abc"
	if err := store.SetCode(shareURL, "validcode"); err != nil {
		t.Fatalf("SetCode failed: %v", err)
	}

	// Verify code was stored
	code := store.GetCode(shareURL)
	if code != "validcode" {
		t.Fatalf("Expected code 'validcode', got %q", code)
	}

	// Corrupt the file after store creation
	sessionsFile := filepath.Join(tmpDir, "sessions.json")
	if err := os.WriteFile(sessionsFile, []byte("{ invalid json"), 0600); err != nil {
		t.Fatalf("Failed to corrupt sessions.json: %v", err)
	}

	// GetCode should return empty string without panicking
	code = store.GetCode(shareURL)
	if code != "" {
		t.Errorf("Expected empty code after corruption, got %q", code)
	}
}

func TestAgentID_GeneratedOnFirstLoad(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("OPENCLOUDSHARE_DATA_DIR", tmpDir)

	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	// First call should generate and return a UUID
	id1 := store.GetAgentID()
	if id1 == "" {
		t.Fatal("GetAgentID() returned empty string on first call")
	}

	// Verify it's a valid UUID format
	if _, err := uuid.Parse(id1); err != nil {
		t.Fatalf("GetAgentID() returned non-UUID: %q, error: %v", id1, err)
	}

	// Create new store instance (simulating restart)
	store2, err := New()
	if err != nil {
		t.Fatalf("New() second call failed: %v", err)
	}

	// Should return same ID
	id2 := store2.GetAgentID()
	if id1 != id2 {
		t.Fatalf("AgentID changed after restart: %q -> %q", id1, id2)
	}
}
