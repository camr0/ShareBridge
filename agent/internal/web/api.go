package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"sharebridge/agent/internal/config"
)

// listSharesHandler renders share cards for all active sessions.
func (ws *WebServer) listSharesHandler(w http.ResponseWriter, r *http.Request) {
	if ws.daemon == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<p><em>No daemon available</em></p>"))
		return
	}

	sessions := ws.daemon.ListSessions()

	if len(sessions) == 0 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<p><em>No active shares</em></p>"))
		return
	}

	// Get signaling URL for deriving public share URLs
	signalingURL := ""
	if ws.daemon != nil {
		cfg := ws.daemon.GetConfig()
		signalingURL = cfg.SignalingURL
	}

	// Parse the share-card template
	tmpl, err := template.ParseFS(embeddedFS, "templates/share-card.html")
	if err != nil {
		http.Error(w, fmt.Sprintf("parse share-card template: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	for _, session := range sessions {
		data := sessionData{
			Code:               session.Code,
			ShareURL:           session.ShareURL,
			PublicURL:          derivePublicURL(signalingURL, session.Code, session.ShareType),
			Downloads:          session.Downloads,
			MaxDownloads:       session.MaxDownloads,
			RelayOnly:          session.RelayOnly,
			ExpiresAtFormatted: formatExpiry(session.ExpiresAt),
		}

		if err := tmpl.ExecuteTemplate(w, "share-card", data); err != nil {
			http.Error(w, fmt.Sprintf("render share card: %v", err), http.StatusInternalServerError)
			return
		}
	}
}

// createShareHandler parses form data and creates a new share session.
// Returns the share card HTML for HTMX to insert.
func (ws *WebServer) createShareHandler(w http.ResponseWriter, r *http.Request) {
	if ws.daemon == nil {
		http.Error(w, "Daemon not available", http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, fmt.Sprintf("parse form: %v", err), http.StatusBadRequest)
		return
	}

	shareURL := r.FormValue("share_url")
	password := r.FormValue("password")
	shareType := r.FormValue("share_type")

	if shareType == "" {
		http.Error(w, "share_type is required", http.StatusBadRequest)
		return
	}
	if err := validateShareType(shareType, ws.daemon.GetConfig()); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Parse expiry hours
	expiryHours := 24
	if val := r.FormValue("expiry_hours"); val != "" {
		if parsed, err := strconv.Atoi(val); err == nil && parsed > 0 {
			expiryHours = parsed
		}
	}

	// Parse max downloads
	maxDownloads := 10
	if val := r.FormValue("max_downloads"); val != "" {
		if parsed, err := strconv.Atoi(val); err == nil && parsed >= 0 {
			maxDownloads = parsed
		}
	}

	// Parse relay only
	relayOnly := r.FormValue("relay_only") == "true"

	// Create session
	code, err := ws.daemon.CreateSession(
		r.Context(),
		shareURL,
		shareType,
		password,
		time.Duration(expiryHours)*time.Hour,
		maxDownloads,
		relayOnly,
	)
	if err != nil {
		if isValidationError(err) {
			http.Error(w, fmt.Sprintf("create session: %v", err), http.StatusBadRequest)
			return
		}
		http.Error(w, fmt.Sprintf("create session: %v", err), http.StatusInternalServerError)
		return
	}

	// Get the created session
	session := ws.daemon.GetSession(code)
	if session == nil {
		http.Error(w, "Session not found after creation", http.StatusInternalServerError)
		return
	}

	// Get signaling URL for deriving public share URL
	signalingURL := ""
	if ws.daemon != nil {
		cfg := ws.daemon.GetConfig()
		signalingURL = cfg.SignalingURL
	}

	// Parse share-card template
	tmpl, err := template.ParseFS(embeddedFS, "templates/share-card.html")
	if err != nil {
		http.Error(w, fmt.Sprintf("parse share-card template: %v", err), http.StatusInternalServerError)
		return
	}

	// Render the card
	data := sessionData{
		Code:               session.Code,
		ShareURL:           session.ShareURL,
		PublicURL:          derivePublicURL(signalingURL, session.Code, session.ShareType),
		Downloads:          session.Downloads,
		MaxDownloads:       session.MaxDownloads,
		RelayOnly:          session.RelayOnly,
		ExpiresAtFormatted: formatExpiry(session.ExpiresAt),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "share-card", data); err != nil {
		http.Error(w, fmt.Sprintf("render share card: %v", err), http.StatusInternalServerError)
		return
	}
}

func validateShareType(shareType string, cfg *config.Config) error {
	switch shareType {
	case "opencloud", "nextcloud":
		return nil
	case "immich":
		if cfg == nil || cfg.ImmichURL == "" || cfg.ImmichAllowedHost == "" || cfg.ImmichAPIKey == "" {
			return fmt.Errorf("immich is not configured")
		}
		return nil
	default:
		return fmt.Errorf("share_type must be 'opencloud', 'nextcloud', or 'immich'")
	}
}

func isValidationError(err error) bool {
	var validation interface {
		IsValidationError() bool
	}
	return errors.As(err, &validation) && validation.IsValidationError()
}

// revokeShareHandler handles DELETE requests to revoke a share session.
func (ws *WebServer) revokeShareHandler(w http.ResponseWriter, r *http.Request) {
	if ws.daemon == nil {
		http.Error(w, "Daemon not available", http.StatusInternalServerError)
		return
	}

	code := r.PathValue("code")
	if code == "" {
		http.Error(w, "Missing share code", http.StatusBadRequest)
		return
	}

	if err := ws.daemon.RevokeSession(code); err != nil {
		http.Error(w, fmt.Sprintf("revoke session: %v", err), http.StatusInternalServerError)
		return
	}

	// Return empty response with 200 status for HTMX swap
	w.WriteHeader(http.StatusOK)
}

// shareFormHandler renders the share form modal.
func (ws *WebServer) shareFormHandler(w http.ResponseWriter, r *http.Request) {
	// Parse share-form template
	tmpl, err := template.ParseFS(embeddedFS, "templates/share-form.html")
	if err != nil {
		http.Error(w, fmt.Sprintf("parse share-form template: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, "share-form", nil); err != nil {
		http.Error(w, fmt.Sprintf("render share form: %v", err), http.StatusInternalServerError)
		return
	}
}

// statusHandler returns the connection status as HTML.
func (ws *WebServer) statusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if ws.daemon == nil {
		w.Write([]byte(`<span class="status-chip status-chip-offline">● Offline</span>`))
		return
	}

	if ws.daemon.IsConnected() {
		w.Write([]byte(`<span class="status-chip status-chip-online">● Connected</span>`))
	} else {
		w.Write([]byte(`<span class="status-chip status-chip-unknown">● Connecting...</span>`))
	}
}

// turnStatusHandler returns TURN server availability as JSON.
func (ws *WebServer) turnStatusHandler(w http.ResponseWriter, r *http.Request) {
	hasTurn := false
	if ws.daemon != nil {
		hasTurn = ws.daemon.HasTURN()
	}

	response := map[string]bool{
		"available": hasTurn,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		http.Error(w, fmt.Sprintf("encode JSON: %v", err), http.StatusInternalServerError)
		return
	}
}

// saveSettingsHandler handles PUT requests to save settings.
func (ws *WebServer) saveSettingsHandler(w http.ResponseWriter, r *http.Request) {
	if ws.daemon == nil {
		http.Error(w, "Daemon not available", http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, fmt.Sprintf("parse form: %v", err), http.StatusBadRequest)
		return
	}

	// Get current config
	currentCfg := ws.daemon.GetConfig()

	// Ignore env-managed fields even if a crafted request sends them.
	if os.Getenv("SIGNALING_SERVER") == "" {
		currentCfg.SignalingURL = r.FormValue("signaling_url")
	}
	if os.Getenv("SHAREBRIDGE_API_KEY") == "" {
		currentCfg.APIKey = r.FormValue("api_key")
	}
	if os.Getenv("ALLOWED_SHAREBRIDGE_HOST") == "" {
		currentCfg.AllowedHost = r.FormValue("allowed_host")
	}
	if os.Getenv("NC_ALLOWED_SHAREBRIDGE_HOST") == "" {
		currentCfg.NCAllowedHost = r.FormValue("nc_allowed_host")
	}

	// Parse default expiry hours
	if val := r.FormValue("default_expiry_hours"); val != "" {
		if parsed, err := strconv.Atoi(val); err == nil && parsed > 0 {
			currentCfg.DefaultExpiry = parsed
		}
	}

	// Parse default max downloads
	if val := r.FormValue("default_max_downloads"); val != "" {
		if parsed, err := strconv.Atoi(val); err == nil && parsed >= 0 {
			currentCfg.DefaultMaxDownloads = parsed
		}
	}

	// Parse default relay only
	currentCfg.DefaultRelayOnly = r.FormValue("default_relay_only") == "true"

	// Save config using the daemon
	if err := ws.daemon.SaveConfig(currentCfg); err != nil {
		http.Error(w, fmt.Sprintf("save config: %v", err), http.StatusInternalServerError)
		return
	}

	// Return success message as HTML
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<div class="success"><p>Settings saved successfully!</p></div>`))
}

// formatExpiry returns a human-readable expiry string.
func formatExpiry(expiresAt time.Time) string {
	if expiresAt.IsZero() {
		return "Never"
	}

	now := time.Now()
	if expiresAt.Before(now) {
		return "Expired"
	}

	duration := expiresAt.Sub(now)
	hours := int(duration.Hours())

	if hours >= 24 {
		days := hours / 24
		if days == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", days)
	}

	if hours >= 1 {
		if hours == 1 {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", hours)
	}

	minutes := int(duration.Minutes())
	if minutes <= 1 {
		return "<1 min"
	}
	return fmt.Sprintf("%d min", minutes)
}

// quotaInlineHandler returns inline quota HTML for the share form.
// Simpler than the dashboard widget - no outer wrapper.
func (ws *WebServer) quotaInlineHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if ws.daemon == nil {
		w.Write([]byte(`<div class="quota-unavailable">Agent not ready</div>`))
		return
	}

	cfg := ws.daemon.GetConfig()
	if cfg.SignalingURL == "" || cfg.APIKey == "" {
		w.Write([]byte(`<div class="quota-unavailable">No signaling server or API key configured</div>`))
		return
	}

	quota, err := ws.fetchQuotaFromSignalingServer(cfg)
	if err != nil {
		w.Write([]byte(`<div class="quota-unavailable">Unable to load quota</div>`))
		return
	}

	remainingPercent := 100.0 - quota.PercentageUsed
	barClass := ""
	if remainingPercent <= 10 {
		barClass = "quota-bar-critical"
	} else if remainingPercent <= 30 {
		barClass = "quota-bar-warning"
	}

	periodEndStr := quota.PeriodEnd.Format("Jan 7")
	if quota.PeriodEnd.Year() != time.Now().Year() {
		periodEndStr = quota.PeriodEnd.Format("Jan 7, 2006")
	}

	html := fmt.Sprintf(`
<div class="quota-info">
    <div class="quota-bar-container">
        <div class="quota-bar-used %s" style="width: %.1f%%;"></div>
    </div>
    <div class="quota-text">
        <span class="quota-remaining">%.2f GB remaining / %.0f GB quota</span>
        <span>%.0f%%</span>
    </div>
    <div class="quota-period">Resets %s</div>
</div>
`, barClass, remainingPercent, quota.RemainingGB, quota.LimitGB, remainingPercent, periodEndStr)

	w.Write([]byte(html))
}

// fetchQuotaFromSignalingServer is a helper that fetches quota from the signaling server.
func (ws *WebServer) fetchQuotaFromSignalingServer(cfg *config.Config) (*quotaResponse, error) {
	quotaURL := cfg.SignalingURL
	if strings.HasPrefix(quotaURL, "wss://") {
		quotaURL = "https://" + strings.TrimPrefix(quotaURL, "wss://")
	} else if strings.HasPrefix(quotaURL, "ws://") {
		quotaURL = "http://" + strings.TrimPrefix(quotaURL, "ws://")
	}
	quotaURL = strings.TrimSuffix(quotaURL, "/") + "/api/account/quota?api_key=" + cfg.APIKey

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(quotaURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var quota quotaResponse
	if err := json.NewDecoder(resp.Body).Decode(&quota); err != nil {
		return nil, err
	}

	return &quota, nil
}

type quotaResponse struct {
	LimitGB        float64   `json:"limit_gb"`
	UsedGB         float64   `json:"used_gb"`
	RemainingGB    float64   `json:"remaining_gb"`
	PeriodStart    time.Time `json:"period_start"`
	PeriodEnd      time.Time `json:"period_end"`
	PercentageUsed float64   `json:"percentage_used"`
}

// quotaWidgetHandler returns an HTML quota widget for the dashboard.
// Uses HTMX polling to refresh every 30s.
func (ws *WebServer) quotaWidgetHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if ws.daemon == nil {
		w.Write([]byte(`<div class="quota-widget"><div class="quota-unavailable">Agent not ready</div></div>`))
		return
	}

	cfg := ws.daemon.GetConfig()
	if cfg.SignalingURL == "" || cfg.APIKey == "" {
		w.Write([]byte(`<div class="quota-widget"><div class="quota-unavailable">No signaling server or API key configured</div></div>`))
		return
	}

	quota, err := ws.fetchQuotaFromSignalingServer(cfg)
	if err != nil {
		w.Write([]byte(`<div class="quota-widget"><div class="quota-unavailable">Unable to load quota</div></div>`))
		return
	}

	remainingPercent := 100.0 - quota.PercentageUsed
	barClass := ""
	if remainingPercent <= 10 {
		barClass = "quota-bar-critical"
	} else if remainingPercent <= 30 {
		barClass = "quota-bar-warning"
	}

	periodEndStr := quota.PeriodEnd.Format("Jan 7")
	if quota.PeriodEnd.Year() != time.Now().Year() {
		periodEndStr = quota.PeriodEnd.Format("Jan 7, 2006")
	}

	// Render HTML widget
	html := fmt.Sprintf(`
<div class="quota-widget">
    <div class="quota-widget-header">
        <span class="quota-widget-title">Relay Quota <small>(TURN bandwidth)</small></span>
    </div>
    <div class="quota-bar-container">
        <div class="quota-bar-used %s" style="width: %.1f%%;"></div>
    </div>
    <div class="quota-text">
        <span class="quota-remaining">%.2f GB remaining / %.0f GB quota</span>
        <span>%.0f%%</span>
    </div>
    <div class="quota-period">Resets %s</div>
</div>
`, barClass, remainingPercent, quota.RemainingGB, quota.LimitGB, remainingPercent, periodEndStr)

	w.Write([]byte(html))
}

// relayQuotaHandler fetches quota info from the signaling server.
// Returns 204 if daemon not ready, no API key, or no signaling URL.
// Returns quota JSON on success, or 204 on error.
func (ws *WebServer) relayQuotaHandler(w http.ResponseWriter, r *http.Request) {
	// Check daemon is ready
	if ws.daemon == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	cfg := ws.daemon.GetConfig()

	// Need signaling URL and API key to fetch quota
	if cfg.SignalingURL == "" || cfg.APIKey == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Convert ws:// to http://, wss:// to https://
	quotaURL := cfg.SignalingURL
	if strings.HasPrefix(quotaURL, "wss://") {
		quotaURL = "https://" + strings.TrimPrefix(quotaURL, "wss://")
	} else if strings.HasPrefix(quotaURL, "ws://") {
		quotaURL = "http://" + strings.TrimPrefix(quotaURL, "ws://")
	}
	quotaURL = strings.TrimSuffix(quotaURL, "/") + "/api/account/quota?api_key=" + cfg.APIKey

	// Create request to signaling server
	req, err := http.NewRequestWithContext(r.Context(), "GET", quotaURL, nil)

	// Make the request
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Parse and forward the quota response
	var quotaResp struct {
		LimitGB        float64   `json:"limit_gb"`
		UsedGB         float64   `json:"used_gb"`
		RemainingGB    float64   `json:"remaining_gb"`
		PeriodStart    time.Time `json:"period_start"`
		PeriodEnd      time.Time `json:"period_end"`
		PercentageUsed float64   `json:"percentage_used"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&quotaResp); err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(quotaResp); err != nil {
		w.WriteHeader(http.StatusNoContent)
	}
}
