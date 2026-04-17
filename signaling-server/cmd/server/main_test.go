package main

import (
	"net/http"
	"net/http/httptest"
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
