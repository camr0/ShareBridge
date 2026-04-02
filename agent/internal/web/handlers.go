package web

import (
	"fmt"
	"net/http"
	"runtime"
	"time"
)

// pageData holds data passed to page templates.
type pageData struct {
	Title       string
	ActivePage  string
	Version     string
	Uptime      string
	GoVersion   string
	ConfigPath  string
	Config      configData
}

// configData holds configuration data for templates.
type configData struct {
	SignalingURL      string
	APIKey            string
	AllowedHost       string
	DefaultExpiry     int
	DefaultMaxDownloads int
	DefaultRelayOnly  bool
	UIPort            int
}

// sessionData holds session data for templates.
type sessionData struct {
	Code              string
	ShareURL          string
	Downloads         int
	MaxDownloads      int
	RelayOnly         bool
	ExpiresAtFormatted string
}

// renderPage renders a page template with the layout.
// It clones the base layout template, parses the specific page template,
// and executes with the given data.
func (ws *WebServer) renderPage(w http.ResponseWriter, name string, data pageData) {
	// Clone the base layout template
	tmpl, err := ws.layoutTmpl.Clone()
	if err != nil {
		http.Error(w, fmt.Sprintf("clone layout template: %v", err), http.StatusInternalServerError)
		return
	}

	// Parse the specific page template
	_, err = tmpl.ParseFS(embeddedFS, "templates/"+name)
	if err != nil {
		http.Error(w, fmt.Sprintf("parse page template: %v", err), http.StatusInternalServerError)
		return
	}

	// Set default version if not provided
	if data.Version == "" {
		data.Version = "dev"
	}

	// Auto-populate uptime from daemon if not already set
	if data.Uptime == "" {
		if ws.daemon != nil {
			data.Uptime = formatUptime(time.Now().Add(-ws.daemon.GetUptime()))
		} else {
			data.Uptime = "unknown"
		}
	}

	// Execute the template
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		http.Error(w, fmt.Sprintf("execute template: %v", err), http.StatusInternalServerError)
		return
	}
}

// dashboardHandler renders the dashboard page.
func (ws *WebServer) dashboardHandler(w http.ResponseWriter, r *http.Request) {
	data := pageData{
		Title:      "Dashboard",
		ActivePage: "dashboard",
	}
	ws.renderPage(w, "dashboard.html", data)
}

// settingsHandler renders the settings page with current configuration.
func (ws *WebServer) settingsHandler(w http.ResponseWriter, r *http.Request) {
	var cfg configData
	if ws.daemon != nil {
		c := ws.daemon.GetConfig()
		cfg = configData{
			SignalingURL:      c.SignalingURL,
			APIKey:            c.APIKey,
			AllowedHost:       c.AllowedHost,
			DefaultExpiry:     c.DefaultExpiry,
			DefaultMaxDownloads: c.DefaultMaxDownloads,
			DefaultRelayOnly:  c.DefaultRelayOnly,
			UIPort:            c.UIPort,
		}
	}

	configPath := ""
	if ws.daemon != nil {
		configPath = ws.daemon.GetConfigPath()
	}

	data := pageData{
		Title:      "Settings",
		ActivePage: "settings",
		Config:     cfg,
		GoVersion:  runtime.Version(),
		ConfigPath: configPath,
	}
	ws.renderPage(w, "settings.html", data)
}

// historyHandler renders the history page.
func (ws *WebServer) historyHandler(w http.ResponseWriter, r *http.Request) {
	data := pageData{
		Title:      "History",
		ActivePage: "history",
	}
	ws.renderPage(w, "history.html", data)
}

// formatUptime returns a human-readable uptime string.
func formatUptime(start time.Time) string {
	duration := time.Since(start)

	hours := int(duration.Hours())
	minutes := int(duration.Minutes()) % 60

	if hours >= 24 {
		days := hours / 24
		hours = hours % 24
		if hours > 0 {
			return fmt.Sprintf("%dd %dh", days, hours)
		}
		return fmt.Sprintf("%dd", days)
	}

	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}

	return fmt.Sprintf("%dm", minutes)
}