// Package web serves the dabberz control-plane UI.
//
// The assets are embedded in the binary and need no build step, which keeps
// deployment to the single binary the rest of the system already is. Routing
// is done in the fragment, so there is no server-side rewrite to keep in sync
// with the client.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var assets embed.FS

// FS returns the embedded asset tree rooted at the static directory.
func FS() (fs.FS, error) { return fs.Sub(assets, "static") }

// Handler serves the UI.
func Handler() (http.Handler, error) {
	root, err := FS()
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServerFS(root)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The UI is served from a fragment-routed single page, so any path
		// that is not an asset is that page rather than a 404. Without this,
		// reloading on a deep link would fail.
		if !isAsset(r.URL.Path) {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		// The assets ship inside the binary and change only when it does, so
		// revalidating on each load is what keeps a redeploy visible.
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	}), nil
}

// isAsset reports whether a path names a file rather than a UI route.
func isAsset(path string) bool {
	base := path[strings.LastIndex(path, "/")+1:]
	return strings.Contains(base, ".")
}
