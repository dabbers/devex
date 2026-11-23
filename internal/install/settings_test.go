package install

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func smallPNG(t *testing.T, shade uint8) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, 6, 2))
	for i := range img.Pix {
		img.Pix[i] = shade
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// TestAnOwnersSettingsReachTheirLiveSiteWithoutAChange: the app declares what
// may be changed, the owner chooses, and the live site shows it on the next
// page -- no agent, no rebuild, no approval -- with the logo served by the
// edge and nothing else from the library reachable.
func TestAnOwnersSettingsReachTheirLiveSiteWithoutAChange(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles an app and provisions a database")
	}
	dsn := os.Getenv(clusterEnv)
	if dsn == "" {
		t.Skip("set " + clusterEnv + " to a superuser DSN to run the install")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	installer, _, owner, edge, visiting := newInstaller(t, dsn)
	installation, err := installer.Install(ctx, Request{UserID: owner.ID, App: "forms", Name: "branded"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	waitForHealth(t, ctx, edge, installation.URL, "/healthz")

	view, err := installer.Settings(ctx, installation)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if view.Spec == nil || view.Problem != "" {
		t.Fatalf("the installed app declares no usable settings: %q", view.Problem)
	}
	if view.Effective["/brand/name"] != "Forms" {
		t.Fatalf("the default name is %v", view.Effective["/brand/name"])
	}

	lib := installer.deps.Library
	logo, err := lib.Put(ctx, owner.ID, "", "brand/logo.png", smallPNG(t, 40), "person")
	if err != nil {
		t.Fatal(err)
	}
	private, err := lib.Put(ctx, owner.ID, "", "brand/unused.png", smallPNG(t, 200), "person")
	if err != nil {
		t.Fatal(err)
	}

	// Values the app did not declare, or that do not fit, are refused whole.
	for _, bad := range []map[string]any{
		{"/brand/accent": "red"}, {"/brand/name": "<b>x</b>\u0000"}, {"/nope": "x"},
		{"/brand/logo": "lib_00000000000000000000000000"},
	} {
		if _, err := installer.SetSettings(ctx, installation, "person", bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}

	if _, err := installer.SetSettings(ctx, installation, "person", map[string]any{
		"/brand/name": "Acme Hiring", "/brand/accent": "#1D2B4F", "/brand/logo": logo.ID,
	}); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	get := func(path string) (*http.Response, string) {
		t.Helper()
		target, host := edgeOf(t, edge, installation.URL, path)
		req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
		req.Host = host
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(body)
	}

	// Through the real edge to the real app, as a stranger, on a form page.
	visiting.be(owner)
	created := post(t, &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}, edge, installation.URL, "/forms", map[string][]string{
		"title": {"Apply"}, "fields": {"Name"}, "audience": {"public"},
	}, nil)
	if created != http.StatusSeeOther {
		t.Fatalf("creating a form returned %d", created)
	}
	visiting.be(nil)

	var page string
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, page = get("/f/apply")
		if strings.Contains(page, "--maroon:#1d2b4f") || time.Now().After(deadline) {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	logoURL := "/_dabberz/asset/" + logo.SHA256 + ".png"
	for _, want := range []string{"Acme Hiring", "--maroon:#1d2b4f", logoURL} {
		if !strings.Contains(page, want) {
			t.Errorf("the live page does not show %q", want)
		}
	}

	resp, body := get(logoURL)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/png" || !strings.HasPrefix(body, "\x89PNG") {
		t.Fatalf("the edge did not serve the chosen logo: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if resp, _ := get("/_dabberz/asset/" + private.SHA256 + ".png"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a library picture no setting uses was served: %d", resp.StatusCode)
	}

	// The file the app reads is what was chosen, and the app cannot change it.
	ws, err := installer.deps.Store.GetWorkspace(ctx, installation.WorkspaceID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := installer.deps.Workspaces.ReadFile(ctx, ws, SettingsFile, 1<<20)
	if err != nil {
		t.Fatalf("reading what the app was given: %v", err)
	}
	var delivered Delivered
	if err := json.Unmarshal(raw, &delivered); err != nil || delivered.Values["/brand/name"] != "Acme Hiring" {
		t.Fatalf("delivered %s (%v)", raw, err)
	}

	// Clearing a value puts the default back.
	if _, err := installer.SetSettings(ctx, installation, "person", map[string]any{"/brand/name": nil}); err != nil {
		t.Fatal(err)
	}
	view, _ = installer.Settings(ctx, installation)
	if view.Effective["/brand/name"] != "Forms" || view.Effective["/brand/accent"] != "#1d2b4f" {
		t.Fatalf("after clearing the name: %v", view.Effective)
	}
}
