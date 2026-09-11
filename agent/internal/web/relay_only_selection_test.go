package web

// M4 closeout batch 4: the per-share relay-only selection posted by the share
// form / JSON API must reach the daemon's CreateSession relay-only argument,
// with explicit selection winning over the persisted DefaultRelayOnly default
// and an absent field falling back to it. These tests are the web half of the
// wired chain; the daemon half (binding/registration/persistence) lives in
// agent/internal/daemon/relay_only_pershare_test.go.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"sharebridge/agent/internal/config"
)

func immichWebConfig(defaultRelayOnly bool) *config.Config {
	return &config.Config{
		AgentAPIKey:       "key",
		SignalingURL:      "wss://share.example.com",
		DefaultExpiry:     24,
		DefaultRelayOnly:  defaultRelayOnly,
		ImmichURL:         "http://immich.lan:2283",
		ImmichAllowedHost: "immich.lan:2283",
		ImmichAPIKey:      "api",
	}
}

func postShareForm(t *testing.T, ws *WebServer, values url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/shares", strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	ws.createShareHandler(rec, req)
	return rec
}

// TestCreateShareForm_PerShareRelayOnlyReachesDaemon pins the form half of the
// fix: the visible relay radio posts relay_only=true and the handler forwards
// that per-share decision to CreateSession.
func TestCreateShareForm_PerShareRelayOnlyReachesDaemon(t *testing.T) {
	ws, mock := newV1TestServer(immichWebConfig(false))

	rec := postShareForm(t, ws, url.Values{
		"share_url":  {"immich://IMMICHFORMRELAY"},
		"share_type": {"immich"},
		"relay_only": {"true"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	session := mock.GetSession("code-1")
	if session == nil {
		t.Fatal("session not created")
	}
	if !session.RelayOnly {
		t.Error("per-share relay_only=true must reach CreateSession (got a direct share)")
	}
}

// TestCreateShareForm_PerShareDirectWinsOverGlobalRelayDefault pins the
// explicit-wins arm for the form: relay_only=false overrides DefaultRelayOnly.
func TestCreateShareForm_PerShareDirectWinsOverGlobalRelayDefault(t *testing.T) {
	ws, mock := newV1TestServer(immichWebConfig(true))

	rec := postShareForm(t, ws, url.Values{
		"share_url":  {"immich://IMMICHFORMDIRECT"},
		"share_type": {"immich"},
		"relay_only": {"false"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	session := mock.GetSession("code-1")
	if session == nil {
		t.Fatal("session not created")
	}
	if session.RelayOnly {
		t.Error("an explicit form relay_only=false must win over DefaultRelayOnly=true")
	}
}

// TestCreateShareForm_AbsentRelayOnlyFallsBackToDefault pins the unchanged
// behavior for a share created without the field: it follows DefaultRelayOnly.
func TestCreateShareForm_AbsentRelayOnlyFallsBackToDefault(t *testing.T) {
	for _, tc := range []struct {
		name             string
		defaultRelayOnly bool
	}{
		{"default relay-only", true},
		{"default direct", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws, mock := newV1TestServer(immichWebConfig(tc.defaultRelayOnly))

			rec := postShareForm(t, ws, url.Values{
				"share_url":  {"immich://IMMICHFORMABSENT"},
				"share_type": {"immich"},
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			session := mock.GetSession("code-1")
			if session == nil {
				t.Fatal("session not created")
			}
			if session.RelayOnly != tc.defaultRelayOnly {
				t.Errorf("absent relay_only: RelayOnly = %t, want the DefaultRelayOnly fallback %t",
					session.RelayOnly, tc.defaultRelayOnly)
			}
		})
	}
}

// TestV1CreateShare_PerShareRelayOnlyReachesDaemon pins the JSON-API half: the
// extension/create surface used by the UI honors relay_only.
func TestV1CreateShare_PerShareRelayOnlyReachesDaemon(t *testing.T) {
	ws, mock := newV1TestServer(immichWebConfig(false))

	body := `{"share_url":"immich://IMMICHAPIRELAY","share_type":"immich","relay_only":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ws.v1CreateShareHandler(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	session := mock.GetSession("code-1")
	if session == nil {
		t.Fatal("session not created")
	}
	if !session.RelayOnly {
		t.Error("per-share relay_only=true must reach CreateSession via the JSON API")
	}
}

// TestV1CreateShare_PerShareDirectWinsOverGlobalRelayDefault pins the
// explicit-wins arm for the JSON API.
func TestV1CreateShare_PerShareDirectWinsOverGlobalRelayDefault(t *testing.T) {
	ws, mock := newV1TestServer(immichWebConfig(true))

	body := `{"share_url":"immich://IMMICHAPIDIRECT","share_type":"immich","relay_only":false}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ws.v1CreateShareHandler(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	session := mock.GetSession("code-1")
	if session == nil {
		t.Fatal("session not created")
	}
	if session.RelayOnly {
		t.Error("an explicit JSON relay_only=false must win over DefaultRelayOnly=true")
	}
}

// TestV1CreateShare_AbsentRelayOnlyFallsBackToDefault pins the unchanged
// behavior for a JSON request that omits the field.
func TestV1CreateShare_AbsentRelayOnlyFallsBackToDefault(t *testing.T) {
	ws, mock := newV1TestServer(immichWebConfig(true))

	body := `{"share_url":"immich://IMMICHAPIABSENT","share_type":"immich"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	ws.v1CreateShareHandler(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	session := mock.GetSession("code-1")
	if session == nil {
		t.Fatal("session not created")
	}
	if !session.RelayOnly {
		t.Error("an absent JSON relay_only must fall back to DefaultRelayOnly=true")
	}
}

// TestShareFormRelayOnlyFieldMatchesHandler is the form/handler drift guard:
// it renders the real share form, extracts the relay-mode radio group's field
// name from the HTML, and posts that exact extracted name back to the handler.
// If the template and the handler ever disagree about the field name (the
// class of bug this batch fixes), the handler reads nothing, falls back to the
// (here false) default, and this test fails.
func TestShareFormRelayOnlyFieldMatchesHandler(t *testing.T) {
	ws, mock := newV1TestServer(immichWebConfig(false))

	formReq := httptest.NewRequest(http.MethodGet, "/api/share-form", nil)
	formRec := httptest.NewRecorder()
	ws.shareFormHandler(formRec, formReq)
	if formRec.Code != http.StatusOK {
		t.Fatalf("share form status = %d, want 200: %s", formRec.Code, formRec.Body.String())
	}
	form := formRec.Body.String()

	fieldName := radioFieldName(t, form, "mode-relay")
	if fieldName == "" {
		t.Fatal("share form has no named relay-mode radio")
	}
	if got := radioValue(t, form, "mode-relay"); got != "true" {
		t.Errorf("relay radio value = %q, want %q", got, "true")
	}
	if got := radioFieldName(t, form, "mode-direct"); got != fieldName {
		t.Errorf("direct radio field = %q, relay radio field = %q; the mode radios must share one group", got, fieldName)
	}
	if got := radioValue(t, form, "mode-direct"); got != "false" {
		t.Errorf("direct radio value = %q, want %q", got, "false")
	}

	// Post the field name the TEMPLATE actually rendered. The handler must
	// parse the same name for the explicit selection to take effect.
	rec := postShareForm(t, ws, url.Values{
		"share_url":  {"immich://IMMICHFIELDGUARD"},
		"share_type": {"immich"},
		fieldName:    {"true"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	session := mock.GetSession("code-1")
	if session == nil {
		t.Fatal("session not created")
	}
	if !session.RelayOnly {
		t.Errorf("handler did not consume the rendered form field %q (template/handler drift)", fieldName)
	}
}

// radioFieldName extracts the name attribute of the radio input with the given
// id from a rendered form.
func radioFieldName(t *testing.T, body, id string) string {
	t.Helper()
	input := radioInput(t, body, id)
	m := regexp.MustCompile(`\bname="([^"]*)"`).FindStringSubmatch(input)
	if m == nil {
		return ""
	}
	return m[1]
}

// radioValue extracts the value attribute of the radio input with the given
// id from a rendered form.
func radioValue(t *testing.T, body, id string) string {
	t.Helper()
	input := radioInput(t, body, id)
	m := regexp.MustCompile(`\bvalue="([^"]*)"`).FindStringSubmatch(input)
	if m == nil {
		return ""
	}
	return m[1]
}

func radioInput(t *testing.T, body, id string) string {
	t.Helper()
	re := regexp.MustCompile(`<input\s+[^>]*id="` + regexp.QuoteMeta(id) + `"[^>]*>`)
	input := re.FindString(body)
	if input == "" {
		t.Fatalf("radio input id=%q not found in rendered form", id)
	}
	return input
}
