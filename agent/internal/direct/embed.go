// agent/internal/direct/embed.go
package direct

import "embed"

// staticFS embeds the recipient gallery UI (index.html + gallery.js + app.js +
// the extracted stylesheet + the lightGallery vendor tree + the CSP-safe
// placeholder image). It is rooted at the direct package's static/ directory
// and served by DirectServer under /s/{code}/static/…
//
// Note: the UI lives under this package's own static/ subtree (not the admin
// UI's internal/web/static) because go:embed cannot reference files outside
// the embedding package's directory tree, and importing internal/web here
// would create an import cycle (web → daemon → direct).
//
//go:embed static
var staticFS embed.FS
