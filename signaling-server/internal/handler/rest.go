package handler

import (
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/hub"
)

// ServeFile returns a handler that serves a single static file.
func ServeFile(path string) func(*core.RequestEvent) error {
	return func(requestEvent *core.RequestEvent) error {
		http.ServeFile(requestEvent.Response, requestEvent.Request, path)
		return nil
	}
}

type SessionInfoResponse struct {
	Code      string     `json:"code"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	IsActive  bool       `json:"is_active"`
}

// GetSessionInfo returns basic public session info by code.
func GetSessionInfo(app core.App, sessionHub *hub.Hub) func(*core.RequestEvent) error {
	return func(requestEvent *core.RequestEvent) error {
		code := requestEvent.Request.PathValue("code")
		if code == "" {
			return requestEvent.JSON(http.StatusBadRequest, map[string]string{"error": "code required"})
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
			return requestEvent.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to lookup session"})
		}
		if len(records) == 0 {
			return requestEvent.JSON(http.StatusNotFound, map[string]string{"error": "session not found"})
		}

		sessionRecord := records[0]
		var expiresAtPtr *time.Time
		isActive := true

		expiresAt := sessionRecord.GetDateTime("expires_at")
		if !expiresAt.IsZero() {
			expiresAtTime := expiresAt.Time()
			expiresAtPtr = &expiresAtTime
			if expiresAtTime.Before(time.Now()) {
				isActive = false
			}
		}

		if !sessionHub.AgentConnected(sessionRecord.GetString("api_key_id")) {
			isActive = false
		}

		return requestEvent.JSON(http.StatusOK, SessionInfoResponse{
			Code:      sessionRecord.GetString("code"),
			ExpiresAt: expiresAtPtr,
			IsActive:  isActive,
		})
	}
}
