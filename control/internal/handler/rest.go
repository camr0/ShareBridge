package handler

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

// ServeFile returns a handler that serves a single static file.
func ServeFile(path string) func(*core.RequestEvent) error {
	return func(requestEvent *core.RequestEvent) error {
		http.ServeFile(requestEvent.Response, requestEvent.Request, path)
		return nil
	}
}

// ServeFileNoCache returns a handler that serves a single static file with
// cache disabled. Use this for HTML shells and live source assets so browsers
// pick up fresh deploys immediately.
func ServeFileNoCache(path string) func(*core.RequestEvent) error {
	return func(requestEvent *core.RequestEvent) error {
		requestEvent.Response.Header().Set("Cache-Control", "no-store")
		http.ServeFile(requestEvent.Response, requestEvent.Request, path)
		return nil
	}
}

// ServeDir returns a handler that serves files under root using a "{path...}"
// route wildcard. Requests attempting to escape the root are rejected.
func ServeDir(root string) func(*core.RequestEvent) error {
	return func(requestEvent *core.RequestEvent) error {
		relPath := filepath.Clean(requestEvent.Request.PathValue("path"))
		if relPath == "." || relPath == "" {
			return requestEvent.NotFoundError("file not found", nil)
		}
		if strings.HasPrefix(relPath, ".."+string(filepath.Separator)) || relPath == ".." {
			return requestEvent.NotFoundError("file not found", nil)
		}
		http.ServeFile(requestEvent.Response, requestEvent.Request, filepath.Join(root, relPath))
		return nil
	}
}
