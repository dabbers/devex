package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAssetsAreEmbedded(t *testing.T) {
	root, err := FS()
	if err != nil {
		t.Fatalf("FS: %v", err)
	}
	// The binary must carry the whole UI: a missing asset would only show up
	// as a blank page at runtime.
	for _, name := range []string{"index.html", "app.js", "style.css"} {
		if _, err := fs.Stat(root, name); err != nil {
			t.Errorf("asset %s is not embedded: %v", name, err)
		}
	}
}

func TestHandlerServesAssets(t *testing.T) {
	handler, err := Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}

	tests := map[string]string{
		"/":          "text/html",
		"/app.js":    "javascript",
		"/style.css": "text/css",
	}
	for path, wantType := range tests {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
			continue
		}
		if got := rec.Header().Get("Content-Type"); !strings.Contains(got, wantType) {
			t.Errorf("GET %s content type = %q, want it to contain %q", path, got, wantType)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s returned an empty body", path)
		}
	}
}

func TestIndexIsCanonicalisedToRoot(t *testing.T) {
	handler, err := Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	// The file server canonicalises /index.html to /; asserting it redirects
	// rather than errors keeps that behaviour pinned.
	req := httptest.NewRequest(http.MethodGet, "/index.html", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("GET /index.html = %d, want a redirect to /", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "./" && got != "/" {
		t.Fatalf("Location = %q, want the root", got)
	}
}

func TestDeepLinksServeTheApp(t *testing.T) {
	handler, err := Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	// Reloading on a deep link must return the page, not a 404, or the UI
	// would break on refresh.
	for _, path := range []string{"/fork/fork_123", "/activity", "/repo/repo_abc"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want the app shell", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "<title>dabberz</title>") {
			t.Errorf("GET %s did not return the app shell", path)
		}
	}
}

func TestMissingAssetIsNotFound(t *testing.T) {
	handler, err := Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	// A genuinely missing file must 404 rather than silently serving HTML,
	// which would turn a typo in a script tag into a confusing parse error.
	req := httptest.NewRequest(http.MethodGet, "/nope.js", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /nope.js = %d, want 404", rec.Code)
	}
}

func TestAssetsRevalidate(t *testing.T) {
	handler, err := Handler()
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/app.js", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Assets change when the binary does, so a cached copy must be revalidated
	// or a redeploy would not be visible.
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
}

func TestUIDoesNotAssignUntrustedHTML(t *testing.T) {
	root, err := FS()
	if err != nil {
		t.Fatalf("FS: %v", err)
	}
	script, err := fs.ReadFile(root, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	// Agent output, verifier reports and workstream names all reach the feed.
	// Assigning any of it to innerHTML would execute it.
	for _, sink := range []string{"innerHTML =", "outerHTML =", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(string(script), sink) {
			t.Errorf("app.js uses %q; untrusted agent output must not reach an HTML sink", sink)
		}
	}
}

func TestStylesheetCarriesTheDesignSystemTokens(t *testing.T) {
	root, err := FS()
	if err != nil {
		t.Fatalf("FS: %v", err)
	}
	styles, err := fs.ReadFile(root, "style.css")
	if err != nil {
		t.Fatalf("read style.css: %v", err)
	}
	sheet := string(styles)

	// Broadsheet is a token system; colors and spacing come from variables so
	// the look can be retuned in one place.
	for _, token := range []string{"--color-accent:", "--color-bg:", "--font-heading:", "--space-3:", "--radius-md:"} {
		if !strings.Contains(sheet, token) {
			t.Errorf("stylesheet is missing the %s token", token)
		}
	}
	// The serif is the chrome: no sans-serif UI font is introduced.
	if strings.Contains(sheet, "system-ui, -apple-system") {
		t.Error("a sans-serif UI stack was reintroduced; the serif is the chrome")
	}
	// A webfont that cannot be reached must fall back to a real serif, since a
	// self-hosted control plane may have no route to Google Fonts.
	if !strings.Contains(sheet, "Georgia") {
		t.Error("the heading stack has no local serif fallback")
	}
}

func TestUISpeaksTheProductVocabulary(t *testing.T) {
	root, err := FS()
	if err != nil {
		t.Fatalf("FS: %v", err)
	}
	script, err := fs.ReadFile(root, "app.js")
	if err != nil {
		t.Fatalf("read app.js: %v", err)
	}
	// The interface says repository / project / sub-task where the API says
	// repo / task / fork. Losing that mapping is how the two drift apart.
	for _, word := range []string{"sub-task", "project", "machine"} {
		if !strings.Contains(string(script), word) {
			t.Errorf("the UI does not use the word %q anywhere", word)
		}
	}
}
