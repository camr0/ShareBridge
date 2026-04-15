package web

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// v1ShareResponse is the JSON representation of a ShareBridge session for the extension API.
type v1ShareResponse struct {
	Code         string    `json:"code"`
	PublicURL    string    `json:"public_url"`
	ShareURL     string    `json:"share_url"`
	FileID       string    `json:"file_id"`
	Downloads    int       `json:"downloads"`
	MaxDownloads int       `json:"max_downloads"`
	RelayOnly    bool      `json:"relay_only"`
	ExpiresAt    time.Time `json:"expires_at"`
	CreatedAt    time.Time `json:"created_at"`
}

type v1CreateShareRequest struct {
	ShareURL     string `json:"share_url"`
	ShareType    string `json:"share_type"`
	Password     string `json:"password"`
	ExpiryHours  int    `json:"expiry_hours"`
	MaxDownloads int    `json:"max_downloads"`
	RelayOnly    bool   `json:"relay_only"`
}

type v1CreateShareResponse struct {
	Code      string    `json:"code"`
	PublicURL string    `json:"public_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

type v1SettingsResponse struct {
	DefaultExpiryHours  int  `json:"default_expiry_hours"`
	DefaultMaxDownloads int  `json:"default_max_downloads"`
	DefaultRelayOnly    bool `json:"default_relay_only"`
	TURNAvailable       bool `json:"turn_available"`
}

type v1ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// writeJSON writes v as JSON with the given HTTP status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// v1ListSharesHandler returns all active sessions as JSON.
// Optional ?file_id= query param filters by oc:fileid.
func (ws *WebServer) v1ListSharesHandler(w http.ResponseWriter, r *http.Request) {
	if ws.daemon == nil {
		writeJSON(w, http.StatusServiceUnavailable, v1ErrorResponse{"Service unavailable", "UNAVAILABLE"})
		return
	}

	fileID := r.URL.Query().Get("file_id")
	cfg := ws.daemon.GetConfig()

	shares := []v1ShareResponse{}
	for _, session := range ws.daemon.ListSessions() {
		if fileID != "" && session.FileID != fileID {
			continue
		}
		shares = append(shares, v1ShareResponse{
			Code:         session.Code,
			PublicURL:    derivePublicURL(cfg.SignalingURL, session.Code),
			ShareURL:     session.ShareURL,
			FileID:       session.FileID,
			Downloads:    session.Downloads,
			MaxDownloads: session.MaxDownloads,
			RelayOnly:    session.RelayOnly,
			ExpiresAt:    session.ExpiresAt,
			CreatedAt:    session.CreatedAt,
		})
	}

	writeJSON(w, http.StatusOK, shares)
}

// v1CreateShareHandler creates a new ShareBridge share from a JSON request body.
func (ws *WebServer) v1CreateShareHandler(w http.ResponseWriter, r *http.Request) {
	if ws.daemon == nil {
		writeJSON(w, http.StatusServiceUnavailable, v1ErrorResponse{"Service unavailable", "UNAVAILABLE"})
		return
	}

	var req v1CreateShareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, v1ErrorResponse{"Invalid request body", "BAD_REQUEST"})
		return
	}
	if req.ShareURL == "" {
		writeJSON(w, http.StatusBadRequest, v1ErrorResponse{"share_url is required", "BAD_REQUEST"})
		return
	}
	if req.ShareType == "" {
		writeJSON(w, http.StatusBadRequest, v1ErrorResponse{"share_type is required", "BAD_REQUEST"})
		return
	}
	if req.ShareType != "opencloud" && req.ShareType != "nextcloud" {
		writeJSON(w, http.StatusBadRequest, v1ErrorResponse{"share_type must be 'opencloud' or 'nextcloud'", "BAD_REQUEST"})
		return
	}

	expiryHours := req.ExpiryHours
	if expiryHours <= 0 {
		expiryHours = ws.daemon.GetConfig().DefaultExpiry
	}

	code, err := ws.daemon.CreateSession(
		r.Context(),
		req.ShareURL,
		req.ShareType,
		req.Password,
		time.Duration(expiryHours)*time.Hour,
		req.MaxDownloads,
		req.RelayOnly,
	)
	if err != nil {
		log.Printf("ERROR v1CreateShare: shareURL=%q shareType=%q err=%v", req.ShareURL, req.ShareType, err)
		writeJSON(w, http.StatusInternalServerError, v1ErrorResponse{err.Error(), "INTERNAL_ERROR"})
		return
	}

	session := ws.daemon.GetSession(code)
	if session == nil {
		writeJSON(w, http.StatusInternalServerError, v1ErrorResponse{"Session not found after creation", "INTERNAL_ERROR"})
		return
	}

	writeJSON(w, http.StatusCreated, v1CreateShareResponse{
		Code:      session.Code,
		PublicURL: derivePublicURL(ws.daemon.GetConfig().SignalingURL, session.Code),
		ExpiresAt: session.ExpiresAt,
	})
}

// v1RevokeShareHandler revokes a share by code, returning 204 on success.
func (ws *WebServer) v1RevokeShareHandler(w http.ResponseWriter, r *http.Request) {
	if ws.daemon == nil {
		writeJSON(w, http.StatusServiceUnavailable, v1ErrorResponse{"Service unavailable", "UNAVAILABLE"})
		return
	}

	code := r.PathValue("code")
	if code == "" {
		writeJSON(w, http.StatusBadRequest, v1ErrorResponse{"Missing share code", "BAD_REQUEST"})
		return
	}

	if err := ws.daemon.RevokeSession(code); err != nil {
		writeJSON(w, http.StatusNotFound, v1ErrorResponse{"Share not found", "NOT_FOUND"})
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// v1SettingsHandler returns the agent's default share configuration for the extension form.
func (ws *WebServer) v1SettingsHandler(w http.ResponseWriter, r *http.Request) {
	if ws.daemon == nil {
		writeJSON(w, http.StatusServiceUnavailable, v1ErrorResponse{"Service unavailable", "UNAVAILABLE"})
		return
	}

	cfg := ws.daemon.GetConfig()
	writeJSON(w, http.StatusOK, v1SettingsResponse{
		DefaultExpiryHours:  cfg.DefaultExpiry,
		DefaultMaxDownloads: cfg.DefaultMaxDownloads,
		DefaultRelayOnly:    cfg.DefaultRelayOnly,
		TURNAvailable:       ws.daemon.HasTURN(),
	})
}