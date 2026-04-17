package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	pbrouter "github.com/pocketbase/pocketbase/tools/router"
)

func TestRegisterStaticRoutes_ServesBundleAsJavaScript(t *testing.T) {
	webRoot := filepath.Join("..", "..", "web")
	r := pbrouter.NewRouter(func(w http.ResponseWriter, req *http.Request) (*core.RequestEvent, pbrouter.EventCleanupFunc) {
		return &core.RequestEvent{
			Event: pbrouter.Event{
				Response: w,
				Request:  req,
			},
		}, nil
	})

	registerStaticRoutes(r, webRoot)

	mux, err := r.BuildMux()
	if err != nil {
		t.Fatalf("build mux: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/app.bundle.js", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("content-type = %q", rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), "window.join") {
		t.Fatalf("bundle body missing expected script contents")
	}
}

func TestEffectiveHTTPArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		port string
		want []string
	}{
		{
			name: "appends default http flag when absent",
			args: []string{"server", "serve"},
			port: "8080",
			want: []string{"server", "serve", "--http=0.0.0.0:8080"},
		},
		{
			name: "keeps explicit http flag",
			args: []string{"server", "serve", "--http=127.0.0.1:8080"},
			port: "8080",
			want: []string{"server", "serve", "--http=127.0.0.1:8080"},
		},
		{
			name: "keeps split explicit http flag",
			args: []string{"server", "serve", "--http", "127.0.0.1:8080"},
			port: "8080",
			want: []string{"server", "serve", "--http", "127.0.0.1:8080"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := effectiveHTTPArgs(tt.args, tt.port)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("effectiveHTTPArgs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
