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
	"strings"
	"time"

	"sharebridge/agent/internal/daemon"
)

//go:embed static/* templates/*
var embeddedFS embed.FS

// WebServer provides the HTTP server for the agent admin UI.
// It serves embedded static assets and templates, with CSRF protection
// and optional basic auth.
type WebServer struct {
	daemon     *daemon.Daemon
	port       int
	password   string
	server     *http.Server
	layoutTmpl *template.Template // Base layout, cloned per request
	staticFS   http.FileSystem
}

// NewWebServer creates a new web server instance.
// It parses the layout template and creates a static file sub-filesystem.
// The daemon reference may be nil initially and set later via SetDaemon.
func NewWebServer(d *daemon.Daemon, port int, password string) (*WebServer, error) {
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
	// Create HTTP server with auth middleware if password is set
	mux := http.NewServeMux()

	// Register routes
	ws.registerRoutes(mux)

	// Apply auth middleware if password configured
	var handler http.Handler = mux
	if ws.password != "" {
		handler = ws.authMiddleware(handler)
	}

	ws.server = &http.Server{
		Addr:         fmt.Sprintf("127.0.0.1:%d", ws.port),
		Handler:      handler,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Start server in background
	errChan := make(chan error, 1)
	go func() {
		log.Printf("web server starting on http://127.0.0.1:%d", ws.port)
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

	// Relay quota endpoint
	mux.HandleFunc("GET /api/relay-quota", ws.relayQuotaHandler)

	// Quota widget for dashboard (returns HTML)
	mux.HandleFunc("GET /api/quota-widget", ws.quotaWidgetHandler)

	// Inline quota for share form (returns HTML, same style)
	mux.HandleFunc("GET /api/quota-inline", ws.quotaInlineHandler)
}

// csrfMiddleware verifies a browser-set request header on non-GET requests.
// Accepts HX-Request (sent by HTMX) or X-Requested-With (sent by XHR/jQuery/CLI client).
// Either header proves the request came from JS, not a cross-origin form POST.
func (ws *WebServer) csrfMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			htmxRequest := r.Header.Get("HX-Request") == "true"
			xhrRequest := r.Header.Get("X-Requested-With") == "XMLHttpRequest"
			if !htmxRequest && !xhrRequest {
				http.Error(w, "Forbidden - CSRF check failed", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

// authMiddleware provides optional basic auth protection.
// If a password is configured, all requests require authentication.
func (ws *WebServer) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for static files (they contain no sensitive data)
		if strings.HasPrefix(r.URL.Path, "/static/") {
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
