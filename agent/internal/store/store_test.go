package store

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestNew_CreatesMissingDir verifies that New() creates the data directory
// if it does not exist.
func TestNew_CreatesMissingDir(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "nonexistent", "subdir")

	t.Setenv("SHAREBRIDGE_DATA_DIR", dataDir)

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
	t.Setenv("SHAREBRIDGE_DATA_DIR", tmpDir)

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

// TestGetSession_MissingFile verifies that GetSession returns nil when
// sessions.json does not exist.
func TestGetSession_MissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SHAREBRIDGE_DATA_DIR", tmpDir)

	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	session := store.GetSession("nonexistent")
	if session != nil {
		t.Errorf("Expected nil session for missing file, got %+v", session)
	}
}

// TestSaveSession_GetSession_RoundTrip verifies that SaveSession and GetSession work
// together to store and retrieve the same value.
func TestSaveSession_GetSession_RoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SHAREBRIDGE_DATA_DIR", tmpDir)

	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	now := time.Now()
	session := SessionEntry{
		Code:         "xyz123",
		ShareURL:     "https://example.com/share/abc",
		Password:     "secret",
		ExpiresAt:    now.Add(1 * time.Hour),
		MaxDownloads: 5,
		Downloads:    0,
		RelayOnly:    true,
		CreatedAt:    now,
	}

	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	gotSession := store.GetSession("xyz123")
	if gotSession == nil {
		t.Fatalf("GetSession returned nil")
	}

	if gotSession.Code != session.Code {
		t.Errorf("Code: got %q, expected %q", gotSession.Code, session.Code)
	}
	if gotSession.ShareURL != session.ShareURL {
		t.Errorf("ShareURL: got %q, expected %q", gotSession.ShareURL, session.ShareURL)
	}
	if gotSession.Password != session.Password {
		t.Errorf("Password: got %q, expected %q", gotSession.Password, session.Password)
	}
	if gotSession.MaxDownloads != session.MaxDownloads {
		t.Errorf("MaxDownloads: got %d, expected %d", gotSession.MaxDownloads, session.MaxDownloads)
	}
	if gotSession.Downloads != session.Downloads {
		t.Errorf("Downloads: got %d, expected %d", gotSession.Downloads, session.Downloads)
	}
	if gotSession.RelayOnly != session.RelayOnly {
		t.Errorf("RelayOnly: got %v, expected %v", gotSession.RelayOnly, session.RelayOnly)
	}
}

// TestSaveSession_GetSession_RoundTripShareType verifies that ShareType is
// persisted and returned unchanged.
func TestSaveSession_GetSession_RoundTripShareType(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SHAREBRIDGE_DATA_DIR", tmpDir)

	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	session := SessionEntry{
		Code:      "sharetype-code",
		ShareURL:  "https://example.com/share/sharetype",
		ShareType: "cloudwebdav",
		CreatedAt: time.Now(),
	}

	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	gotSession := store.GetSession("sharetype-code")
	if gotSession == nil {
		t.Fatalf("GetSession returned nil")
	}
	if gotSession.ShareType != session.ShareType {
		t.Errorf("ShareType: got %q, expected %q", gotSession.ShareType, session.ShareType)
	}
}

// TestSaveSession_MultipleSessions verifies that different sessions are stored
// independently and can both be retrieved.
func TestSaveSession_MultipleSessions(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SHAREBRIDGE_DATA_DIR", tmpDir)

	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	session1 := SessionEntry{
		Code:      "code111",
		ShareURL:  "https://example.com/share/first",
		CreatedAt: time.Now(),
	}
	session2 := SessionEntry{
		Code:      "code222",
		ShareURL:  "https://example.com/share/second",
		CreatedAt: time.Now(),
	}

	if err := store.SaveSession(session1); err != nil {
		t.Fatalf("SaveSession for session1 failed: %v", err)
	}
	if err := store.SaveSession(session2); err != nil {
		t.Fatalf("SaveSession for session2 failed: %v", err)
	}

	gotSession1 := store.GetSession("code111")
	gotSession2 := store.GetSession("code222")

	if gotSession1 == nil || gotSession1.ShareURL != session1.ShareURL {
		t.Errorf("GetSession(code111) returned wrong session: %+v", gotSession1)
	}
	if gotSession2 == nil || gotSession2.ShareURL != session2.ShareURL {
		t.Errorf("GetSession(code222) returned wrong session: %+v", gotSession2)
	}
}

// TestGetByShareURL verifies that GetByShareURL correctly retrieves sessions by URL.
func TestGetByShareURL(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SHAREBRIDGE_DATA_DIR", tmpDir)

	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	session := SessionEntry{
		Code:      "testcode",
		ShareURL:  "https://example.com/share/abc",
		CreatedAt: time.Now(),
	}

	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	gotSession := store.GetByShareURL("https://example.com/share/abc")
	if gotSession == nil {
		t.Fatalf("GetByShareURL returned nil")
	}

	if gotSession.Code != "testcode" {
		t.Errorf("GetByShareURL returned wrong code: got %q, expected %q", gotSession.Code, "testcode")
	}

	// Test non-existent URL
	notFound := store.GetByShareURL("https://example.com/share/nonexistent")
	if notFound != nil {
		t.Errorf("Expected nil for non-existent URL, got %+v", notFound)
	}
}

// TestIncrementDownloads verifies that IncrementDownloads correctly
// increments and returns the count.
func TestIncrementDownloads(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SHAREBRIDGE_DATA_DIR", tmpDir)

	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	session := SessionEntry{
		Code:      "testcode",
		ShareURL:  "https://example.com/share/abc",
		Downloads: 0,
		CreatedAt: time.Now(),
	}

	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	// First increment: 0 -> 1
	count, err := store.IncrementDownloads("testcode")
	if err != nil {
		t.Fatalf("First IncrementDownloads failed: %v", err)
	}
	if count != 1 {
		t.Errorf("First increment returned %d, expected 1", count)
	}

	// Second increment: 1 -> 2
	count, err = store.IncrementDownloads("testcode")
	if err != nil {
		t.Fatalf("Second IncrementDownloads failed: %v", err)
	}
	if count != 2 {
		t.Errorf("Second increment returned %d, expected 2", count)
	}

	// Verify GetSession also shows 2
	gotSession := store.GetSession("testcode")
	if gotSession == nil || gotSession.Downloads != 2 {
		t.Errorf("GetSession returned downloads %d, expected 2", gotSession.Downloads)
	}

	// Test non-existent code
	_, err = store.IncrementDownloads("nonexistent")
	if err == nil {
		t.Error("Expected error for non-existent code, got nil")
	}
}

// TestDeleteSession verifies that DeleteSession removes sessions correctly.
func TestDeleteSession(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SHAREBRIDGE_DATA_DIR", tmpDir)

	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	session := SessionEntry{
		Code:      "testcode",
		ShareURL:  "https://example.com/share/abc",
		CreatedAt: time.Now(),
	}

	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	// Verify session exists
	if store.GetSession("testcode") == nil {
		t.Fatalf("Session should exist before deletion")
	}

	// Delete session
	if err := store.DeleteSession("testcode"); err != nil {
		t.Fatalf("DeleteSession failed: %v", err)
	}

	// Verify session no longer exists
	if store.GetSession("testcode") != nil {
		t.Errorf("Session should not exist after deletion")
	}

	// Deleting non-existent session should not error
	if err := store.DeleteSession("nonexistent"); err != nil {
		t.Errorf("DeleteSession for non-existent session should not error: %v", err)
	}
}

// TestListSessions verifies ListSessions with and without expired filtering.
func TestListSessions(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SHAREBRIDGE_DATA_DIR", tmpDir)

	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	now := time.Now()

	// Create sessions: one active, one expired, one with no expiry
	activeSession := SessionEntry{
		Code:      "active",
		ShareURL:  "https://example.com/share/active",
		ExpiresAt: now.Add(1 * time.Hour),
		CreatedAt: now,
	}
	expiredSession := SessionEntry{
		Code:      "expired",
		ShareURL:  "https://example.com/share/expired",
		ExpiresAt: now.Add(-1 * time.Hour), // already expired
		CreatedAt: now,
	}
	noExpirySession := SessionEntry{
		Code:      "noexpiry",
		ShareURL:  "https://example.com/share/noexpiry",
		ExpiresAt: time.Time{}, // zero value = no expiry
		CreatedAt: now,
	}

	if err := store.SaveSession(activeSession); err != nil {
		t.Fatalf("SaveSession active failed: %v", err)
	}
	if err := store.SaveSession(expiredSession); err != nil {
		t.Fatalf("SaveSession expired failed: %v", err)
	}
	if err := store.SaveSession(noExpirySession); err != nil {
		t.Fatalf("SaveSession noexpiry failed: %v", err)
	}

	// List all sessions
	allSessions := store.ListSessions(false)
	if len(allSessions) != 3 {
		t.Errorf("ListSessions(false) returned %d sessions, expected 3", len(allSessions))
	}

	// List only active sessions (filter expired)
	activeSessions := store.ListSessions(true)
	if len(activeSessions) != 2 {
		t.Errorf("ListSessions(true) returned %d sessions, expected 2 (active + noexpiry)", len(activeSessions))
	}

	// Verify the expired one is not in the filtered list
	for _, s := range activeSessions {
		if s.Code == "expired" {
			t.Error("Expired session should not be in filtered list")
		}
	}
}

// TestSaveSession_FilePermissions verifies that sessions.json is created with
// 0600 permissions (readable and writable only by owner).
func TestSaveSession_FilePermissions(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SHAREBRIDGE_DATA_DIR", tmpDir)

	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	// Trigger file creation by saving a session
	session := SessionEntry{
		Code:      "testcode",
		ShareURL:  "https://example.com/share/abc",
		CreatedAt: time.Now(),
	}
	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
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

// TestSaveSession_UpdatesExisting verifies that saving a session with the same code
// updates the existing session.
func TestSaveSession_UpdatesExisting(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SHAREBRIDGE_DATA_DIR", tmpDir)

	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	// Create initial session
	session := SessionEntry{
		Code:      "testcode",
		ShareURL:  "https://example.com/share/abc",
		Downloads: 0,
		CreatedAt: time.Now(),
	}
	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	// Update the session
	updatedSession := SessionEntry{
		Code:      "testcode",
		ShareURL:  "https://example.com/share/abc",
		Downloads: 5,
		CreatedAt: time.Now(),
	}
	if err := store.SaveSession(updatedSession); err != nil {
		t.Fatalf("SaveSession update failed: %v", err)
	}

	// Verify only one session exists
	sessions := store.ListSessions(false)
	if len(sessions) != 1 {
		t.Errorf("Expected 1 session, got %d", len(sessions))
	}

	// Verify the update
	gotSession := store.GetSession("testcode")
	if gotSession == nil {
		t.Fatalf("GetSession returned nil")
	}
	if gotSession.Downloads != 5 {
		t.Errorf("Downloads: got %d, expected 5", gotSession.Downloads)
	}
}

// TestGetSession_MidRunCorruption verifies that GetSession returns nil
// when the file becomes corrupted after the store was created (mid-run).
// This should not panic.
func TestGetSession_MidRunCorruption(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SHAREBRIDGE_DATA_DIR", tmpDir)

	// Create store and save a session
	store, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	session := SessionEntry{
		Code:      "validcode",
		ShareURL:  "https://example.com/share/abc",
		CreatedAt: time.Now(),
	}
	if err := store.SaveSession(session); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	// Verify session was stored
	gotSession := store.GetSession("validcode")
	if gotSession == nil || gotSession.Code != "validcode" {
		t.Fatalf("Expected code 'validcode', got %+v", gotSession)
	}

	// Corrupt the file after store creation - this won't affect the in-memory cache
	sessionsFile := filepath.Join(tmpDir, "sessions.json")
	if err := os.WriteFile(sessionsFile, []byte("{ invalid json"), 0600); err != nil {
		t.Fatalf("Failed to corrupt sessions.json: %v", err)
	}

	// GetSession should still return the cached session (in-memory cache)
	gotSession = store.GetSession("validcode")
	if gotSession == nil || gotSession.Code != "validcode" {
		t.Errorf("Expected cached session with code 'validcode', got %+v", gotSession)
	}
}

func TestAgentID_GeneratedOnFirstLoad(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SHAREBRIDGE_DATA_DIR", tmpDir)

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

func TestSaveSession_PersistsFileID(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SHAREBRIDGE_DATA_DIR", tmpDir)

	st, err := New()
	if err != nil {
		t.Fatalf("New() error: %v", err)
	}

	session := SessionEntry{
		Code:         "test-code",
		ShareURL:     "https://opencloud.example.com/s/abc",
		FileID:       "storage-users-1$abc!def",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
		MaxDownloads: 5,
		CreatedAt:    time.Now(),
	}
	if err := st.SaveSession(session); err != nil {
		t.Fatalf("SaveSession() error: %v", err)
	}

	loaded := st.GetSession("test-code")
	if loaded == nil {
		t.Fatal("GetSession() returned nil")
	}
	if loaded.FileID != "storage-users-1$abc!def" {
		t.Errorf("FileID = %q, want storage-users-1$abc!def", loaded.FileID)
	}
}

func TestGetRelayStaticPrivateKey_GeneratesAndPersistsStableKey(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SHAREBRIDGE_DATA_DIR", tmpDir)

	st, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	first, err := st.GetRelayStaticPrivateKey()
	if err != nil {
		t.Fatalf("GetRelayStaticPrivateKey() first call failed: %v", err)
	}
	second, err := st.GetRelayStaticPrivateKey()
	if err != nil {
		t.Fatalf("GetRelayStaticPrivateKey() second call failed: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Fatal("relay static private key changed within one store instance")
	}

	reloaded, err := New()
	if err != nil {
		t.Fatalf("New() reload failed: %v", err)
	}
	third, err := reloaded.GetRelayStaticPrivateKey()
	if err != nil {
		t.Fatalf("GetRelayStaticPrivateKey() after reload failed: %v", err)
	}
	if !bytes.Equal(first, third) {
		t.Fatal("relay static private key changed after reload")
	}
}
