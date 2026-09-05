package main

import (
	"context"
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
	"sharebridge/control/internal/certcoordinator"
	"sharebridge/control/internal/config"
	"sharebridge/control/internal/ddns"
	"sharebridge/control/internal/directctl"
	"sharebridge/control/internal/handler"
	"sharebridge/control/internal/hub"
	"sharebridge/control/internal/middleware"
	_ "sharebridge/control/migrations"
)

func main() {
	cfg := config.Load()
	h := hub.New()

	app := pocketbase.NewWithConfig(pocketbase.Config{
		DefaultDataDir: cfg.DataDir,
	})

	// Build the direct-mode control plane. A missing Cloudflare token simply
	// leaves the DDNS client nil, so the controller degrades gracefully and the
	// legacy relay path keeps working unconfigured.
	coord, err := certcoordinator.NewCoordinator(certcoordinator.CoordinatorConfig{
		CA:              cfg.ACMECADir,
		Email:           cfg.ACMEEmail,
		CloudflareToken: cfg.CloudflareToken,
		BaseDomain:      cfg.BaseDomain,
		AccountKeyPath:  filepath.Join(cfg.DataDir, "acme_account.pem"),
	})
	if err != nil {
		log.Fatalf("cert coordinator: %v", err)
	}

	var dnsClient *ddns.Cloudflare
	if cfg.CloudflareToken != "" && cfg.BaseDomain != "" {
		ddnsCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dnsClient, err = ddns.New(ddnsCtx, cfg.CloudflareToken, cfg.BaseDomain)
		if err != nil {
			log.Printf("warning: ddns unavailable, direct mode disabled: %v", err)
			dnsClient = nil
		}
	}

	ctrl := directctl.NewController(app, h, coord, dnsClient, directctl.Config{
		BaseDomain:       cfg.BaseDomain,
		RelayGatewayIPv4: cfg.RelayGatewayIPv4,
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
			handlerFunc := handler.AgentWS(app, h, cfg, ctrl)
			authMiddleware(http.HandlerFunc(handlerFunc)).ServeHTTP(e.Response, e.Request)
			return nil
		})

		// Redirect /share/{code} and /s/{code} through the single tombstone-aware
		// resolver. Active gallery shares 302 to the agent's direct origin;
		// expired/unsupported return 410 and revoked/unknown return 404 (§8).
		router.GET("/share/{code}", serveShareRedirect(ctrl))

		router.GET("/s/{code}", func(e *core.RequestEvent) error {
			code := e.Request.PathValue("code")
			_, status := ctrl.ResolveForRedirect(code)
			switch status {
			case http.StatusFound:
				return ctrl.Redirect(e.Response, e.Request, code)
			case http.StatusGone:
				return e.Error(http.StatusGone, "share expired or unsupported", nil)
			default:
				return e.NotFoundError("session not found", nil)
			}
		})
		// Homepage (marketing)
		router.GET("/", handler.ServeFileNoCache("./web/home.html"))

		// User-facing pages (placeholders - full implementation in Task 10)
		router.GET("/register", handler.ServeFileNoCache("./web/register.html"))
		router.GET("/login", handler.ServeFileNoCache("./web/login.html"))
		router.GET("/account", handler.ServeFileNoCache("./web/account.html"))

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

		log.Printf("control server listening on :%s", cfg.Port)
		log.Printf("pocketbase data dir: %s", cfg.DataDir)

		return se.Next()
	})

	// Set the listen address explicitly from config.
	os.Args = append(os.Args, "--http=0.0.0.0:"+cfg.Port)

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}

func deleteExpiredSessions(app core.App) error {
	records, err := app.FindAllRecords("sessions")
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	for _, record := range records {
		// Only transition still-active rows: an already-inactive row's
		// inactive_reason is authoritative and must not be overwritten (a
		// revoked or unsupported session stays revoked/unsupported even after
		// its original lease lapses).
		if !record.GetBool("is_active") {
			continue
		}
		expiresAt := record.GetDateTime("expires_at")
		if expiresAt.IsZero() || expiresAt.Time().After(now) {
			continue
		}
		record.Set("is_active", false)
		record.Set("inactive_reason", "expired")
		if err := app.Save(record); err != nil {
			return fmt.Errorf("soft-delete expired session %s: %w", record.Id, err)
		}
	}

	return nil
}

// serveShareRedirect resolves a /share/{code} code through the tombstone-aware
// resolver and 302s active gallery shares to the agent's direct origin.
// Expired/unsupported return 410; revoked/unknown return 404 (§8).
func serveShareRedirect(ctrl *directctl.Controller) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		code := e.Request.PathValue("code")
		_, status := ctrl.ResolveForRedirect(code)
		switch status {
		case http.StatusFound:
			return ctrl.Redirect(e.Response, e.Request, code)
		case http.StatusGone:
			return e.Error(http.StatusGone, "share expired or unsupported", nil)
		default:
			return e.NotFoundError("session not found", nil)
		}
	}
}
