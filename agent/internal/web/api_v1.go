package web

import (
	"encoding/json"
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

func (ws *WebServer) v1ListSharesHandler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func (ws *WebServer) v1CreateShareHandler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func (ws *WebServer) v1RevokeShareHandler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

func (ws *WebServer) v1SettingsHandler(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}