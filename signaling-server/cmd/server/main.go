package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/handler"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/middleware"
	_ "sharebridge/server/migrations"
)

func main() {
	cfg := config.Load()
	h := hub.New()
	dataDir := pocketBaseDataDir(cfg.DBPath)

	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir: dataDir,
	})

	// IMPORTANT: wire PocketBase to cfg.DBPath and cfg.Port explicitly.
	// PocketBase expects a data directory, while DATABASE_PATH is historically a file path.
	// We preserve the deployment contract by deriving the PocketBase data dir from it.

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

		// WebSocket endpoints
		router.GET("/ws/agent", func(e *core.RequestEvent) error {
			// Apply API key auth middleware then handler
			authMiddleware := middleware.APIKeyAuth(app)
			handlerFunc := handler.AgentWS(app, h, cfg)
			authMiddleware(http.HandlerFunc(handlerFunc)).ServeHTTP(e.Response, e.Request)
			return nil
		})

		router.GET("/ws/client", func(e *core.RequestEvent) error {
			handler.BrowserWS(app, h, cfg)(e.Response, e.Request)
			return nil
		})

		// Public session info endpoint
		router.GET("/sessions/{code}", handler.GetSessionInfo(app, h))

		// Direct link route - serves index.html; JS reads code from window.location
		router.GET("/s/{code}", handler.ServeFile("./web/index.html"))

		// Static web files
		router.GET("/", handler.ServeFile("./web/index.html"))
		router.GET("/app.js", handler.ServeFile("./web/app.js"))

		// User-facing pages (placeholders - full implementation in Task 10)
		router.GET("/register", handler.ServeFile("./web/register.html"))
		router.GET("/login", handler.ServeFile("./web/login.html"))
		router.GET("/account", handler.ServeFile("./web/account.html"))

		// User-scoped API key management (requires JWT auth)
		// Uses Bind middleware for auth (apis.RequireAuth returns *hook.Handler)
		apiKeys := router.Group("/api/keys")
		apiKeys.Bind(apis.RequireAuth())
		apiKeys.POST("", handler.CreateAPIKey(app))
		apiKeys.GET("", handler.ListAPIKeys(app))
		apiKeys.DELETE("/{id}", handler.RevokeAPIKey(app, h))

		// Cron: clean up expired sessions every 5 minutes
		app.Cron().MustAdd("expiry_cleanup", "*/5 * * * *", func() {
			if err := deleteExpiredSessions(app); err != nil {
				log.Printf("error cleaning expired sessions: %v", err)
			}
		})

		log.Printf("signaling server listening on :%s", cfg.Port)
		log.Printf("database path: %s", cfg.DBPath)
		log.Printf("pocketbase data dir: %s", dataDir)

		return se.Next()
	})

	// Set the listen address explicitly from config.
	os.Args = append(os.Args, "--http=0.0.0.0:"+cfg.Port)

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

func pocketBaseDataDir(dbPath string) string {
	if dbPath == "" {
		return ""
	}
	clean := filepath.Clean(dbPath)
	if filepath.Ext(clean) == ".db" {
		return filepath.Dir(clean)
	}
	return clean
}

func deleteExpiredSessions(app core.App) error {
	for {
		records, err := app.FindRecordsByFilter(
			"sessions",
			"expires_at <= {:now}",
			"",
			500,
			0,
			map[string]any{"now": time.Now().UTC().Format(time.RFC3339)},
		)
		if err != nil {
			return err
		}
		if len(records) == 0 {
			return nil
		}

		for _, record := range records {
			if err := app.Delete(record); err != nil {
				return fmt.Errorf("delete expired session %s: %w", record.Id, err)
			}
		}
	}
}
