package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"sharebridge/agent/internal/config"
)

func TestCreateShareForm_RequiresShareType(t *testing.T) {
	cfg := &config.Config{AgentAPIKey: "key", DefaultExpiry: 24}
	ws, _ := newV1TestServer(cfg)

	formData := url.Values{}
	formData.Set("share_url", "https://oc.example.com/s/xyz")
	// No share_type provided

	req := httptest.NewRequest("POST", "/api/shares", strings.NewReader(formData.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	ws.createShareHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "share_type is required") {
		t.Errorf("body = %q, want 'share_type is required' message", rec.Body.String())
	}
}

func TestCreateShareForm_RejectsInvalidShareType(t *testing.T) {
	cfg := &config.Config{AgentAPIKey: "key", DefaultExpiry: 24}
	ws, _ := newV1TestServer(cfg)

	formData := url.Values{}
	formData.Set("share_url", "https://oc.example.com/s/xyz")
	formData.Set("share_type", "invalid")

	req := httptest.NewRequest("POST", "/api/shares", strings.NewReader(formData.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	ws.createShareHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "share_type must be") {
		t.Errorf("body = %q, want 'share_type must be' message", rec.Body.String())
	}
}

func TestCreateShareForm_AcceptsOpencloudShareType(t *testing.T) {
	cfg := &config.Config{
		AgentAPIKey:  "key",
		SignalingURL: "wss://share.example.com",
		DefaultExpiry: 24,
	}
	ws, mock := newV1TestServer(cfg)

	formData := url.Values{}
	formData.Set("share_url", "https://oc.example.com/s/xyz")
	formData.Set("share_type", "opencloud")

	req := httptest.NewRequest("POST", "/api/shares", strings.NewReader(formData.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	ws.createShareHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// Verify session was created
	session := mock.GetSession("code-1")
	if session == nil {
		t.Fatal("session not created")
	}
}

func TestCreateShareForm_AcceptsNextcloudShareType(t *testing.T) {
	cfg := &config.Config{
		AgentAPIKey:  "key",
		SignalingURL: "wss://share.example.com",
		DefaultExpiry: 24,
	}
	ws, mock := newV1TestServer(cfg)

	formData := url.Values{}
	formData.Set("share_url", "https://nc.example.com/s/xyz")
	formData.Set("share_type", "nextcloud")

	req := httptest.NewRequest("POST", "/api/shares", strings.NewReader(formData.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	ws.createShareHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// Verify session was created
	session := mock.GetSession("code-1")
	if session == nil {
		t.Fatal("session not created")
	}
}