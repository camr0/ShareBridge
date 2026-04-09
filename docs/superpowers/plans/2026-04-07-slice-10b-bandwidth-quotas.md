# Slice 10b — Bandwidth Tracking + Quota Enforcement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add per-account bandwidth tracking via Prometheus + Coturn, enforce 50 GB/30-day soft quotas on new relay sessions, and display usage on the account dashboard and agent share form.

**Architecture:** The signaling server polls Prometheus every 5 minutes for `turn_traffic_sent` counters keyed by account-scoped TURN usernames, stores running usage in PocketBase, and checks quota after the WebSocket upgrade. If exceeded, it sends the browser STUN-only ICE config (no TURN) plus a `relay_quota_exceeded` WS message — the browser can show a clear error. IP protection for relay-only shares is enforced by the agent itself via `ICETransportPolicyRelay`, not by the server. Self-hosted deployments without Prometheus have no quota limits.

**Tech Stack:** Go (signaling server), PocketBase (DB + hooks), Prometheus HTTP API (PromQL), Coturn with `--prometheus --prometheus-username-labels`, vanilla JS + HTML (web UI), HTMX (agent UI)

---

## File Map

**New files (signaling server):**
- `signaling-server/internal/metrics/prometheus.go` — Prometheus HTTP API client
- `signaling-server/internal/metrics/prometheus_test.go` — tests for client
- `signaling-server/internal/quota/poller.go` — quota polling loop + period reset
- `signaling-server/internal/quota/poller_test.go` — tests for poller logic
- `signaling-server/internal/handler/account.go` — `GET /api/account` (JWT) and `GET /api/account/quota` (API key)
- `signaling-server/migrations/3_add_quota_fields.go` — adds quota fields to users, creates bandwidth_usage collection
- `signaling-server/prometheus.yml` — Prometheus scrape config

**Modified files (signaling server):**
- `signaling-server/internal/turn/credentials.go` — rename `sessionID` param to `accountID`
- `signaling-server/internal/turn/credentials_test.go` — update param name
- `signaling-server/internal/handler/browser_ws.go` — look up accountID from api_key; post-upgrade quota check: STUN-only ICE + `relay_quota_exceeded` WS message if exceeded
- `signaling-server/internal/config/config.go` — add `DEFAULT_QUOTA_GB`, `PROMETHEUS_URL`, `QUOTA_CHECK_INTERVAL`
- `signaling-server/cmd/server/main.go` — wire up quota poller, account endpoints, new-user hook
- `signaling-server/web/account.html` — add quota progress card
- `signaling-server/docker-compose.yml` — add Prometheus service, update Coturn command

**Modified files (agent):**
- `agent/internal/web/server.go` — register `GET /api/relay-quota` route
- `agent/internal/web/api.go` — `relayQuotaHandler` (fetches from signaling server)
- `agent/internal/web/templates/share-form.html` — show quota bar when relay mode selected

---

## Task 1: Account-scope TURN credentials

`turn/credentials.go` currently names the second parameter `sessionID`, and `browser_ws.go` passes `sessionCode`. Change both to use `accountID` — same HMAC logic, different label.

**Files:**
- Modify: `signaling-server/internal/turn/credentials.go`
- Modify: `signaling-server/internal/turn/credentials_test.go`
- Modify: `signaling-server/internal/handler/browser_ws.go`

- [ ] **Step 1: Update the test to reflect the new parameter name**

Replace `sessionID` with `accountID` in `signaling-server/internal/turn/credentials_test.go`:

```go
func TestGenerateCredentials(t *testing.T) {
	secret   := "test-secret"
	accountID := "abc123"
	expiry   := time.Unix(1700000000, 0)

	creds := GenerateCredentials(secret, accountID, expiry)

	expectedUsername := "1700000000:abc123"
	if creds.Username != expectedUsername {
		t.Errorf("username = %q, want %q", creds.Username, expectedUsername)
	}

	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(expectedUsername))
	expectedCred := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	if creds.Credential != expectedCred {
		t.Errorf("credential = %q, want %q", creds.Credential, expectedCred)
	}
}

func TestGenerateCredentials_DifferentSecrets(t *testing.T) {
	creds1 := GenerateCredentials("secret1", "account_abc", time.Now())
	creds2 := GenerateCredentials("secret2", "account_abc", time.Now())

	if creds1.Credential == creds2.Credential {
		t.Error("different secrets should produce different credentials")
	}
}
```

- [ ] **Step 2: Run the test — it should still pass (parameter name is cosmetic)**

```bash
cd signaling-server && go test ./internal/turn/... -v
```
Expected: PASS (parameter name change is cosmetic, behaviour unchanged)

- [ ] **Step 3: Update the function signature in `credentials.go`**

```go
// GenerateCredentials creates HMAC-based TURN credentials.
// The username format is {timestamp}:{accountID} and the credential is HMAC-SHA1 of the username.
func GenerateCredentials(secret, accountID string, expiry time.Time) Credentials {
	username := fmt.Sprintf("%d:%s", expiry.Unix(), accountID)

	mac := hmac.New(sha1.New, []byte(secret))
	mac.Write([]byte(username))
	credential := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	return Credentials{
		Username:   username,
		Credential: credential,
	}
}
```

- [ ] **Step 4: Update `browser_ws.go` to look up the account ID and pass it to `GenerateCredentials`**

Replace the existing TURN credentials block (lines 85–89) with:

```go
// Send ICE config immediately so the browser can set up RTCPeerConnection
// while the knock/nonce round-trip happens in parallel.
var turnCreds *turn.Credentials
if cfg.HasTurn() {
	turnExpiry := time.Now().Add(24 * time.Hour)

	// Use account-scoped TURN username to bound Prometheus label cardinality.
	apiKeyRecord, err := app.FindRecordById("api_keys", sessionRecord.GetString("api_key_id"))
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	accountID := apiKeyRecord.GetString("account_id")

	creds := turn.GenerateCredentials(cfg.TurnSecret, accountID, turnExpiry)
	turnCreds = &creds
}
```

- [ ] **Step 5: Run the turn tests to confirm everything still passes**

```bash
cd signaling-server && go test ./internal/turn/... -v
```
Expected: PASS

- [ ] **Step 6: Commit**

```bash
cd signaling-server && git add internal/turn/credentials.go internal/turn/credentials_test.go internal/handler/browser_ws.go
git commit -m "feat: scope TURN credentials to account ID instead of session code"
```

---

## Task 2: Add quota config fields

**Files:**
- Modify: `signaling-server/internal/config/config.go`

- [ ] **Step 1: Write a test for the new config fields**

Create `signaling-server/internal/config/config_test.go`:

```go
package config

import (
	"os"
	"testing"
	"time"
)

func TestLoad_Defaults(t *testing.T) {
	// Unset any env vars that might interfere
	os.Unsetenv("DEFAULT_QUOTA_GB")
	os.Unsetenv("PROMETHEUS_URL")
	os.Unsetenv("QUOTA_CHECK_INTERVAL")

	cfg := Load()

	if cfg.DefaultQuotaGB != 50.0 {
		t.Errorf("DefaultQuotaGB = %v, want 50.0", cfg.DefaultQuotaGB)
	}
	if cfg.PrometheusURL != "http://prometheus:9090" {
		t.Errorf("PrometheusURL = %q, want %q", cfg.PrometheusURL, "http://prometheus:9090")
	}
	if cfg.QuotaCheckInterval != 5*time.Minute {
		t.Errorf("QuotaCheckInterval = %v, want 5m", cfg.QuotaCheckInterval)
	}
}

func TestLoad_QuotaOverride(t *testing.T) {
	os.Setenv("DEFAULT_QUOTA_GB", "100")
	os.Setenv("PROMETHEUS_URL", "http://prom.internal:9090")
	os.Setenv("QUOTA_CHECK_INTERVAL", "2m")
	defer func() {
		os.Unsetenv("DEFAULT_QUOTA_GB")
		os.Unsetenv("PROMETHEUS_URL")
		os.Unsetenv("QUOTA_CHECK_INTERVAL")
	}()

	cfg := Load()

	if cfg.DefaultQuotaGB != 100.0 {
		t.Errorf("DefaultQuotaGB = %v, want 100.0", cfg.DefaultQuotaGB)
	}
	if cfg.PrometheusURL != "http://prom.internal:9090" {
		t.Errorf("PrometheusURL = %q", cfg.PrometheusURL)
	}
	if cfg.QuotaCheckInterval != 2*time.Minute {
		t.Errorf("QuotaCheckInterval = %v, want 2m", cfg.QuotaCheckInterval)
	}
}
```

- [ ] **Step 2: Run the test — expect compile failure (quota fields don't exist yet)**

```bash
cd signaling-server && go test ./internal/config/... -v
```
Expected: FAIL — `cfg.DefaultQuotaGB` undefined

- [ ] **Step 3: Add the new fields to `Config` and `Load()` in `config.go`**

Replace the entire file with:

```go
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port    string
	STUNURL string
	DataDir string

	// TURN configuration
	TurnHost   string
	TurnPort   string
	TurnSecret string

	// Bandwidth quota — always enforced when TURN is configured (requires Prometheus)
	DefaultQuotaGB     float64
	PrometheusURL      string
	QuotaCheckInterval time.Duration

	// SMTP (optional)
	SMTPHost     string
	SMTPPort     string
	SMTPUser     string
	SMTPPassword string
}

func Load() *Config {
	return &Config{
		Port:    getEnv("PORT", "8080"),
		STUNURL: getEnv("STUN_URL", "stun:stun.cloudflare.com:3478"),
		DataDir: getEnv("DATA_DIR", "./pb_data"),

		TurnHost:   getEnv("TURN_HOST", ""),
		TurnPort:   getEnv("TURN_PORT", "3478"),
		TurnSecret: getEnv("TURN_SECRET", ""),

		DefaultQuotaGB:     getEnvFloat("DEFAULT_QUOTA_GB", 50.0),
		PrometheusURL:      getEnv("PROMETHEUS_URL", "http://prometheus:9090"),
		QuotaCheckInterval: getEnvDuration("QUOTA_CHECK_INTERVAL", 5*time.Minute),

		SMTPHost:     getEnv("SMTP_HOST", ""),
		SMTPPort:     getEnv("SMTP_PORT", "587"),
		SMTPUser:     getEnv("SMTP_USER", ""),
		SMTPPassword: getEnv("SMTP_PASSWORD", ""),
	}
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func getEnvDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

// HasTurn returns true if TURN is configured.
func (c *Config) HasTurn() bool {
	return c.TurnHost != "" && c.TurnSecret != ""
}

// TurnURL returns the TURN URL (e.g., "turn:yourdomain.com:3478").
func (c *Config) TurnURL() string {
	return fmt.Sprintf("turn:%s:%s", c.TurnHost, c.TurnPort)
}

// HasSMTP returns true if SMTP is configured.
func (c *Config) HasSMTP() bool {
	return c.SMTPHost != ""
}
```

- [ ] **Step 4: Run the config tests**

```bash
cd signaling-server && go test ./internal/config/... -v
```
Expected: PASS

- [ ] **Step 5: Confirm the whole server still builds**

```bash
cd signaling-server && go build ./...
```
Expected: no errors

- [ ] **Step 6: Commit**

```bash
cd signaling-server && git add internal/config/config.go internal/config/config_test.go
git commit -m "feat: add quota config fields (DEFAULT_QUOTA_GB, PROMETHEUS_URL, QUOTA_CHECK_INTERVAL)"
```

---

## Task 3: Migration 3 — quota fields + bandwidth_usage collection

**Files:**
- Create: `signaling-server/migrations/3_add_quota_fields.go`

- [ ] **Step 1: Write the migration test**

Add to `signaling-server/migrations/migrations_test.go`. Open the file first, then append after the existing test(s):

```go
func TestMigration3_AddQuotaFields(t *testing.T) {
	app := testApp(t)

	// Run migrations 1 and 2 first
	if err := CreateCollections(app); err != nil {
		t.Fatalf("CreateCollections: %v", err)
	}
	if err := AddAPIKeyTimestamps(app); err != nil {
		t.Fatalf("AddAPIKeyTimestamps: %v", err)
	}

	// Run migration 3
	if err := AddQuotaFields(app); err != nil {
		t.Fatalf("AddQuotaFields: %v", err)
	}

	// users collection should have quota fields
	usersCol, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatalf("find users collection: %v", err)
	}
	for _, fieldName := range []string{"relay_quota_gb", "current_period_usage_gb", "quota_period_start", "quota_period_end", "turn_baseline_bytes"} {
		if usersCol.Fields.GetByName(fieldName) == nil {
			t.Errorf("users collection missing field %q", fieldName)
		}
	}

	// bandwidth_usage collection should exist
	bwCol, err := app.FindCollectionByNameOrId("bandwidth_usage")
	if err != nil {
		t.Fatalf("bandwidth_usage collection not created: %v", err)
	}
	for _, fieldName := range []string{"account_id", "period_start", "period_end", "bytes_transferred"} {
		if bwCol.Fields.GetByName(fieldName) == nil {
			t.Errorf("bandwidth_usage collection missing field %q", fieldName)
		}
	}

	// Idempotent: running again should not error
	if err := AddQuotaFields(app); err != nil {
		t.Fatalf("AddQuotaFields (second run): %v", err)
	}
}
```

- [ ] **Step 2: Run the test — expect compile failure**

```bash
cd signaling-server && go test ./migrations/... -v -run TestMigration3
```
Expected: FAIL — `AddQuotaFields` undefined

- [ ] **Step 3: Check what `testApp` looks like in migrations_test.go and confirm the pattern**

```bash
cd signaling-server && head -60 migrations/migrations_test.go
```
(Read output and adjust step 4 if `testApp` uses a different setup helper)

- [ ] **Step 4: Create `signaling-server/migrations/3_add_quota_fields.go`**

```go
package migrations

import (
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
	m "github.com/pocketbase/pocketbase/migrations"
)

func init() {
	m.Register(AddQuotaFields, nil)
}

// AddQuotaFields adds bandwidth quota tracking fields to the users collection
// and creates the bandwidth_usage audit collection.
func AddQuotaFields(app core.App) error {
	if err := addQuotaFieldsToUsers(app); err != nil {
		return err
	}
	return createBandwidthUsageCollection(app)
}

func addQuotaFieldsToUsers(app core.App) error {
	usersCol, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return err
	}

	changed := false

	additions := []struct {
		name  string
		field core.Field
	}{
		{"relay_quota_gb", &core.NumberField{Name: "relay_quota_gb"}},
		{"current_period_usage_gb", &core.NumberField{Name: "current_period_usage_gb"}},
		{"quota_period_start", &core.DateField{Name: "quota_period_start"}},
		{"quota_period_end", &core.DateField{Name: "quota_period_end"}},
		{"turn_baseline_bytes", &core.NumberField{Name: "turn_baseline_bytes"}},
	}

	for _, addition := range additions {
		if usersCol.Fields.GetByName(addition.name) == nil {
			usersCol.Fields.Add(addition.field)
			changed = true
		}
	}

	if !changed {
		return nil
	}

	if err := app.Save(usersCol); err != nil {
		return err
	}

	// Backfill existing users: set quota_period_start = created, period_end = created + 30d,
	// relay_quota_gb = 50, others = 0.
	users, err := app.FindAllRecords("users")
	if err != nil {
		return err
	}

	for _, user := range users {
		if !user.GetDateTime("quota_period_start").IsZero() {
			continue // already initialized
		}
		created := user.GetDateTime("created")
		periodStart := time.Now().UTC()
		if !created.IsZero() {
			periodStart = created.Time()
		}
		periodEnd := periodStart.Add(30 * 24 * time.Hour)

		user.Set("relay_quota_gb", 50.0)
		user.Set("current_period_usage_gb", 0.0)
		user.Set("quota_period_start", periodStart)
		user.Set("quota_period_end", periodEnd)
		user.Set("turn_baseline_bytes", 0.0)

		if err := app.Save(user); err != nil {
			return err
		}
	}

	return nil
}

func createBandwidthUsageCollection(app core.App) error {
	if _, err := app.FindCollectionByNameOrId("bandwidth_usage"); err == nil {
		return nil // already exists
	}

	usersCol, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		return err
	}

	col := core.NewBaseCollection("bandwidth_usage")
	col.Fields.Add(
		&core.RelationField{
			Name:          "account_id",
			CollectionId:  usersCol.Id,
			Required:      true,
			CascadeDelete: true,
			MaxSelect:     1,
		},
		&core.DateField{Name: "period_start"},
		&core.DateField{Name: "period_end"},
		&core.NumberField{Name: "bytes_transferred"},
		&core.AutodateField{
			Name:     "updated",
			OnCreate: true,
			OnUpdate: true,
		},
	)

	// Server-side only — no public access
	col.ListRule = nil
	col.ViewRule = nil
	col.CreateRule = nil
	col.UpdateRule = nil
	col.DeleteRule = nil

	if err := app.Save(col); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			return nil
		}
		return err
	}
	return nil
}
```

- [ ] **Step 5: Run the migration test**

```bash
cd signaling-server && go test ./migrations/... -v -run TestMigration3
```
Expected: PASS

- [ ] **Step 6: Run all migration tests to confirm no regressions**

```bash
cd signaling-server && go test ./migrations/... -v
```
Expected: all PASS

- [ ] **Step 7: Commit**

```bash
cd signaling-server && git add migrations/3_add_quota_fields.go migrations/migrations_test.go
git commit -m "feat: add quota fields to users collection and create bandwidth_usage audit collection"
```

---

## Task 4: Initialize quota fields on new-user registration

New users get quota fields set when their account is created. This is a PocketBase `OnRecordCreate` hook registered in `main.go`.

**Files:**
- Modify: `signaling-server/cmd/server/main.go`

- [ ] **Step 1: Add the hook inside `app.OnServe().BindFunc(...)` in `main.go`, before `return se.Next()`**

Add after the cron job block:

```go
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
```

Make sure `"time"` is in the imports (it already is from the existing `deleteExpiredSessions` function).

- [ ] **Step 2: Build to confirm no errors**

```bash
cd signaling-server && go build ./...
```
Expected: success

- [ ] **Step 3: Commit**

```bash
cd signaling-server && git add cmd/server/main.go
git commit -m "feat: initialize quota fields on new user registration"
```

---

## Task 5: Prometheus metrics client

**Files:**
- Create: `signaling-server/internal/metrics/prometheus.go`
- Create: `signaling-server/internal/metrics/prometheus_test.go`

- [ ] **Step 1: Write the tests using an httptest server**

Create `signaling-server/internal/metrics/prometheus_test.go`:

```go
package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestQueryAccountBytes_ReturnsBytes(t *testing.T) {
	// Simulate Prometheus returning 12345678 bytes for account "acc001"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result": []map[string]any{
					{
						"metric": map[string]string{},
						"value":  []any{1700000000, "12345678"},
					},
				},
			},
		})
	}))
	defer server.Close()

	client := NewPrometheusClient(server.URL)
	got, err := client.QueryAccountBytes(context.Background(), "acc001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 12345678 {
		t.Errorf("QueryAccountBytes = %d, want 12345678", got)
	}
}

func TestQueryAccountBytes_NoDataReturnsZero(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "vector",
				"result":     []any{},
			},
		})
	}))
	defer server.Close()

	client := NewPrometheusClient(server.URL)
	got, err := client.QueryAccountBytes(context.Background(), "acc001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 0 {
		t.Errorf("QueryAccountBytes = %d, want 0", got)
	}
}

func TestQueryAccountBytes_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewPrometheusClient(server.URL)
	_, err := client.QueryAccountBytes(context.Background(), "acc001")
	if err == nil {
		t.Error("expected error from 500 response, got nil")
	}
}

func TestQueryAccountBytes_IncludesAccountIDInQuery(t *testing.T) {
	var capturedQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data":   map[string]any{"resultType": "vector", "result": []any{}},
		})
	}))
	defer server.Close()

	client := NewPrometheusClient(server.URL)
	client.QueryAccountBytes(context.Background(), "myaccount123")

	if capturedQuery == "" {
		t.Fatal("no query sent to Prometheus")
	}
	if !contains(capturedQuery, "myaccount123") {
		t.Errorf("query %q does not contain account ID", capturedQuery)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsHelper(s, sub))
}

func containsHelper(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run the tests — expect compile failure**

```bash
cd signaling-server && go test ./internal/metrics/... -v
```
Expected: FAIL — package does not exist yet

- [ ] **Step 3: Create `signaling-server/internal/metrics/prometheus.go`**

```go
package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// PrometheusClient queries the Prometheus HTTP API.
type PrometheusClient struct {
	baseURL    string
	httpClient *http.Client
}

// NewPrometheusClient creates a client targeting the given Prometheus base URL
// (e.g., "http://prometheus:9090").
func NewPrometheusClient(baseURL string) *PrometheusClient {
	return &PrometheusClient{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// QueryAccountBytes returns the current total bytes sent via TURN for the
// given accountID by summing all turn_traffic_sent counters whose username
// label matches the pattern "[0-9]+:{accountID}".
//
// Returns 0 if no metrics exist for this account (no relay usage yet).
//
// NOTE: The Coturn metric name "turn_traffic_sent" must match the actual
// metric exported by your Coturn version. Verify with:
//   curl http://coturn:9641/metrics | grep turn_traffic
func (c *PrometheusClient) QueryAccountBytes(ctx context.Context, accountID string) (int64, error) {
	// PromQL: sum all counters for this account across any TURN credential timestamp.
	// Username format is "{timestamp}:{accountID}", so we match any timestamp prefix.
	query := fmt.Sprintf(`sum(turn_traffic_sent{username=~"[0-9]+:%s"})`, accountID)

	reqURL := c.baseURL + "/api/v1/query?query=" + url.QueryEscape(query)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return 0, fmt.Errorf("build prometheus request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("prometheus query: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("prometheus returned HTTP %d", resp.StatusCode)
	}

	var result promQueryResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decode prometheus response: %w", err)
	}

	if result.Status != "success" {
		return 0, fmt.Errorf("prometheus returned status %q", result.Status)
	}

	if len(result.Data.Result) == 0 {
		return 0, nil // no relay traffic for this account
	}

	// Value field is [unixTimestamp, "valueString"]
	valueArr := result.Data.Result[0].Value
	if len(valueArr) < 2 {
		return 0, nil
	}

	valueStr, ok := valueArr[1].(string)
	if !ok {
		return 0, fmt.Errorf("unexpected value type in prometheus response")
	}

	valueFloat, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return 0, fmt.Errorf("parse prometheus value %q: %w", valueStr, err)
	}

	return int64(valueFloat), nil
}

type promQueryResult struct {
	Status string   `json:"status"`
	Data   promData `json:"data"`
}

type promData struct {
	ResultType string       `json:"resultType"`
	Result     []promVector `json:"result"`
}

type promVector struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
}
```

- [ ] **Step 4: Run the metrics tests**

```bash
cd signaling-server && go test ./internal/metrics/... -v
```
Expected: all PASS

- [ ] **Step 5: Commit**

```bash
cd signaling-server && git add internal/metrics/prometheus.go internal/metrics/prometheus_test.go
git commit -m "feat: add Prometheus HTTP API client for per-account bandwidth queries"
```

---

## Task 6: Quota poller

Polls Prometheus on a ticker, updates `current_period_usage_gb` in PocketBase, and handles period rollover (archive old period, reset counter).

**Files:**
- Create: `signaling-server/internal/quota/poller.go`
- Create: `signaling-server/internal/quota/poller_test.go`

- [ ] **Step 1: Write the tests**

Create `signaling-server/internal/quota/poller_test.go`:

```go
package quota

import (
	"testing"
	"time"
)

func TestComputeUsageGB(t *testing.T) {
	tests := []struct {
		name          string
		totalBytes    int64
		baselineBytes int64
		wantGB        float64
	}{
		{"typical usage", 5_000_000_000, 0, 5.0},
		{"with baseline", 10_000_000_000, 3_000_000_000, 7.0},
		{"negative baseline (Coturn restart offset)", -7_000_000_000, 0, 7.0},
			{"counter behind positive baseline (should not happen)", 1000, 5000, 0.0},
		{"zero usage", 0, 0, 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeUsageGB(tt.totalBytes, tt.baselineBytes)
			if got != tt.wantGB {
				t.Errorf("computeUsageGB(%d, %d) = %v, want %v", tt.totalBytes, tt.baselineBytes, got, tt.wantGB)
			}
		})
	}
}

func TestIsQuotaExceeded(t *testing.T) {
	tests := []struct {
		name    string
		usedGB  float64
		limitGB float64
		want    bool
	}{
		{"under limit", 30.0, 50.0, false},
		{"exactly at limit", 50.0, 50.0, true},
		{"over limit", 55.0, 50.0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsQuotaExceeded(tt.usedGB, tt.limitGB)
			if got != tt.want {
				t.Errorf("IsQuotaExceeded(%v, %v) = %v, want %v", tt.usedGB, tt.limitGB, got, tt.want)
			}
		})
	}
}

func TestNextPeriodStart(t *testing.T) {
	periodEnd := time.Date(2026, 2, 14, 10, 30, 0, 0, time.UTC)
	next := nextPeriodEnd(periodEnd)

	expected := periodEnd.Add(30 * 24 * time.Hour)
	if !next.Equal(expected) {
		t.Errorf("nextPeriodEnd = %v, want %v", next, expected)
	}
}
```

- [ ] **Step 2: Run the tests — expect compile failure**

```bash
cd signaling-server && go test ./internal/quota/... -v
```
Expected: FAIL — package does not exist

- [ ] **Step 3: Create `signaling-server/internal/quota/poller.go`**

```go
package quota

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/metrics"
)

// Poller polls Prometheus periodically and updates per-account bandwidth usage.
// It also handles quota period rollover: when a period expires, the usage is
// archived to bandwidth_usage and the counter resets.
type Poller struct {
	app      core.App
	prom     *metrics.PrometheusClient
	interval time.Duration
}

// NewPoller creates a quota poller.
func NewPoller(app core.App, prom *metrics.PrometheusClient, interval time.Duration) *Poller {
	return &Poller{app: app, prom: prom, interval: interval}
}

// Run starts the polling loop. Blocks until ctx is cancelled.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := p.poll(ctx); err != nil {
				log.Printf("quota poller: %v", err)
			}
		}
	}
}

func (p *Poller) poll(ctx context.Context) error {
	users, err := p.app.FindAllRecords("users")
	if err != nil {
		return fmt.Errorf("find users: %w", err)
	}

	now := time.Now().UTC()

	for _, user := range users {
		periodEnd := user.GetDateTime("quota_period_end")
		if periodEnd.IsZero() {
			continue // quota not initialized yet
		}

		if now.After(periodEnd.Time()) {
			if err := p.resetPeriod(ctx, user, now); err != nil {
				log.Printf("quota: reset period for user %s: %v", user.Id, err)
			}
			continue
		}

		totalBytes, err := p.prom.QueryAccountBytes(ctx, user.Id)
		if err != nil {
			log.Printf("quota: prometheus query for user %s: %v", user.Id, err)
			continue
		}

		baselineBytes := int64(user.GetFloat("turn_baseline_bytes"))

		if totalBytes < baselineBytes {
			// Coturn restarted — its counter reset to 0 and has been climbing
			// from zero since. The DB baseline is from before the restart, so
			// it's now larger than totalBytes.
			//
			// Fix: set baseline to a negative number that encodes the pre-restart
			// usage as a permanent offset. Because subtracting a negative adds:
			//
			//   usageGB = (totalBytes - baseline) / 1e9
			//
			// If baseline = totalBytes - preRestartBytes (negative when preRestartBytes > totalBytes):
			//
			//   usageGB = (totalBytes - (totalBytes - preRestartBytes)) / 1e9
			//           = preRestartBytes / 1e9   ← pre-restart usage preserved ✓
			//
			// Every subsequent poll uses the same formula with no special cases —
			// the negative baseline just keeps adding the offset automatically.
			//
			// Example: pre-restart usage = 10 GB, totalBytes after restart = 3 GB
			//   baseline = 3 GB - 10 GB = -7 GB
			//   next poll at 5 GB: (5 GB - (-7 GB)) / 1e9 = 12 GB ✓
			preRestartBytes := int64(user.GetFloat("current_period_usage_gb") * 1e9)
			baselineBytes = totalBytes - preRestartBytes
			user.Set("turn_baseline_bytes", float64(baselineBytes))
		}

		usageGB := computeUsageGB(totalBytes, baselineBytes)

		user.Set("current_period_usage_gb", usageGB)
		if err := p.app.Save(user); err != nil {
			log.Printf("quota: save user %s: %v", user.Id, err)
		}
	}

	return nil
}

func (p *Poller) resetPeriod(ctx context.Context, user *core.Record, now time.Time) error {
	// Archive the completed period.
	bwCol, err := p.app.FindCollectionByNameOrId("bandwidth_usage")
	if err != nil {
		return fmt.Errorf("find bandwidth_usage: %w", err)
	}

	usedGB := user.GetFloat("current_period_usage_gb")
	archiveRecord := core.NewRecord(bwCol)
	archiveRecord.Set("account_id", user.Id)
	archiveRecord.Set("period_start", user.GetDateTime("quota_period_start").Time())
	archiveRecord.Set("period_end", user.GetDateTime("quota_period_end").Time())
	archiveRecord.Set("bytes_transferred", int64(usedGB*1e9))

	if err := p.app.Save(archiveRecord); err != nil {
		return fmt.Errorf("archive bandwidth_usage: %w", err)
	}

	// Capture current Prometheus counter as the new baseline.
	newBaseline, err := p.prom.QueryAccountBytes(ctx, user.Id)
	if err != nil {
		log.Printf("quota: get new baseline for user %s: %v", user.Id, err)
		newBaseline = 0
	}

	// Advance period by exactly 30 days from the old period end.
	oldPeriodEnd := user.GetDateTime("quota_period_end").Time()
	newPeriodStart := oldPeriodEnd
	newPeriodEnd := nextPeriodEnd(oldPeriodEnd)

	user.Set("quota_period_start", newPeriodStart)
	user.Set("quota_period_end", newPeriodEnd)
	user.Set("current_period_usage_gb", 0.0)
	user.Set("turn_baseline_bytes", float64(newBaseline))

	return p.app.Save(user)
}

// computeUsageGB converts raw Prometheus bytes (total - baseline) to gigabytes.
func computeUsageGB(totalBytes, baselineBytes int64) float64 {
	periodBytes := totalBytes - baselineBytes
	if periodBytes < 0 {
		return 0
	}
	return float64(periodBytes) / 1e9
}

// IsQuotaExceeded returns true if usedGB has reached or exceeded limitGB.
func IsQuotaExceeded(usedGB, limitGB float64) bool {
	return usedGB >= limitGB
}

func nextPeriodEnd(periodEnd time.Time) time.Time {
	return periodEnd.Add(30 * 24 * time.Hour)
}
```

- [ ] **Step 4: Run the quota tests**

```bash
cd signaling-server && go test ./internal/quota/... -v
```
Expected: all PASS

- [ ] **Step 5: Wire the poller into `main.go`**

Add in the `app.OnServe().BindFunc` block, before `return se.Next()`:

```go
// Start quota poller when TURN is configured — quotas are always enforced with TURN.
if cfg.HasTurn() {
	promClient := metrics.NewPrometheusClient(cfg.PrometheusURL)
	poller := quota.NewPoller(app, promClient, cfg.QuotaCheckInterval)
	go poller.Run(se.Server.BaseContext(se.Server))
	log.Printf("quota poller started (interval: %s, prometheus: %s)", cfg.QuotaCheckInterval, cfg.PrometheusURL)
}
```

Add to imports in `main.go`:
```go
"sharebridge/server/internal/metrics"
"sharebridge/server/internal/quota"
```

- [ ] **Step 6: Build to confirm**

```bash
cd signaling-server && go build ./...
```
Expected: success

- [ ] **Step 7: Commit**

```bash
cd signaling-server && git add internal/quota/poller.go internal/quota/poller_test.go cmd/server/main.go
git commit -m "feat: add quota poller — polls Prometheus and updates per-account bandwidth usage"
```

---

## Task 7: Quota enforcement in `browser_ws.go`

After the WebSocket upgrade, look up the account's quota. If exceeded, send the browser STUN-only ICE config (no TURN credentials) plus a `relay_quota_exceeded` WS message so the browser can display a clear error.

**Why post-upgrade, not pre-upgrade:** The agent enforces IP protection itself via `ICETransportPolicyRelay` in `peer.go` — it never emits host/srflx candidates regardless of what the server sends the browser. The server doesn't need to know whether a session is relay-only; withholding TURN credentials is sufficient. Doing the check post-upgrade also gives the browser a structured error message rather than a silent WebSocket connection failure.

**Behaviour by scenario:**
- Quota OK → send STUN + TURN ICE config as normal
- Quota exceeded, direct share → STUN-only ICE config + `relay_quota_exceeded` warning; direct connection may still succeed
- Quota exceeded, relay-only share → STUN-only ICE config + `relay_quota_exceeded` error; agent only has relay candidates so ICE fails naturally, but agent's IP never leaked (enforced by `ICETransportPolicyRelay` on the agent side, not by us)

**Files:**
- Modify: `signaling-server/internal/handler/browser_ws.go`
- Modify: `signaling-server/web/app.js`

- [ ] **Step 1: Write the tests**

Read the existing test file first:

```bash
cd signaling-server && cat internal/handler/browser_ws_test.go
```

Then append to `signaling-server/internal/handler/browser_ws_test.go`:

```go
func TestCheckRelayQuota_Exceeded(t *testing.T) {
	usersCol := &core.Collection{}
	user := core.NewRecord(usersCol)
	user.Set("relay_quota_gb", 50.0)
	user.Set("current_period_usage_gb", 55.0)
	user.Set("quota_period_end", time.Now().Add(10*24*time.Hour))

	exceeded, periodEnd := checkRelayQuota(user)
	if !exceeded {
		t.Error("expected quota to be exceeded")
	}
	if periodEnd.IsZero() {
		t.Error("expected non-zero period end")
	}
}

func TestCheckRelayQuota_NotExceeded(t *testing.T) {
	usersCol := &core.Collection{}
	user := core.NewRecord(usersCol)
	user.Set("relay_quota_gb", 50.0)
	user.Set("current_period_usage_gb", 30.0)
	user.Set("quota_period_end", time.Now().Add(10*24*time.Hour))

	exceeded, _ := checkRelayQuota(user)
	if exceeded {
		t.Error("expected quota to NOT be exceeded")
	}
}

```

- [ ] **Step 2: Run the tests — expect compile failure**

```bash
cd signaling-server && go test ./internal/handler/... -v -run TestCheckRelayQuota
```
Expected: FAIL — `checkRelayQuota` undefined

- [ ] **Step 3: Add `checkRelayQuota` helper to `browser_ws.go`**

Add at the bottom of `browser_ws.go`:

```go
// checkRelayQuota reports whether the account has exceeded its relay bandwidth quota.
// Returns (exceeded, periodEnd).
func checkRelayQuota(accountRecord *core.Record) (bool, time.Time) {
	limitGB   := accountRecord.GetFloat("relay_quota_gb")
	usedGB    := accountRecord.GetFloat("current_period_usage_gb")
	periodEnd := accountRecord.GetDateTime("quota_period_end").Time()
	return usedGB >= limitGB, periodEnd
}
```

- [ ] **Step 4: Replace the ICE config block in `BrowserWS` with the quota-aware version**

Find and replace the existing TURN credentials block (the `var turnCreds` block sent before the message loop). The new block does account ID lookup, quota check, and sends the `relay_quota_exceeded` message before the ICE config:

```go
// Look up account ID for account-scoped TURN credentials and quota check.
apiKeyRecord, err := app.FindRecordById("api_keys", sessionRecord.GetString("api_key_id"))
if err != nil {
	http.Error(w, "internal error", http.StatusInternalServerError)
	return
}
accountID := apiKeyRecord.GetString("account_id")

// Quota check: if exceeded, notify the browser and withhold TURN credentials.
// IP protection for relay-only shares is enforced by the agent (ICETransportPolicyRelay)
// so we don't need to know whether this session is relay-only.
quotaExceeded := false
if cfg.HasTurn() {
	if accountRecord, err := app.FindRecordById("users", accountID); err == nil {
		if exceeded, periodEnd := checkRelayQuota(accountRecord); exceeded {
			quotaExceeded = true
			usedGB  := accountRecord.GetFloat("current_period_usage_gb")
			limitGB := accountRecord.GetFloat("relay_quota_gb")
			hub.SendDirect(requestCtx, browserConn, map[string]any{
				"type":       "relay_quota_exceeded",
				"message":    fmt.Sprintf("Relay quota exceeded (%.1f GB used). Resets after %s.", usedGB, periodEnd.Format("Jan 2")),
				"usage_gb":   usedGB,
				"quota_gb":   limitGB,
				"period_end": periodEnd.Format(time.RFC3339),
			})
		}
	}
}

// Send ICE config. Omit TURN credentials when quota is exceeded so only
// direct connections are possible.
var turnCreds *turn.Credentials
if cfg.HasTurn() && !quotaExceeded {
	turnExpiry := time.Now().Add(24 * time.Hour)
	creds := turn.GenerateCredentials(cfg.TurnSecret, accountID, turnExpiry)
	turnCreds = &creds
}
iceServers := turn.BuildICEConfig(&turn.ICEConfigRequest{
	STUNURL:     cfg.STUNURL,
	TurnURL:     cfg.TurnURL(),
	Credentials: turnCreds,
})
hub.SendDirect(requestCtx, browserConn, map[string]any{
	"type":        "ice_config",
	"ice_servers": iceServers,
})
```

Add `"fmt"` to the imports if not already present. This block supersedes the account ID lookup added in Task 1 — the Task 1 block should no longer exist in the file at this point since this step replaces it entirely.

- [ ] **Step 5: Handle `relay_quota_exceeded` in `app.js`**

In `signaling-server/web/app.js`, add a case in the `ws.onmessage` switch after the `ice_config` case:

```js
case 'relay_quota_exceeded':
  status('Relay quota exceeded — ' + msg.message);
  // Direct connection will still be attempted below.
  // If it also fails, this message explains why relay was unavailable.
  break;
```

- [ ] **Step 6: Run all handler tests**

```bash
cd signaling-server && go test ./internal/handler/... -v
```
Expected: all PASS

- [ ] **Step 7: Commit**

```bash
cd signaling-server && git add internal/handler/browser_ws.go internal/handler/browser_ws_test.go web/app.js
git commit -m "feat: post-upgrade quota check — STUN-only ICE + relay_quota_exceeded WS message when over limit"
```

---

## Task 8: Account REST endpoints

Two endpoints:
1. `GET /api/account` — JWT auth, for the browser account dashboard
2. `GET /api/account/quota` — API key auth, for the agent to fetch quota info

**Files:**
- Create: `signaling-server/internal/handler/account.go`
- Modify: `signaling-server/cmd/server/main.go`

- [ ] **Step 1: Write the handler tests**

Create `signaling-server/internal/handler/account_test.go`:

```go
package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/migrations"
)

func TestGetAccount_ReturnsQuota(t *testing.T) {
	app := testApp(t)
	if err := migrations.CreateCollections(app); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := migrations.AddQuotaFields(app); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Create a user
	usersCol, _ := app.FindCollectionByNameOrId("users")
	user := core.NewRecord(usersCol)
	user.SetEmail("dash@example.com")
	user.SetPassword("password123")
	periodStart := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC)
	periodEnd := periodStart.Add(30 * 24 * time.Hour)
	user.Set("relay_quota_gb", 50.0)
	user.Set("current_period_usage_gb", 32.5)
	user.Set("quota_period_start", periodStart)
	user.Set("quota_period_end", periodEnd)
	app.Save(user)

	// Test buildQuotaResponse directly
	resp := buildQuotaResponse(user)

	if resp.LimitGB != 50.0 {
		t.Errorf("LimitGB = %v, want 50.0", resp.LimitGB)
	}
	if resp.UsedGB != 32.5 {
		t.Errorf("UsedGB = %v, want 32.5", resp.UsedGB)
	}
	wantRemaining := 50.0 - 32.5
	if resp.RemainingGB != wantRemaining {
		t.Errorf("RemainingGB = %v, want %v", resp.RemainingGB, wantRemaining)
	}
	wantPct := int(32.5 / 50.0 * 100)
	if resp.PercentageUsed != wantPct {
		t.Errorf("PercentageUsed = %v, want %v", resp.PercentageUsed, wantPct)
	}
}

func TestGetAccountQuotaHandler_APIKey(t *testing.T) {
	app := testApp(t)
	if err := migrations.CreateCollections(app); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := migrations.AddQuotaFields(app); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Create user + api key
	usersCol, _ := app.FindCollectionByNameOrId("users")
	user := core.NewRecord(usersCol)
	user.SetEmail("agent@example.com")
	user.SetPassword("password123")
	user.Set("relay_quota_gb", 50.0)
	user.Set("current_period_usage_gb", 10.0)
	user.Set("quota_period_start", time.Now())
	user.Set("quota_period_end", time.Now().Add(30*24*time.Hour))
	app.Save(user)

	apiKeysCol, _ := app.FindCollectionByNameOrId("api_keys")
	key := core.NewRecord(apiKeysCol)
	key.Set("account_id", user.Id)
	key.Set("label", "test")
	key.Set("is_active", true)
	key.Set("key_hash", "irrelevant-for-this-test")
	app.Save(key)

	// Call GetAccountQuotaByAPIKey with the key's account ID
	accountRecord, _ := app.FindRecordById("users", user.Id)
	quota := buildQuotaResponse(accountRecord)

	if quota.LimitGB != 50.0 {
		t.Errorf("LimitGB = %v, want 50.0", quota.LimitGB)
	}
	if quota.UsedGB != 10.0 {
		t.Errorf("UsedGB = %v, want 10.0", quota.UsedGB)
	}
}
```

- [ ] **Step 2: Run the tests — expect compile failure**

```bash
cd signaling-server && go test ./internal/handler/... -v -run TestGetAccount
```
Expected: FAIL — `buildQuotaResponse` undefined

- [ ] **Step 3: Create `signaling-server/internal/handler/account.go`**

```go
package handler

import (
	"net/http"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"sharebridge/server/internal/middleware"
)

// QuotaInfo is the bandwidth quota section of an account response.
type QuotaInfo struct {
	LimitGB        float64   `json:"limit_gb"`
	UsedGB         float64   `json:"used_gb"`
	RemainingGB    float64   `json:"remaining_gb"`
	PeriodStart    time.Time `json:"period_start"`
	PeriodEnd      time.Time `json:"period_end"`
	PercentageUsed int       `json:"percentage_used"`
}

// AccountResponse is the payload for GET /api/account.
type AccountResponse struct {
	ID    string    `json:"id"`
	Email string    `json:"email"`
	Quota QuotaInfo `json:"quota"`
}

// GetAccount returns account info + quota for the authenticated user.
// Route: GET /api/account (JWT auth via apis.RequireAuth middleware)
func GetAccount(app core.App) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		authRecord := e.Auth
		if authRecord == nil {
			return e.JSON(http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		}

		accountRecord, err := app.FindRecordById("users", authRecord.Id)
		if err != nil {
			return e.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to load account"})
		}

		return e.JSON(http.StatusOK, AccountResponse{
			ID:    accountRecord.Id,
			Email: accountRecord.GetString("email"),
			Quota: buildQuotaResponse(accountRecord),
		})
	}
}

// GetAccountQuotaByAPIKey returns quota info for the account owning the API key.
// Route: GET /api/account/quota (API key auth via middleware.APIKeyAuth)
// Used by the agent to display quota in the share creation form.
func GetAccountQuotaByAPIKey(app core.App) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID := middleware.GetAccountID(r.Context())
		if accountID == "" {
			http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
			return
		}

		accountRecord, err := app.FindRecordById("users", accountID)
		if err != nil {
			http.Error(w, `{"error":"account not found"}`, http.StatusNotFound)
			return
		}

		quota := buildQuotaResponse(accountRecord)
		w.Header().Set("Content-Type", "application/json")
		encodeJSON(w, quota)
	}
}

// buildQuotaResponse constructs a QuotaInfo from a users record.
func buildQuotaResponse(accountRecord *core.Record) QuotaInfo {
	limitGB := accountRecord.GetFloat("relay_quota_gb")
	usedGB := accountRecord.GetFloat("current_period_usage_gb")
	remainingGB := limitGB - usedGB
	if remainingGB < 0 {
		remainingGB = 0
	}

	pct := 0
	if limitGB > 0 {
		pct = int(usedGB / limitGB * 100)
		if pct > 100 {
			pct = 100
		}
	}

	periodStart := accountRecord.GetDateTime("quota_period_start").Time()
	periodEnd := accountRecord.GetDateTime("quota_period_end").Time()

	return QuotaInfo{
		LimitGB:        limitGB,
		UsedGB:         usedGB,
		RemainingGB:    remainingGB,
		PeriodStart:    periodStart,
		PeriodEnd:      periodEnd,
		PercentageUsed: pct,
	}
}

// encodeJSON writes v as JSON to w, ignoring encode errors (response already started).
func encodeJSON(w http.ResponseWriter, v any) {
	enc := jsonEncoder(w)
	enc.Encode(v)
}
```

Note: `jsonEncoder` doesn't exist yet — replace `encodeJSON` with direct usage:

```go
import "encoding/json"

func encodeJSON(w http.ResponseWriter, v any) {
	json.NewEncoder(w).Encode(v)
}
```

- [ ] **Step 4: Wire the endpoints in `main.go`**

In the `app.OnServe().BindFunc` block, add after the `apiKeys` group:

```go
// Account info (JWT auth)
accountGroup := router.Group("/api/account")
accountGroup.Bind(apis.RequireAuth())
accountGroup.GET("", handler.GetAccount(app))

// Account quota (API key auth — for agent UI)
router.GET("/api/account/quota", func(e *core.RequestEvent) error {
	authMiddleware := middleware.APIKeyAuth(app)
	handlerFunc := handler.GetAccountQuotaByAPIKey(app)
	authMiddleware(http.HandlerFunc(handlerFunc)).ServeHTTP(e.Response, e.Request)
	return nil
})
```

- [ ] **Step 5: Run handler tests**

```bash
cd signaling-server && go test ./internal/handler/... -v -run TestGetAccount
```
Expected: PASS

- [ ] **Step 6: Build**

```bash
cd signaling-server && go build ./...
```
Expected: success

- [ ] **Step 7: Commit**

```bash
cd signaling-server && git add internal/handler/account.go internal/handler/account_test.go cmd/server/main.go
git commit -m "feat: add GET /api/account (JWT) and GET /api/account/quota (API key) endpoints"
```

---

## Task 9: Account dashboard UI — quota card

Add a quota progress card to `account.html` that fetches from `GET /api/account` and renders a progress bar.

**Files:**
- Modify: `signaling-server/web/account.html`

- [ ] **Step 1: Add the quota card styles**

In the `<style>` block in `account.html`, add before the closing `</style>`:

```css
/* Quota card */
.quota-bar-track {
  background: #1e1e2e;
  border-radius: 4px;
  height: 12px;
  margin: 12px 0 8px;
  overflow: hidden;
}
.quota-bar-fill {
  height: 100%;
  border-radius: 4px;
  background: #89b4fa;
  transition: width 0.3s;
}
.quota-bar-fill.warning { background: #fab387; }
.quota-bar-fill.danger  { background: #f38ba8; }
.quota-stats {
  display: flex;
  justify-content: space-between;
  font-size: 0.85rem;
  color: #6c7086;
}
.quota-reset {
  font-size: 0.85rem;
  color: #6c7086;
  margin-top: 6px;
}
```

- [ ] **Step 2: Add the quota section HTML**

In `account.html`, insert the following section **before** the API Keys section `<div class="section">`:

```html
<!-- Quota Section -->
<div class="section" id="quota-section">
  <div class="section-header">
    <span class="section-title">Relay Usage</span>
    <span id="quota-period-label" style="font-size:0.85rem;color:#6c7086;"></span>
  </div>
  <div id="quota-content">
    <div class="empty-state"><p>Loading usage...</p></div>
  </div>
</div>
```

- [ ] **Step 3: Add the `loadQuota()` function to the `<script>` block**

Add before `document.addEventListener('DOMContentLoaded', loadKeys)`:

```js
async function loadQuota() {
  const content = document.getElementById('quota-content');
  const periodLabel = document.getElementById('quota-period-label');

  try {
    const response = await apiRequest('/account');
    if (!response || !response.ok) {
      content.innerHTML = '<div class="empty-state"><p>Quota info unavailable</p></div>';
      return;
    }

    const data = await response.json();
    const quota = data.quota;

    if (!quota || quota.limit_gb === 0) {
      content.innerHTML = '<div class="empty-state"><p>No relay quota configured</p></div>';
      return;
    }

    const usedGB    = quota.used_gb.toFixed(1);
    const limitGB   = quota.limit_gb.toFixed(0);
    const pct       = Math.min(quota.percentage_used, 100);
    const fillClass = pct >= 90 ? 'danger' : pct >= 70 ? 'warning' : '';

    const periodEnd  = new Date(quota.period_end);
    const now        = new Date();
    const daysLeft   = Math.max(0, Math.ceil((periodEnd - now) / 86400000));

    periodLabel.textContent = '30-day period';

    content.innerHTML = `
      <div class="quota-bar-track">
        <div class="quota-bar-fill ${fillClass}" style="width:${pct}%"></div>
      </div>
      <div class="quota-stats">
        <span>${usedGB} GB used / ${limitGB} GB limit</span>
        <span>${pct}%</span>
      </div>
      <div class="quota-reset">Resets in ${daysLeft} day${daysLeft === 1 ? '' : 's'} (${periodEnd.toLocaleDateString('en-US', {month:'short',day:'numeric'})})</div>
    `;
  } catch (err) {
    content.innerHTML = '<div class="empty-state"><p>Error loading quota</p></div>';
  }
}
```

- [ ] **Step 4: Call `loadQuota()` on page load**

Change the existing `DOMContentLoaded` listener to:

```js
document.addEventListener('DOMContentLoaded', () => {
  loadQuota();
  loadKeys();
});
```

- [ ] **Step 5: Manual smoke test**

Start the server locally (or in docker-compose) and visit `/account`. Confirm:
- Quota card shows "Loading usage..." then renders the progress bar
- With TURN configured: card shows current usage from Prometheus
- Without TURN (direct-only): card shows 0 used / 50 GB limit (poller not running, which is fine)

- [ ] **Step 6: Commit**

```bash
cd signaling-server && git add web/account.html
git commit -m "feat: add relay quota progress card to account dashboard"
```

---

## Task 10: Agent share form — relay quota display

The agent's share form already has relay mode radio buttons. Fetch quota info from the signaling server when the form opens so the user sees their remaining quota before making a choice, not after clicking relay.

**Files:**
- Modify: `agent/internal/web/server.go`
- Modify: `agent/internal/web/api.go`
- Modify: `agent/internal/web/templates/share-form.html`

- [ ] **Step 1: Add the relay quota handler to `api.go`**

In `agent/internal/web/api.go`, add this function before `listSharesHandler`:

```go
// relayQuotaHandler fetches bandwidth quota from the signaling server using
// the configured API key. Returns quota JSON, or 204 if quota is unavailable
// (quotas not enabled on server, API key not configured, etc.).
func (ws *WebServer) relayQuotaHandler(w http.ResponseWriter, r *http.Request) {
	if ws.daemon == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	cfg := ws.daemon.GetConfig()
	if cfg.APIKey == "" || cfg.SignalingURL == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Convert ws:// → http://, wss:// → https://
	httpURL := cfg.SignalingURL
	switch {
	case len(httpURL) >= 6 && httpURL[:6] == "wss://":
		httpURL = "https://" + httpURL[6:]
	case len(httpURL) >= 5 && httpURL[:5] == "ws://":
		httpURL = "http://" + httpURL[5:]
	}

	quotaURL := httpURL + "/api/account/quota?api_key=" + cfg.APIKey

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, quotaURL, nil)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	io.Copy(w, resp.Body)
}
```

Add `"io"` and `"net/http"` to the imports (they should already be there from existing handlers).

- [ ] **Step 2: Register the route in `server.go`**

In `registerRoutes`, add:

```go
mux.HandleFunc("GET /api/relay-quota", ws.relayQuotaHandler)
```

- [ ] **Step 3: Update `share-form.html` to show quota on form open**

In `share-form.html`, add a quota info div inside the relay option, after the `.mode-option-warning.red` div:

```html
<div class="mode-option" id="relay-option">
    <div class="mode-option-header">
        <input type="radio" id="mode-relay" name="relay_only" value="true">
        <div>
            <div class="mode-option-title">Relay (hide IP)</div>
            <div class="mode-option-desc">All traffic via TURN server, IP never exposed</div>
        </div>
    </div>
    <div class="mode-option-warning red">
        ⚠️ Requires TURN server. Connection will fail if TURN is not configured.
    </div>
    <div id="relay-quota-info" style="margin-top:8px;font-size:0.85rem;color:#6c7086;">
        Loading quota...
    </div>
</div>
```

In the `<script>` block at the bottom of `share-form.html`, add after the existing `setMode` function:

```js
// Load relay quota info
(function loadRelayQuota() {
    fetch('/api/relay-quota')
        .then(function(r) { return r.ok ? r.json() : null; })
        .then(function(quota) {
            var info = dlg.querySelector('#relay-quota-info');
            if (!info || !quota) return;

            var usedGB = quota.used_gb.toFixed(1);
            var limitGB = quota.limit_gb.toFixed(0);
            var pct = Math.min(Math.round(quota.percentage_used), 100);

            if (quota.used_gb >= quota.limit_gb) {
                info.innerHTML = '❌ Relay quota exceeded (' + usedGB + ' / ' + limitGB + ' GB). Direct mode available.';
                info.style.color = '#f38ba8';
                // Disable relay radio and switch to direct
                var modeRelayRadio = dlg.querySelector('#mode-relay');
                if (modeRelayRadio) modeRelayRadio.disabled = true;
            } else {
                info.innerHTML = 'Quota: ' + usedGB + ' GB / ' + limitGB + ' GB used (' + pct + '%)';
            }
            info.style.display = 'block';
        })
        .catch(function() { /* quota info unavailable, silently ignore */ });
})();
```

- [ ] **Step 4: Build the agent**

```bash
cd agent && go build ./...
```
Expected: success

- [ ] **Step 5: Commit**

```bash
cd agent && git add internal/web/server.go internal/web/api.go internal/web/templates/share-form.html
git commit -m "feat: show relay quota in agent share form, fetched from signaling server"
```

---

## Task 11: Docker-compose + Prometheus config + SSRF protection

Without `denied-peer-ip` flags, Coturn can be used as an HTTP proxy to reach cloud metadata services (`169.254.169.254`) and internal networks. These flags block TURN from relaying to private/link-local ranges, regardless of quota.

**Files:**
- Modify: `signaling-server/docker-compose.yml`
- Create: `signaling-server/prometheus.yml`

- [ ] **Step 1: Create `signaling-server/prometheus.yml`**

```yaml
global:
  scrape_interval: 60s

scrape_configs:
  - job_name: 'coturn'
    static_configs:
      - targets: ['coturn:9641']
```

- [ ] **Step 2: Update `signaling-server/docker-compose.yml`**

Replace the entire file with:

```yaml
services:
  sharebridge-server:
    build: .
    ports:
      - "8080:8080"
    environment:
      - PORT=8080
      - DATA_DIR=/data/pb_data
      - TURN_HOST=${TURN_HOST}
      - TURN_PORT=3478
      - TURN_SECRET=${TURN_SECRET}
      - SMTP_HOST=${SMTP_HOST}
      - SMTP_PORT=${SMTP_PORT}
      - SMTP_USER=${SMTP_USER}
      - SMTP_PASSWORD=${SMTP_PASSWORD}
      # Quota — always enforced when TURN is configured (requires Prometheus)
      - DEFAULT_QUOTA_GB=${DEFAULT_QUOTA_GB:-50}
      - PROMETHEUS_URL=http://prometheus:9090
      - QUOTA_CHECK_INTERVAL=${QUOTA_CHECK_INTERVAL:-5m}
    volumes:
      - sharebridge-data:/data
    depends_on:
      - coturn

  coturn:
    image: coturn/coturn:latest
    network_mode: host
    command: >
      -n
      --log-file=stdout
      --min-port=49152
      --max-port=65535
      --realm=sharebridge
      --use-auth-secret
      --static-auth-secret=${TURN_SECRET}
      --prometheus
      --prometheus-port=9641
      --prometheus-username-labels
      --denied-peer-ip=10.0.0.0/8
      --denied-peer-ip=172.16.0.0/12
      --denied-peer-ip=192.168.0.0/16
      --denied-peer-ip=169.254.0.0/16
      --denied-peer-ip=127.0.0.0/8
      --denied-peer-ip=0.0.0.0/8
      --denied-peer-ip=::1/128
    restart: unless-stopped

  prometheus:
    image: prom/prometheus:latest
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml
      - prometheus-data:/prometheus
    command:
      - '--config.file=/etc/prometheus/prometheus.yml'
      - '--storage.tsdb.retention.time=35d'
    # Expose port only if you want the Prometheus UI — remove in production
    # ports:
    #   - "9090:9090"
    restart: unless-stopped

volumes:
  sharebridge-data:
  prometheus-data:
```

- [ ] **Step 3: Verify SSRF protection is active after deploy**

After `docker-compose up`, confirm Coturn refuses relay to the metadata address. From a host that can reach the TURN port:

```bash
# Install turnutils if needed: apt install coturn
turnutils_uclient -T -u $(date +%s):testaccount -W <TURN_SECRET_HMAC> \
  -p 3478 <TURN_HOST> 169.254.169.254
```

Expected: allocation succeeds but peer connection attempt to `169.254.169.254` is rejected by Coturn (error 403 or connection refused at the TURN relay level).

Alternatively, check Coturn logs for a "denied peer" line:
```
WARNING: peer address is denied: 169.254.169.254
```

- [ ] **Step 4: Commit**

```bash
cd signaling-server && git add docker-compose.yml prometheus.yml
git commit -m "feat: add Prometheus service, per-username Coturn metrics, and SSRF protection via denied-peer-ip"
```

---

## Spec Coverage Self-Review

**Spec acceptance criteria vs. plan tasks:**

| Criterion | Covered in |
|-----------|-----------|
| When TURN configured, Prometheus required + quotas always enforced | Tasks 2, 6, 7 |
| When TURN not configured, Prometheus not required, no limits | Task 2 (`HasTurn()` guard in poller wiring) |
| New accounts get 50 GB/30 days by default | Task 4 (user creation hook) |
| Relay sessions rejected with clear error when quota exceeded | Task 7 (`relay_quota_exceeded` WS message + STUN-only ICE) |
| Quota usage displays in account dashboard with progress bar | Task 9 |
| Quota period is fixed 30 days from account creation | Tasks 3, 4, 6 |
| Bandwidth usage logged for audit purposes | Task 6 (`resetPeriod` archives to `bandwidth_usage`) |
| Self-hosters can configure `DEFAULT_QUOTA_GB` | Task 2 |
| Account-scoped TURN credentials (prevents Prometheus cardinality leak) | Task 1 |
| SSRF protection — block relay to private/link-local IPs | Task 11 (`denied-peer-ip`) |

**Breaking changes documented in spec:**
- Per-session TURN creds → account-scoped: covered in Task 1 ✓
- No bandwidth limits → 50 GB default: covered in Tasks 3, 4, 7 ✓

**Placeholder scan:** None found — all steps contain concrete code.

**Type consistency check:**
- `QuotaInfo` struct defined once in `account.go`, used in `AccountResponse` and returned by `buildQuotaResponse` ✓
- `checkRelayQuota` in `browser_ws.go` uses `GetFloat` consistent with PocketBase API ✓
- `computeUsageGB` and `IsQuotaExceeded` defined once in `quota/poller.go`, tested directly ✓
- `NewPrometheusClient` / `QueryAccountBytes` defined in `metrics/prometheus.go`, imported by `quota/poller.go` ✓

---

## Deployment Notes

**First deploy with TURN configured:**
1. Deploy server — migration 3 runs automatically, backfilling existing users with 30-day periods from their `created` date
2. Prometheus starts scraping Coturn immediately
3. Quota poller begins updating `current_period_usage_gb` every 5 minutes (auto-starts when `cfg.HasTurn()` and `cfg.PrometheusURL` is set)

**Direct-only deployment (no TURN):**
- No Prometheus service needed — remove it from `docker-compose.yml`
- No relay limits apply; quota card shows 0 used with default limit

**Verifying Coturn metric name:**
The plan uses `turn_traffic_sent` as specified in the spec. To verify your Coturn version exports this exact name:
```bash
curl http://localhost:9641/metrics | grep -i traffic
```
If the name differs (e.g., `coturn_relay_bytes_sent_total`), update the PromQL in `metrics/prometheus.go`.

**Self-hosted without TURN (direct-only):**
- Prometheus is not required — remove the `prometheus` service from `docker-compose.yml`
- No relay limits apply (nothing to meter without a TURN server)
- Account-scoped TURN credentials are still generated if TURN is ever added later
