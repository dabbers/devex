package main

import (
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func renderWithSettings(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if contents != "" {
		if err := os.WriteFile(path, []byte(contents), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(settingsEnv, path)
	a, err := newApp(nil, config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	a.render(rec, httptest.NewRequest("GET", "/", nil), "error.html",
		map[string]any{"Title": "Hello", "Message": "m"})
	return rec.Body.String()
}

// TestTheOwnersSettingsReachThePage: what the owner chose in the panel is what
// the page shows, without a restart and without code.
func TestTheOwnersSettingsReachThePage(t *testing.T) {
	logo := "/_dabberz/asset/" + strings.Repeat("a", 64) + ".png"
	page := renderWithSettings(t, `{"values": {"/brand/name": "Acme Hiring", "/brand/tagline": "Join us",
		"/brand/accent": "#1d2b4f", "/brand/header": "#0b1020"},
		"images": {"/brand/logo": {"url": "`+logo+`"}}}`)
	for _, want := range []string{"Hello · Acme Hiring", "--maroon:#1d2b4f", "--maroon-deep:#0b1020", `src="` + logo + `"`, `alt="Acme Hiring"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not contain %q:\n%s", want, page)
		}
	}
}

// TestNoSettingsMeansTheDefaults: an app that has never been configured looks
// as it always did.
func TestNoSettingsMeansTheDefaults(t *testing.T) {
	page := renderWithSettings(t, "")
	for _, want := range []string{"Hello · Forms", "--maroon:#7a1f2b", "<span>on dabberz</span>"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not contain %q", want)
		}
	}
}

// TestASettingCannotInjectIntoThePage: the file is the platform's, but a value
// that is not what it should be is dropped, and text is escaped, so nothing
// in it becomes markup, style or a link elsewhere.
func TestASettingCannotInjectIntoThePage(t *testing.T) {
	page := renderWithSettings(t, `{"values": {
		"/brand/name": "<script>alert(1)</script>",
		"/brand/accent": "red;}body{display:none",
		"/brand/header": "#12345g"},
		"images": {"/brand/logo": {"url": "https://evil.example/x.png"}}}`)
	if strings.Contains(page, "<script>alert") {
		t.Error("a site name became markup")
	}
	if strings.Contains(page, "body{display:none") || !strings.Contains(page, "--maroon:#7a1f2b") {
		t.Error("an accent that is not a colour reached the style sheet")
	}
	if !strings.Contains(page, "--maroon-deep:#3a1015") {
		t.Error("a header colour that is not a colour was used")
	}
	if strings.Contains(page, "evil.example") {
		t.Error("a logo from somewhere other than the platform was linked")
	}
}

// TestSettingsAreReadAgainWhenTheyChange: the platform rewrites the file when
// the owner saves, and the next page after that shows it.
func TestSettingsAreReadAgainWhenTheyChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(path, []byte(`{"values": {"/brand/name": "First"}}`), 0o644)
	t.Setenv(settingsEnv, path)
	s := newSiteSettings()
	if got := s.get().Name; got != "First" {
		t.Fatalf("name = %q", got)
	}
	os.WriteFile(path, []byte(`{"values": {"/brand/name": "Second one"}}`), 0o644)
	later := time.Now().Add(2 * time.Second)
	os.Chtimes(path, later, later)
	s.checked = time.Time{}
	if got := s.get().Name; got != "Second one" {
		t.Fatalf("after the file changed, name = %q", got)
	}
}
