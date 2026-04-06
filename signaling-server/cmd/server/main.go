package main

import (
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/handler"
	"sharebridge/server/internal/hub"
	_ "sharebridge/server/migrations"
)

func main() {
	cfg := config.Load()
	_ = hub.New() // h will be used in subsequent tasks for WebSocket handlers

	app := pocketbase.New()

	// IMPORTANT: wire PocketBase to cfg.DBPath and cfg.Port explicitly.
	// The PocketBase CLI uses --dir for data directory

	// Configure SMTP if provided (must be done before Bootstrap so email flows work).
	if cfg.HasSMTP() {
		app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
			settings := app.Settings()
			settings.SMTP.Enabled = true
			settings.SMTP.Host = cfg.SMTPHost
			smtpPort, _ := strconv.Atoi(cfg.SMTPPort)
			settings.SMTP.Port = smtpPort
			settings.SMTP.Username = cfg.SMTPUser
			settings.SMTP.Password = cfg.SMTPPassword
			return e.Next()
		})
	}

	app.OnServe().BindFunc(func(se *core.ServeEvent) error {
		router := se.Router

		// Do not expose /_/ on the public reverse-proxied site.
		// Enforce the primary deny rule in nginx/Caddy/Traefik; a same-host reverse
		// proxy makes RemoteAddr appear local, so an app-only localhost check is not enough.

		// WebSocket endpoints - placeholder for now (full implementation in Task 6)
		router.GET("/ws/agent", placeholderWSHandler("agent"))
		router.GET("/ws/client", placeholderWSHandler("client"))

		// Session info REST endpoint - placeholder
		router.GET("/sessions/{code}", placeholderSessionHandler())

		// Direct link route - serves index.html; JS reads code from window.location
		router.GET("/s/{code}", handler.ServeFile("./web/index.html"))

		// Static web files
		router.GET("/", handler.ServeFile("./web/index.html"))
		router.GET("/app.js", handler.ServeFile("./web/app.js"))

		// User-facing pages (placeholders - full implementation in Task 10)
		router.GET("/register", handler.ServeFile("./web/register.html"))
		router.GET("/login", handler.ServeFile("./web/login.html"))
		router.GET("/account", handler.ServeFile("./web/account.html"))

		// User-scoped API key management (requires JWT auth) - placeholder handlers
		// Uses Bind middleware for auth (apis.RequireAuth returns *hook.Handler)
		apiKeys := router.Group("/api/keys")
		apiKeys.Bind(apis.RequireAuth())
		apiKeys.POST("/", placeholderAPIHandler("create key"))
		apiKeys.GET("/", placeholderAPIHandler("list keys"))
		apiKeys.DELETE("/{id}", placeholderAPIHandler("revoke key"))

		// Cron: clean up expired sessions every 5 minutes
		// Placeholder - full implementation with pbstore in Task 4
		app.Cron().MustAdd("expiry_cleanup", "*/5 * * * *", func() {
			log.Printf("Running expired session cleanup (placeholder)")
			// Full implementation: pbstore.DeleteExpiredSessions(app)
		})

		log.Printf("signaling server listening on :%s", cfg.Port)
		log.Printf("database path: %s", cfg.DBPath)

		return se.Next()
	})

	// Set port and data directory explicitly from config
	// PocketBase uses --dir for data and --http for port via CLI args
	os.Args = append(os.Args, "--http="+cfg.Port, "--dir="+cfg.DBPath)

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

// placeholderWSHandler returns a placeholder WebSocket handler
func placeholderWSHandler(name string) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		return apis.NewApiError(http.StatusNotImplemented, name+" WebSocket not yet implemented", nil)
	}
}

// placeholderSessionHandler returns a placeholder session handler
func placeholderSessionHandler() func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		return apis.NewApiError(http.StatusNotImplemented, "session lookup not yet implemented", nil)
	}
}

// placeholderAPIHandler returns a placeholder API handler
func placeholderAPIHandler(name string) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		e.Response.Header().Set("Content-Type", "application/json")
		e.Response.WriteHeader(http.StatusNotImplemented)
		e.Response.Write([]byte(`{"message":"` + name + ` not yet implemented"}`))
		return nil
	}
}
