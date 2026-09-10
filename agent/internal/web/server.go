package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"sharebridge/agent/internal/config"
	"sharebridge/agent/internal/daemon"
)

//go:embed static/* templates/*
var embeddedFS embed.FS

// daemonProvider is the set of Daemon methods used by web handlers.
// Using an interface allows web package tests to inject a mock.
type daemonProvider interface {
	ListSessions() []*daemon.Session
	GetSession(code string) *daemon.Session
	GetConfig() *config.Config
	CreateSession(ctx context.Context, shareURL, shareType, password string, expiry time.Duration, maxDownloads int, relayOnly bool) (string, error)
	RevokeSession(code string) error
	HasTURN() bool
	IsConnected() bool
	GetUptime() time.Duration
	GetConfigPath() string
	SaveConfig(cfg *config.Config) error
}

// WebServer provides the HTTP server for the agent admin UI.
// It serves embedded static assets and templates, with CSRF protection
// and optional basic auth.
type WebServer struct {
	daemon     daemonProvider
	addr       string
	port       int
	password   string
	server     *http.Server
	layoutTmpl *template.Template // Base layout, cloned per request
	staticFS   http.FileSystem
}

// NewWebServer creates a new web server instance.
// It parses the layout template and creates a static file sub-filesystem.
// The daemon reference may be nil initially and set later via SetDaemon.
// addr is the bind address (e.g. "127.0.0.1" or "0.0.0.0").
func NewWebServer(d daemonProvider, addr string, port int, password string) (*WebServer, error) {
	// Secure by default: an empty address means loopback, never all
	// interfaces. A non-loopback bind without a credential is refused here so
	// no admin surface can be constructed, let alone started.
	addr = config.NormalizeUIAddr(addr)
	if err := config.ValidateAdminBind(addr, password); err != nil {
		return nil, err
	}

	// Parse the layout template
	layoutTmpl, err := template.ParseFS(embeddedFS, "templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("parse layout template: %w", err)
	}

	// Create sub-filesystem for static files
	staticSubFS, err := fs.Sub(embeddedFS, "static")
	if err != nil {
		return nil, fmt.Errorf("create static sub-filesystem: %w", err)
	}

	return &WebServer{
		daemon:     d,
		addr:       addr,
		port:       port,
		password:   password,
		layoutTmpl: layoutTmpl,
		staticFS:   http.FS(staticSubFS),
	}, nil
}

// SetDaemon sets the daemon reference. This is used to resolve circular
// import issues - the daemon creates the web server, then calls SetDaemon
// to complete the reference cycle.
func (ws *WebServer) SetDaemon(d *daemon.Daemon) {
	ws.daemon = d
}

// Start registers routes and starts the HTTP server.
// The server runs until the context is cancelled or Stop is called.
func (ws *WebServer) Start(ctx context.Context) error {
	// Fail closed before any listener exists: a non-loopback bind without a
	// configured credential must never start. This also covers a WebServer
	// assembled directly (bypassing NewWebServer) and normalizes an empty
	// address to loopback rather than the all-interfaces ":port" form.
	addr := config.NormalizeUIAddr(ws.addr)
	if err := config.ValidateAdminBind(addr, ws.password); err != nil {
		return err
	}
	ws.addr = addr

	handler := ws.handler()

	listenAddr := fmt.Sprintf("%s:%d", addr, ws.port)
	ws.server = &http.Server{
		Addr:         listenAddr,
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Start server in background
	errChan := make(chan error, 1)
	go func() {
		log.Printf("web server starting on http://%s", listenAddr)
		if err := ws.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errChan <- err
		}
	}()

	// Wait for either server error or context cancellation
	select {
	case err := <-errChan:
		return fmt.Errorf("web server error: %w", err)
	case <-ctx.Done():
		log.Println("web server shutting down due to context cancellation")
		return ws.Stop()
	}
}

// Stop gracefully shuts down the HTTP server.
func (ws *WebServer) Stop() error {
	if ws.server == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := ws.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("web server shutdown: %w", err)
	}

	log.Println("web server stopped")
	return nil
}

// handler assembles the production request chain: the full route table wrapped
// in the admin-auth middleware. Start uses it, and tests drive it so they
// exercise the real wiring rather than a mock chain.
func (ws *WebServer) handler() http.Handler {
	mux := http.NewServeMux()
	ws.registerRoutes(mux)
	return ws.adminAuthMiddleware(mux)
}

// registerRoutes sets up all HTTP routes for the web server.
func (ws *WebServer) registerRoutes(mux *http.ServeMux) {
	// Static files - no CSRF needed
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(ws.staticFS)))

	// Page routes - GET only, no CSRF needed
	mux.HandleFunc("GET /", ws.dashboardHandler)
	mux.HandleFunc("GET /settings", ws.settingsHandler)
	mux.HandleFunc("GET /history", ws.historyHandler)

	// API routes - require CSRF for non-GET methods
	// Shares
	mux.HandleFunc("GET /api/shares", ws.csrfMiddleware(ws.listSharesHandler))
	mux.HandleFunc("POST /api/shares", ws.csrfMiddleware(ws.createShareHandler))
	mux.HandleFunc("DELETE /api/shares/{code}", ws.csrfMiddleware(ws.revokeShareHandler))

	// Share form modal
	mux.HandleFunc("GET /api/share-form", ws.shareFormHandler)

	// Status endpoints
	mux.HandleFunc("GET /api/status", ws.statusHandler)
	mux.HandleFunc("GET /api/turn-status", ws.turnStatusHandler)

	// Settings
	mux.HandleFunc("PUT /api/settings", ws.csrfMiddleware(ws.saveSettingsHandler))
	// Secret reveal: the settings page renders masks only; the raw keys are
	// fetched on explicit user action through this admin-authenticated route.
	mux.HandleFunc("GET /api/settings/secrets", ws.settingsSecretsHandler)

	// §13.4 reversible lockdown: authenticated local admin API only. These
	// endpoints change transport availability, so they fail CLOSED when no
	// admin credential is configured (the default UI deployment) and require
	// the configured credential when one is; the state-changing POSTs also
	// carry the CSRF header like every other mutation.
	mux.HandleFunc("POST /api/lockdown", ws.requireAdminCredential(ws.csrfMiddleware(ws.lockdownHandler)))
	mux.HandleFunc("POST /api/unlock", ws.requireAdminCredential(ws.csrfMiddleware(ws.unlockHandler)))
	mux.HandleFunc("GET /api/lockdown-status", ws.lockdownStatusHandler)

	// Relay quota endpoint
	mux.HandleFunc("GET /api/relay-quota", ws.relayQuotaHandler)

	// Quota widget for dashboard (returns HTML)
	mux.HandleFunc("GET /api/quota-widget", ws.quotaWidgetHandler)

	// Inline quota for share form (returns HTML, same style)
	mux.HandleFunc("GET /api/quota-inline", ws.quotaInlineHandler)

	// v1 JSON API — CORS headers + API key auth on every request
	v1 := func(h http.HandlerFunc) http.HandlerFunc {
		return ws.corsMiddleware(ws.apiKeyMiddleware(h))
	}
	mux.HandleFunc("GET /api/v1/shares", v1(ws.v1ListSharesHandler))
	mux.HandleFunc("POST /api/v1/shares", v1(ws.v1CreateShareHandler))
	mux.HandleFunc("DELETE /api/v1/shares/{code}", v1(ws.v1RevokeShareHandler))
	mux.HandleFunc("GET /api/v1/settings", v1(ws.v1SettingsHandler))
	// OPTIONS preflight — CORS only, no auth (browsers don't send auth on preflight)
	mux.HandleFunc("OPTIONS /api/v1/{path...}", ws.corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
}

// csrfMiddleware protects state-changing admin requests. It requires BOTH:
//
//  1. same-origin enforcement: an Origin header, when present, must name the
//     same host as the request (a web page cannot forge Origin), and
//  2. a browser-set JS header (HX-Request or X-Requested-With), which also
//     covers non-browser clients that send no Origin.
//
// The JS header alone is not authentication and is treated as defense in
// depth: a cross-origin request is rejected on the Origin/Host check no matter
// which headers it forges.
func (ws *WebServer) csrfMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			if !sameOriginOrCSRFHeader(r) {
				http.Error(w, "Forbidden - cross-origin or CSRF check failed", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

// sameOriginOrCSRFHeader reports whether a state-changing request is
// same-origin (when it declares an Origin header) and carries a JS-set request
// header. Host comparison is scheme-agnostic so the agent keeps working behind
// a TLS-terminating reverse proxy, while any different host is rejected.
func sameOriginOrCSRFHeader(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
			return false
		}
	}
	htmxRequest := r.Header.Get("HX-Request") == "true"
	xhrRequest := r.Header.Get("X-Requested-With") == "XMLHttpRequest"
	return htmxRequest || xhrRequest
}

// adminAuthMiddleware enforces authentication on every dynamic admin surface
// (pages, HTML fragments, JSON APIs and mutations); static assets are exempt
// because they carry no data. It fails closed:
//
//   - a configured password is always required (constant-time comparison);
//   - with no password, loopback callers are trusted (local UX), while a
//     non-loopback bind without a credential is refused at
//     construction/startup and refused again here for defense in depth.
func (ws *WebServer) adminAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for static files (they contain no sensitive data)
		if strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}

		if ws.password == "" {
			if !config.IsLoopbackAddr(ws.addr) {
				http.Error(w, "Forbidden - admin credential not configured", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		_, password, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(password), []byte(ws.password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="ShareBridge Agent"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		// Password validated - username ignored
		next.ServeHTTP(w, r)
	})
}

// requireAdminCredential is the stricter credential gate used by the §13.4
// lockdown endpoints, which change transport availability. Unlike
// adminAuthMiddleware it fails CLOSED even on loopback when no password is
// configured, so a default deployment can never be stopped by an
// unauthenticated caller. With a password configured, the same constant-time
// basic-auth check applies.
func (ws *WebServer) requireAdminCredential(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ws.password == "" {
			http.Error(w, "Forbidden - admin credential not configured", http.StatusForbidden)
			return
		}
		_, password, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(password), []byte(ws.password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="ShareBridge Agent"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// corsMiddleware sets CORS headers for /api/v1/ endpoints.
// The Origin request header is matched against AllowedHost (OpenCloud) and NCAllowedHost (Nextcloud);
// the matching origin is reflected back. Returns 503 if AllowedHost is not configured.
// Handles OPTIONS preflight by returning 204 without calling next.
func (ws *WebServer) corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ws.daemon == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"Service unavailable","code":"UNAVAILABLE"}`))
			return
		}
		cfg := ws.daemon.GetConfig()
		if cfg.AllowedHost == "" {
			// Without AllowedHost we cannot set a safe CORS origin.
			// Reject rather than use a wildcard.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"AllowedHost not configured","code":"MISCONFIGURED"}`))
			return
		}
		// Match incoming Origin against each configured host.
		origin := r.Header.Get("Origin")
		allowedOrigin := "https://" + cfg.AllowedHost // default to first configured host
		for _, host := range []string{cfg.AllowedHost, cfg.NCAllowedHost} {
			if host != "" && origin == "https://"+host {
				allowedOrigin = origin
				break
			}
		}
		w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-API-Key")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// apiKeyMiddleware checks X-API-Key against AgentAPIKey in config.
// Returns 401 JSON on mismatch; 503 JSON if daemon is not ready.
func (ws *WebServer) apiKeyMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ws.daemon == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"Service unavailable","code":"UNAVAILABLE"}`))
			return
		}
		cfg := ws.daemon.GetConfig()
		provided := r.Header.Get("X-API-Key")
		if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.AgentAPIKey)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"Invalid API key","code":"UNAUTHORIZED"}`))
			return
		}
		next(w, r)
	}
}
