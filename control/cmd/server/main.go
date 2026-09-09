package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
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
	"sharebridge/control/internal/relayctl"
	"sharebridge/control/internal/stun"
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

	// Task 12 route publisher: derives exact relay routes from PocketBase
	// rows and feeds them to agent-WS lifecycle hooks (add after successful
	// registration, revoke before local lifecycle removal) while Run
	// refreshes the gateway's finite 120-second route leases every 30
	// seconds while sync is healthy (§14). Zero config selects the §14
	// defaults; agents without a relay assignment produce no routes, so
	// deployments with relay unconfigured behave exactly as before.
	// Constructed BEFORE the controller: the controller's §9.1 relay terms
	// join presence against the publisher's current route revision (Task 15
	// read-then-check contract).
	routePublisher, err := relayctl.NewPublisher(app, relayctl.PublisherConfig{})
	if err != nil {
		log.Fatalf("route publisher: %v", err)
	}

	// Task 15 presence view: control's gateway-authoritative relay
	// availability. The Task 11 sync server feeds it via PresenceSink
	// (ApplyPresenceSnapshot/ApplyPresenceEvents); the sync LISTENER itself
	// (relayctl.NewServer TLS endpoint plus its certificate/env plumbing) is
	// deliberately NOT wired here — it belongs to a later task's file list.
	// Until that listener ships, the view stays empty in production, so
	// every relay term fails CLOSED (RelayAvailable is always false — never
	// a faked availability): direct candidates get the interstitial, and
	// relay-dependent cases answer 503, exactly as before this wiring.
	presenceView, err := relayctl.NewPresenceView(relayctl.PresenceViewConfig{
		App:    app, // best-effort relay_last_seen_at diagnostics (§12)
		Routes: routePublisher,
	})
	if err != nil {
		log.Fatalf("relay presence view: %v", err)
	}

	// Task 22: the three control-authored route-interstitial assets, loaded
	// once at startup (a missing or oversized asset fails the process closed
	// — the §9.3 page cannot render without it). The raw HTML template is
	// never served; it exists only as rendered by the controller with a
	// per-response nonce and the session-derived CSP.
	interstitialAssets, err := directctl.LoadInterstitialAssets("./web")
	if err != nil {
		log.Fatalf("interstitial assets: %v", err)
	}

	ctrl := directctl.NewController(app, h, coord, dnsClient, directctl.Config{
		BaseDomain:            cfg.BaseDomain,
		RelayGatewayIPv4:      cfg.RelayGatewayIPv4,
		RelaySelectionEnabled: cfg.RelaySelectionEnabled,
		RelayPresence:         presenceView,
		Routes:                routePublisher,
		InterstitialAssets:    interstitialAssets,
	})

	publisherCtx, stopPublisher := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopPublisher()
	go routePublisher.Run(publisherCtx)

	// Task 16: authenticated STUN observation listener (§10.1, §16.4). UDP
	// 3478 is the only new public control listener; a misconfigured bind
	// address or an unbindable port fails the process closed at startup.
	// The receipt key is fresh per process: pending challenges and accepted
	// observations are process-local state that die with it, and agents
	// re-challenge immediately after reconnect (§10.2). Challenge scheduling
	// over the agent WebSocket is Task 18's wiring; the issuer/claimer API
	// (IssueChallenge/TakeObservation) is served by this instance.
	if cfg.STUNEnabled() {
		if _, err := stun.ParseBindAddr(cfg.STUNBindAddr); err != nil {
			log.Fatalf("stun listener: %v", err)
		}
		stunKey, err := stun.NewKey()
		if err != nil {
			log.Fatalf("stun listener: %v", err)
		}
		stunServer, err = stun.NewServer(stun.Config{
			Key:      stunKey,
			BindAddr: cfg.STUNBindAddr,
		})
		if err != nil {
			log.Fatalf("stun listener: %v", err)
		}
		stunConn, err := stunServer.Listen()
		if err != nil {
			log.Fatalf("stun listener: %v", err)
		}
		// The advertise address (the stun_challenge `server` field agents
		// dial, §10.2) is STUN_ADVERTISE_ADDR when set — validated as a
		// public host:port by cfg.STUNAdvertise and advertised verbatim; an
		// invalid value fails the process closed above. Unset ⇒ the
		// STUN_BIND_ADDR value is advertised verbatim, which is DEV-ONLY
		// whenever the bind is loopback/0.0.0.0: agents behind NAT can never
		// reach such an address and direct mode fails closed to relay
		// (§10.3). Task 25's real-NAT gate exercises a publicly resolvable
		// advertise address.
		advertise, err := cfg.STUNAdvertise()
		if err != nil {
			log.Fatalf("stun listener: %v", err)
		}
		stunCtx, stopStun := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stopStun()
		go func() {
			if err := stunServer.Serve(stunCtx, stunConn); err != nil {
				log.Printf("stun listener stopped: %v", err)
			}
		}()
		// Task 18: install the listener on the controller so the §10.2
		// scheduler issues stun_challenge over the agent WebSocket (immediate
		// after enrollment_ready, re-challenge at 4 min + jitter) and claims
		// observations via stun_result at the current WS epoch.
		ctrl.EnableSTUN(stunServer, advertise)
		log.Printf("stun observation listener on %s (udp/3478)", cfg.STUNBindAddr)
	}

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
			handlerFunc := handler.AgentWS(app, h, cfg, ctrl, routePublisher)
			authMiddleware(http.HandlerFunc(handlerFunc)).ServeHTTP(e.Response, e.Request)
			return nil
		})

		// Canonical routes: /share/{code} and /s/{code} both resolve through
		// the single tombstone-aware resolver (§8) and then — for lifecycle-
		// active shares — the §9.1 route-selection matrix: 200 no-store
		// interstitial for direct candidates, 302 to the control-constructed
		// relay origin for relayOnly/hard-direct-ineligible sessions when
		// RELAY_SELECTION_ENABLED=true and a presence lease backs it, and the
		// 503 offline page otherwise. Expired/unsupported return 410 and
		// revoked/unknown return 404. The legacy Phase 3 direct 302 is not
		// part of this dispatch in any flag state.
		router.GET("/share/{code}", serveCanonicalRoute(ctrl))

		router.GET("/s/{code}", serveCanonicalRoute(ctrl))

		// Task 21 bounded preparation endpoint (§9.3): the interstitial's
		// same-origin fetch. The caller is the unauthenticated recipient
		// holding the native share code (the bearer capability) — no auth
		// middleware. One four-second preparation context covers the inline
		// STUN refresh, open_signal/ack and the verified-tuple probe; the
		// response is bounded no-store JSON with control-derived URLs only
		// (404 revoked/unknown, 410 expired/unsupported inside the handler).
		router.POST("/api/shares/{code}/prepare-route", func(e *core.RequestEvent) error {
			return ctrl.PrepareRoute(e.Response, e.Request, e.Request.PathValue("code"))
		})

		// Task 22: the interstitial's two same-origin assets (exact paths the
		// rendered §9.3 page references; nonce-authorized by its CSP). Both
		// are no-store, like every other response on this host.
		router.GET("/web/route-interstitial.js", func(e *core.RequestEvent) error {
			directctl.ServeInterstitialAsset(e.Response, interstitialAssets.JS, "text/javascript; charset=utf-8")
			return nil
		})
		router.GET("/web/route-interstitial.css", func(e *core.RequestEvent) error {
			directctl.ServeInterstitialAsset(e.Response, interstitialAssets.CSS, "text/css; charset=utf-8")
			return nil
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
			if err := deleteExpiredSessions(app, routePublisher); err != nil {
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

// stunServer holds the Task 16 listener instance for the Task 18 challenge
// scheduler: the agent-WS handler will call stunServer.IssueChallenge when it
// emits stun_challenge and stunServer.TakeObservation when stun_result
// arrives. Package-level because main itself does not read it yet.
var stunServer *stun.Server

func deleteExpiredSessions(app core.App, routes handler.RoutePublisher) error {
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
		// Revoke the relay route before the local lifecycle removal (plan
		// Task 12): the gateway stops routing no later than the share
		// disappears control-side. Publishing first is fail-closed — a
		// failed save leaves a temporarily unroutable share, never a
		// routable tombstone.
		if err := routes.PublishRevoke(record.Id); err != nil {
			return fmt.Errorf("revoke relay route for expired session %s: %w", record.Id, err)
		}
		record.Set("is_active", false)
		record.Set("inactive_reason", "expired")
		if err := app.Save(record); err != nil {
			return fmt.Errorf("soft-delete expired session %s: %w", record.Id, err)
		}
	}

	return nil
}

// serveCanonicalRoute resolves a /share/{code} or /s/{code} canonical
// navigation through the tombstone-aware lifecycle resolver and, for active
// shares, the §9.1 route-selection matrix (directctl.SelectRoute):
// expired/unsupported return 410; revoked/unknown return 404; lifecycle-
// active shares get the interstitial, a relay 302, or the 503 offline page
// per the live predicates. Never a legacy direct 302.
func serveCanonicalRoute(ctrl *directctl.Controller) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		code := e.Request.PathValue("code")
		rec, status := ctrl.ResolveForRedirect(code)
		switch status {
		case http.StatusFound:
			return ctrl.SelectRoute(e.Response, e.Request, rec, code)
		case http.StatusGone:
			return e.Error(http.StatusGone, "share expired or unsupported", nil)
		default:
			return e.NotFoundError("session not found", nil)
		}
	}
}
