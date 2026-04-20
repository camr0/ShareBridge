package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/handler"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/metrics"
	"sharebridge/server/internal/middleware"
	"sharebridge/server/internal/quota"
	"sharebridge/server/internal/relay"
	_ "sharebridge/server/migrations"
)

func main() {
	cfg := config.Load()
	h := hub.New()
	reg := relay.NewRegistry(cfg.RelayPendingWaitWindow)

	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir: cfg.DataDir,
	})

	// IMPORTANT: wire PocketBase to cfg.DataDir and cfg.Port explicitly.

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

		// WebSocket endpoints
		router.GET("/ws/agent", func(e *core.RequestEvent) error {
			// Apply API key auth middleware then handler
			authMiddleware := middleware.APIKeyAuth(app)
			handlerFunc := handler.AgentWS(app, h, reg, cfg)
			authMiddleware(http.HandlerFunc(handlerFunc)).ServeHTTP(e.Response, e.Request)
			return nil
		})

		router.GET("/ws/client", func(e *core.RequestEvent) error {
			handler.BrowserWS(app, h, cfg)(e.Response, e.Request)
			return nil
		})

		router.GET("/ws/relay", func(e *core.RequestEvent) error {
			handler.RelayWS(app, reg, cfg)(e.Response, e.Request)
			return nil
		})

		// Public session info endpoint
		router.GET("/sessions/{code}", handler.GetSessionInfo(app, h))

		// Direct link route - serves file client; JS reads code from window.location
		router.GET("/s/{code}", handler.ServeFile("./web/index.html"))

		// Homepage (marketing)
		router.GET("/", handler.ServeFile("./web/home.html"))

		// File transfer client (manual join)
		router.GET("/join", handler.ServeFile("./web/index.html"))

		// Static assets for file client
		router.GET("/app.js", handler.ServeFile("./web/app.js"))
		router.GET("/src/{path...}", handler.ServeDir("./web/src"))
		router.GET("/noise-p256/{path...}", handler.ServeDir("./web/noise-p256"))

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
		apiKeys.POST("/{id}/rotate", handler.RotateAPIKey(app, h))
		apiKeys.DELETE("/{id}", handler.RevokeAPIKey(app, h))

		// Account endpoints
		// GET /api/account - JWT auth for browser account dashboard
		accountGroup := router.Group("/api/account")
		accountGroup.Bind(apis.RequireAuth())
		accountGroup.GET("", handler.GetAccount(app))

		// GET /api/account/quota - API key auth for agent to fetch quota info
		router.GET("/api/account/quota", func(e *core.RequestEvent) error {
			authMiddleware := middleware.APIKeyAuth(app)
			handlerFunc := handler.GetAccountQuota(app)
			authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestEvent := new(core.RequestEvent)
				requestEvent.App = app
				requestEvent.Request = r
				requestEvent.Response = w
				err := handlerFunc(requestEvent)
				if err != nil {
					_ = err
				}
			})).ServeHTTP(e.Response, e.Request)
			return nil
		})

		// Cron: clean up expired sessions every 5 minutes
		app.Cron().MustAdd("expiry_cleanup", "*/5 * * * *", func() {
			if err := deleteExpiredSessions(app); err != nil {
				log.Printf("error cleaning expired sessions: %v", err)
			}
		})

		// Relay registry entries are short-lived and in-memory only. Clean up
		// expired pending sessions so direct-mode success does not leak them until
		// process restart.
		app.Cron().MustAdd("relay_registry_cleanup", "* * * * *", func() {
			reg.CleanupExpired(time.Now().UTC())
		})

		// Initialize quota fields when a new user registers.
		app.OnRecordCreate("users").BindFunc(func(e *core.RecordEvent) error {
			now := time.Now().UTC()
			e.Record.Set("relay_quota_gb", cfg.DefaultQuotaGB)
			e.Record.Set("current_period_usage_gb", 0.0)
			e.Record.Set("quota_period_start", now)
			e.Record.Set("quota_period_end", now.Add(30*24*time.Hour))
			e.Record.Set("turn_baseline_bytes", 0.0)
			return e.Next()
		})

		log.Printf("signaling server listening on :%s", cfg.Port)
		log.Printf("pocketbase data dir: %s", cfg.DataDir)

		// Initialize and start the quota poller only when TURN is configured
		if cfg.HasTurn() {
			metricsClient := metrics.NewPrometheusClient(cfg.PrometheusURL)
			quotaPoller := quota.NewPoller(app, metricsClient, cfg)
			quotaPoller.Start()
			log.Printf("quota poller started with interval: %v", cfg.QuotaCheckInterval)
		}

		return se.Next()
	})

	// Set the listen address explicitly from config.
	os.Args = append(os.Args, "--http=0.0.0.0:"+cfg.Port)

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
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
