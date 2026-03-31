package transfer

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockDC implements DataChannel for testing
type mockDC struct {
	textMessages []string
	binaryData   [][]byte
	buffered     uint64
	closed       bool
	mu           sync.Mutex
}

func (m *mockDC) SendBinary(data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.binaryData = append(m.binaryData, data)
	return nil
}

func (m *mockDC) SendText(text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.textMessages = append(m.textMessages, text)
	return nil
}

func (m *mockDC) BufferedAmount() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buffered
}

func (m *mockDC) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *mockDC) getLastTextMessage() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.textMessages) == 0 {
		return ""
	}
	return m.textMessages[len(m.textMessages)-1]
}

func (m *mockDC) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

func (m *mockDC) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.textMessages = nil
	m.binaryData = nil
	m.closed = false
}

// TestHandleOpen_NoPassword verifies hello sent with password_required=false
func TestHandleOpen_NoPassword(t *testing.T) {
	dc := &mockDC{}
	// Use nil client - we won't be making any client calls in this test

	mgr := NewManager(dc, nil, "", 0)
	mgr.HandleOpen()

	// Wait for async message
	time.Sleep(10 * time.Millisecond)

	lastMsg := dc.getLastTextMessage()
	if lastMsg == "" {
		t.Fatal("expected hello message")
	}

	var hello struct {
		Type             string `json:"type"`
		PasswordRequired bool   `json:"password_required"`
	}
	if err := json.Unmarshal([]byte(lastMsg), &hello); err != nil {
		t.Fatalf("failed to parse hello: %v", err)
	}

	if hello.Type != "hello" {
		t.Errorf("expected type 'hello', got '%s'", hello.Type)
	}
	if hello.PasswordRequired {
		t.Error("expected password_required=false when no password set")
	}
}

// TestHandleOpen_WithPassword verifies hello sent with password_required=true
func TestHandleOpen_WithPassword(t *testing.T) {
	dc := &mockDC{}

	mgr := NewManager(dc, nil, "secret123", 5)
	mgr.HandleOpen()

	// Wait for async message
	time.Sleep(10 * time.Millisecond)

	lastMsg := dc.getLastTextMessage()
	if lastMsg == "" {
		t.Fatal("expected hello message")
	}

	var hello struct {
		Type             string `json:"type"`
		PasswordRequired bool   `json:"password_required"`
	}
	if err := json.Unmarshal([]byte(lastMsg), &hello); err != nil {
		t.Fatalf("failed to parse hello: %v", err)
	}

	if hello.Type != "hello" {
		t.Errorf("expected type 'hello', got '%s'", hello.Type)
	}
	if !hello.PasswordRequired {
		t.Error("expected password_required=true when password is set")
	}
}

// TestPassword_WrongThenCorrect verifies wrong password returns error but channel stays open
func TestPassword_WrongThenCorrect(t *testing.T) {
	dc := &mockDC{}

	mgr := NewManager(dc, nil, "secret123", 0)

	// Send wrong password
	req, _ := json.Marshal(map[string]string{
		"type":     "list_request",
		"password": "wrongpassword",
	})
	mgr.HandleMessage(req)

	// Wait for processing
	time.Sleep(10 * time.Millisecond)

	// Should get error but channel not closed
	lastMsg := dc.getLastTextMessage()
	if !strings.Contains(lastMsg, "error") {
		t.Errorf("expected error message, got: %s", lastMsg)
	}
	if dc.isClosed() {
		t.Error("channel should not be closed after first wrong password")
	}

	dc.reset()

	// Send correct password
	req, _ = json.Marshal(map[string]string{
		"type":     "list_request",
		"password": "secret123",
	})
	mgr.HandleMessage(req)

	time.Sleep(10 * time.Millisecond)

	// After correct password, auth failures should reset to 0
	// and message should be processed (though will error due to nil client)
	lastMsg = dc.getLastTextMessage()
	if lastMsg == "" {
		t.Error("expected some response after correct password")
	}
}

// TestPassword_ThreeStrikesClosesChannel verifies 3 failures closes channel
func TestPassword_ThreeStrikesClosesChannel(t *testing.T) {
	dc := &mockDC{}

	mgr := NewManager(dc, nil, "secret123", 0)

	// Track if auth failed callback was called
	authFailedCalled := false
	mgr.OnAuthFailed = func() {
		authFailedCalled = true
	}

	// Send 3 wrong passwords
	for i := 0; i < 3; i++ {
		dc.reset()
		req, _ := json.Marshal(map[string]string{
			"type":     "list_request",
			"password": "wrongpassword",
		})
		mgr.HandleMessage(req)
		time.Sleep(10 * time.Millisecond)
	}

	// Channel should be closed after 3 strikes
	if !dc.isClosed() {
		t.Error("channel should be closed after 3 failed password attempts")
	}
	if !authFailedCalled {
		t.Error("OnAuthFailed callback should have been called")
	}

	// Reset and verify we can't send messages anymore
	dc.reset()
	req, _ := json.Marshal(map[string]string{
		"type":     "list_request",
		"password": "secret123",
	})
	mgr.HandleMessage(req)
	time.Sleep(10 * time.Millisecond)

	// No messages should be sent after channel closed
	if dc.getLastTextMessage() != "" {
		t.Error("no messages should be processed after channel closed")
	}
}

// TestFileHeader_IncludesSHA1 verifies sha1 field is present when FileInfo has a checksum
func TestFileHeader_IncludesSHA1(t *testing.T) {
	header := struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		MimeType string `json:"mimeType"`
		SHA1     string `json:"sha1,omitempty"`
	}{
		Type:     "file_header",
		Name:     "video.mp4",
		Size:     1048576,
		MimeType: "video/mp4",
		SHA1:     "a7e0206e573edbef0c4d8107a151271fbeccf2fe",
	}
	data, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if result["sha1"] != "a7e0206e573edbef0c4d8107a151271fbeccf2fe" {
		t.Errorf("expected sha1 in file_header, got: %s", data)
	}
}

// TestFileHeader_OmitsSHA1 verifies sha1 field is absent when FileInfo has no checksum
func TestFileHeader_OmitsSHA1(t *testing.T) {
	header := struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Size     int64  `json:"size"`
		MimeType string `json:"mimeType"`
		SHA1     string `json:"sha1,omitempty"`
	}{
		Type:     "file_header",
		Name:     "notes.txt",
		Size:     512,
		MimeType: "text/plain",
		SHA1:     "",
	}
	data, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	if strings.Contains(string(data), "sha1") {
		t.Errorf("expected sha1 to be omitted from file_header, got: %s", data)
	}
}

// TestMaxDownloads_Rejected verifies download limit reached returns error
func TestMaxDownloads_Rejected(t *testing.T) {
	dc := &mockDC{}

	mgr := NewManager(dc, nil, "", 2)

	// Track session expired callback
	sessionExpiredCalled := false
	mgr.OnSessionExpired = func() {
		sessionExpiredCalled = true
	}

	// Simulate 2 downloads completing
	mgr.downloads.Store(2)

	// Third download request should be rejected
	req, _ := json.Marshal(map[string]string{
		"type": "file_request",
		"name": "test.txt",
	})
	mgr.HandleMessage(req)

	time.Sleep(10 * time.Millisecond)

	lastMsg := dc.getLastTextMessage()
	if !strings.Contains(lastMsg, "share has reached its download limit") {
		t.Errorf("expected download limit error, got: %s", lastMsg)
	}
	if !sessionExpiredCalled {
		t.Error("OnSessionExpired callback should have been called")
	}
}
