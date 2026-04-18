package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	pbrouter "github.com/pocketbase/pocketbase/tools/router"
	"sharebridge/server/internal/config"
	"sharebridge/server/internal/handler"
	"sharebridge/server/internal/hub"
	"sharebridge/server/internal/middleware"
	"sharebridge/server/internal/quota"
	"sharebridge/server/internal/relay"
	_ "sharebridge/server/migrations"
)

func main() {
	cfg := config.Load()
	h := hub.New()

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

		// Construct and start the libp2p relay host.
		ctx := context.Background()
		rly, err := relay.New(ctx, relay.Config{
			ListenAddr:     cfg.RelayListenAddr,
			AnnounceAddr:   cfg.RelayAnnounceAddr,
			PrivateKeyPath: cfg.RelayPrivateKeyPath,
			JWTSecret:      cfg.JWTSecret,
			JWTTTL:         cfg.JWTTTL,
		})
		if err != nil {
			log.Fatalf("relay: %v", err)
		}

		// Wire share_code -> api_key_id resolver for relay auth.
		rly.SetCodeResolver(func(code string) (string, bool) {
			records, err := app.FindRecordsByFilter("sessions", "code = {:code}", "", 1, 0, map[string]any{"code": code})
			if err != nil || len(records) == 0 {
				return "", false
			}
			return records[0].GetString("api_key_id"), true
		})

		// Wire quota accumulator to relay's circuit-closed hook.
		acc := quota.NewAccumulator(app, cfg)
		acc.Start()
		rly.SetCircuitClosedHook(func(apiKeyID, _ string, bytesIn, bytesOut int64) {
			acc.Record(apiKeyID, bytesIn, bytesOut)
		})
		rly.Start()
		log.Printf("relay host %s listening on %v", rly.Host().ID(), rly.Host().Addrs())

		// WebSocket endpoints
		router.GET("/ws/agent", func(e *core.RequestEvent) error {
			// Apply API key auth middleware then handler
			authMiddleware := middleware.APIKeyAuth(app)
			handlerFunc := handler.AgentWS(app, h, cfg, rly)
			authMiddleware(http.HandlerFunc(handlerFunc)).ServeHTTP(e.Response, e.Request)
			return nil
		})

		router.GET("/ws/client", func(e *core.RequestEvent) error {
			handler.BrowserWS(app, h, cfg, rly)(e.Response, e.Request)
			return nil
		})

		router.GET("/ws/debug", func(e *core.RequestEvent) error {
			handler.DebugWS(cfg)(e.Response, e.Request)
			return nil
		})

		// Public session info endpoint
		router.GET("/sessions/{code}", handler.GetSessionInfo(app, h))
		registerStaticRoutes(router, "./web")

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

		// Initialize quota fields when a new user registers.
		app.OnRecordCreate("users").BindFunc(func(e *core.RecordEvent) error {
			now := time.Now().UTC()
			e.Record.Set("relay_quota_gb", cfg.DefaultQuotaGB)
			e.Record.Set("current_period_usage_gb", 0.0)
			e.Record.Set("quota_period_start", now)
			e.Record.Set("quota_period_end", now.Add(30*24*time.Hour))
			return e.Next()
		})

		// Shutdown cleanup: stop accumulator and close relay.
		app.OnTerminate().BindFunc(func(_ *core.TerminateEvent) error {
			acc.Stop()
			return rly.Close()
		})

		log.Printf("signaling server listening on :%s", cfg.Port)
		log.Printf("pocketbase data dir: %s", cfg.DataDir)

		return se.Next()
	})

	// Preserve an explicit CLI --http flag; otherwise add the default listen addr.
	os.Args = effectiveHTTPArgs(os.Args, cfg.Port)

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

func effectiveHTTPArgs(args []string, port string) []string {
	for i, arg := range args {
		if arg == "--http" && i+1 < len(args) {
			return args
		}
		if strings.HasPrefix(arg, "--http=") {
			return args
		}
	}
	return append(args, "--http=0.0.0.0:"+port)
}

func registerStaticRoutes(router *pbrouter.Router[*core.RequestEvent], webRoot string) {
	// Direct link route - serves file client; JS reads code from window.location
	router.GET("/s/{code}", handler.ServeFile(filepath.Join(webRoot, "index.html")))

	// Homepage (marketing)
	router.GET("/", handler.ServeFile(filepath.Join(webRoot, "home.html")))

	// File transfer client (manual join)
	router.GET("/join", handler.ServeFile(filepath.Join(webRoot, "index.html")))

	// Static assets for file client
	router.GET("/app.bundle.js", handler.ServeFile(filepath.Join(webRoot, "app.bundle.js")))
	router.GET("/app.bundle.js.map", handler.ServeFile(filepath.Join(webRoot, "app.bundle.js.map")))
	router.GET("/app.js", handler.ServeFile(filepath.Join(webRoot, "app.bundle.js")))

	// User-facing pages (placeholders - full implementation in Task 10)
	router.GET("/register", handler.ServeFile(filepath.Join(webRoot, "register.html")))
	router.GET("/login", handler.ServeFile(filepath.Join(webRoot, "login.html")))
	router.GET("/account", handler.ServeFile(filepath.Join(webRoot, "account.html")))
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
