package handler

import (
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/hub"
)

// ServeFile returns a handler that serves a single static file.
func ServeFile(path string) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		http.ServeFile(e.Response, e.Request, path)
		return nil
	}
}

type SessionInfoResponse struct {
	Code      string     `json:"code"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	IsActive  bool       `json:"is_active"`
}

// GetSessionInfo returns basic public session info by code.
func GetSessionInfo(app core.App, h *hub.Hub) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		code := e.Request.PathValue("code")
		if code == "" {
			return e.JSON(http.StatusBadRequest, map[string]string{"error": "code required"})
		}

		records, err := app.FindRecordsByFilter(
			"sessions",
			"code = {:code}",
			"",
			1,
			0,
			map[string]any{"code": code},
		)
		if err != nil {
			return e.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to lookup session"})
		}
		if len(records) == 0 {
			return e.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
		}

		record := records[0]
		var expiresAtPtr *time.Time
		isActive := true

		expiresAt := record.GetDateTime("expires_at")
		if !expiresAt.IsZero() {
			t := expiresAt.Time()
			expiresAtPtr = &t
			if t.Before(time.Now()) {
				isActive = false
			}
		}

		if !h.AgentConnected(record.GetString("api_key_id")) {
			isActive = false
		}

		return e.JSON(http.StatusOK, SessionInfoResponse{
			Code:      record.GetString("code"),
			ExpiresAt: expiresAtPtr,
			IsActive:  isActive,
		})
	}
}
