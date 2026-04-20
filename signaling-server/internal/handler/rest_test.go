package handler

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/stretchr/testify/require"
)

func TestServeDir_ServesJavaScriptModuleAsset(t *testing.T) {
	root := t.TempDir()
	srcDir := filepath.Join(root, "src")
	require.NoError(t, os.MkdirAll(srcDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(srcDir, "app.js"), []byte("export const value = 1;\n"), 0o644))

	req := httptest.NewRequest(http.MethodGet, "/src/app.js", nil)
	req.SetPathValue("path", "app.js")
	rec := httptest.NewRecorder()

	event := &core.RequestEvent{
		Event: router.Event{
			Request:  req,
			Response: rec,
		},
	}

	err := ServeDir(srcDir)(event)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "javascript")
	require.Contains(t, rec.Body.String(), "export const value = 1")
}
