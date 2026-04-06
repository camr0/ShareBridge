package handler

import (
	"net/http"

	"github.com/pocketbase/pocketbase/core"
)

// ServeFile returns a handler that serves a single static file.
func ServeFile(path string) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		http.ServeFile(e.Response, e.Request, path)
		return nil
	}
}
