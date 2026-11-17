package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// The database is real, deliberately. A store tested against a stand-in proves
// the stand-in works: the things that actually break here are a foreign key, a
// unique index and a JSONB round trip, and none of them exist in a fake.
const testDatabaseEnv = "FORMS_TEST_DATABASE_URL"

func testStore(t *testing.T) *Store {
	t.Helper()

	dsn := os.Getenv(testDatabaseEnv)
	if dsn == "" {
		t.Skip("set " + testDatabaseEnv + " to run against a real postgres")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	store := &Store{db: db}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Each test starts from an empty installation. CASCADE because
	// submissions reference forms, which is the point of the constraint.
	if _, err := db.Exec(`TRUNCATE submissions, forms CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return store
}

func testApp(t *testing.T) (*app, *Store) {
	t.Helper()

	store := testStore(t)
	built, err := newApp(store, config{devUser: "owner@example.com"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	return built, store
}

func contactForm() *Form {
	return &Form{
		Title:       "Contact us",
		Description: "We read everything.",
		Fields: []Field{
			{Label: "Your name", Type: "text", Required: true},
			{Label: "Email", Type: "email", Required: true},
			{Label: "What can we help with?", Type: "textarea"},
		},
	}
}

// TestCriticalPath is what the check suite runs: the sequence whose breakage
// costs a customer money. A form is published, somebody answers it, and the
// answer is there. Everything else in this app is convenience.
func TestCriticalPath(t *testing.T) {
	built, store := testApp(t)
	ctx := context.Background()

	form := contactForm()
	if err := store.CreateForm(ctx, form); err != nil {
		t.Fatalf("CreateForm: %v", err)
	}
	if form.Slug != "contact-us" {
		t.Fatalf("slug = %q, want one derived from the title", form.Slug)
	}

	server := httptest.NewServer(built.handler())
	defer server.Close()

	// The form is readable by anyone: that is what a form is for.
	page, err := http.Get(server.URL + "/f/" + form.Slug)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer page.Body.Close()
	body, _ := io.ReadAll(page.Body)
	if page.StatusCode != http.StatusOK {
		t.Fatalf("GET /f/%s = %d", form.Slug, page.StatusCode)
	}
	if !strings.Contains(string(body), "What can we help with?") {
		t.Fatalf("the form did not render its questions:\n%s", body)
	}

	// Somebody answers it.
	answers := url.Values{
		"your-name":             {"Ada"},
		"email":                 {"ada@example.com"},
		"what-can-we-help-with": {"The checkout is broken"},
		"something-i-made-up":   {"should be discarded"},
	}
	posted, err := http.PostForm(server.URL+"/f/"+form.Slug, answers)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer posted.Body.Close()
	if posted.StatusCode != http.StatusOK {
		t.Fatalf("POST /f/%s = %d", form.Slug, posted.StatusCode)
	}

	// And it is there.
	stored, err := store.ListSubmissions(ctx, form.ID, 10)
	if err != nil {
		t.Fatalf("ListSubmissions: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("got %d submissions, want 1", len(stored))
	}
	if stored[0].Data["your-name"] != "Ada" {
		t.Fatalf("stored %v", stored[0].Data)
	}
	// Only declared fields are kept: a submission is the shape the form asked
	// for, not whatever a client chose to send.
	if _, kept := stored[0].Data["something-i-made-up"]; kept {
		t.Fatalf("an undeclared field was stored: %v", stored[0].Data)
	}
}

// TestARequiredQuestionIsRequired: a form that silently accepts a blank
// required answer is collecting rubbish, which is worse than refusing.
func TestARequiredQuestionIsRequired(t *testing.T) {
	built, store := testApp(t)
	form := contactForm()
	if err := store.CreateForm(context.Background(), form); err != nil {
		t.Fatalf("CreateForm: %v", err)
	}

	server := httptest.NewServer(built.handler())
	defer server.Close()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, err := client.PostForm(server.URL+"/f/"+form.Slug, url.Values{"email": {"a@b.c"}})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect back to the form", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Location"), "required") {
		t.Fatalf("location = %q, which does not say what was missing", resp.Header.Get("Location"))
	}

	stored, _ := store.ListSubmissions(context.Background(), form.ID, 10)
	if len(stored) != 0 {
		t.Fatalf("an incomplete submission was stored: %v", stored)
	}
}

// TestExportRoundTrips is the portability obligation from the platform
// contract, as a test rather than a promise. An export that cannot be imported
// is the usual shape of lock-in, and it decays silently unless something
// checks it every release.
func TestExportRoundTrips(t *testing.T) {
	built, store := testApp(t)
	ctx := context.Background()

	form := contactForm()
	if err := store.CreateForm(ctx, form); err != nil {
		t.Fatalf("CreateForm: %v", err)
	}
	second := &Form{Title: "Event signup", Fields: []Field{{Label: "Name"}, {Label: "Coming", Type: "checkbox"}}}
	if err := store.CreateForm(ctx, second); err != nil {
		t.Fatalf("CreateForm: %v", err)
	}
	for i := range 25 {
		if err := store.Submit(ctx, &Submission{
			FormID: form.ID,
			Data:   map[string]string{"your-name": "Person " + string(rune('A'+i%26))},
		}); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}

	server := httptest.NewServer(built.handler())
	defer server.Close()

	// Export over HTTP, the way a customer leaving would.
	resp, err := http.Get(server.URL + "/export")
	if err != nil {
		t.Fatalf("GET /export: %v", err)
	}
	defer resp.Body.Close()
	exported, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /export = %d: %s", resp.StatusCode, exported)
	}

	var archive Archive
	if err := json.Unmarshal(exported, &archive); err != nil {
		t.Fatalf("the export is not readable JSON: %v", err)
	}
	if len(archive.Forms) != 2 || len(archive.Submissions) != 25 {
		t.Fatalf("archive has %d forms and %d submissions, want 2 and 25",
			len(archive.Forms), len(archive.Submissions))
	}

	// Empty the installation and import the archive back into it.
	if _, err := store.db.Exec(`TRUNCATE submissions, forms CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	imported, err := http.Post(server.URL+"/import", "application/json", bytes.NewReader(exported))
	if err != nil {
		t.Fatalf("POST /import: %v", err)
	}
	defer imported.Body.Close()
	if imported.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(imported.Body)
		t.Fatalf("POST /import = %d: %s", imported.StatusCode, body)
	}

	// The second export must equal the first. Anything that does not survive
	// the trip is data a customer would have lost by leaving.
	again, err := store.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	// ExportedAt is the one field expected to differ.
	again.ExportedAt = archive.ExportedAt
	first, _ := json.Marshal(&archive)
	second2, _ := json.Marshal(again)
	if !bytes.Equal(first, second2) {
		t.Fatalf("the installation did not survive a round trip\nbefore: %s\nafter:  %s", first, second2)
	}
}

// TestImportRefusesToMerge: deciding what wins when an archive meets existing
// data is how an import eats a week of submissions.
func TestImportRefusesToMerge(t *testing.T) {
	built, store := testApp(t)
	ctx := context.Background()
	if err := store.CreateForm(ctx, contactForm()); err != nil {
		t.Fatalf("CreateForm: %v", err)
	}

	archive, err := store.Export(ctx)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	encoded, _ := json.Marshal(archive)

	server := httptest.NewServer(built.handler())
	defer server.Close()

	resp, err := http.Post(server.URL+"/import", "application/json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("POST /import: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

// TestTheAdminSurfaceRefusesWithoutAnIdentity: an app that serves its admin
// pages to anyone because identity was misconfigured is the failure that
// matters most, so it fails closed and says why.
func TestTheAdminSurfaceRefusesWithoutAnIdentity(t *testing.T) {
	store := testStore(t)
	built, err := newApp(store, config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}

	server := httptest.NewServer(built.handler())
	defer server.Close()

	for _, path := range []string{"/", "/export", "/mcp"} {
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("GET %s served without an identity", path)
		}
	}

	// A public form still works: it is meant to be public.
	form := contactForm()
	if err := store.CreateForm(context.Background(), form); err != nil {
		t.Fatalf("CreateForm: %v", err)
	}
	resp, err := http.Get(server.URL + "/f/" + form.Slug)
	if err != nil {
		t.Fatalf("GET public: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the public form returned %d", resp.StatusCode)
	}
}

// TestHealthIsHonest: answering 200 while the database is unreachable has the
// platform route traffic to a service that cannot serve it.
func TestHealthIsHonest(t *testing.T) {
	built, store := testApp(t)
	server := httptest.NewServer(built.handler())
	defer server.Close()

	resp, err := http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthy app reported %d", resp.StatusCode)
	}

	store.db.Close()
	resp, err = http.Get(server.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("an app with no database reported %d, want 503", resp.StatusCode)
	}
}

// rpc calls the agent surface the way a client would.
func rpc(t *testing.T, server *httptest.Server, method string, params any) map[string]any {
	t.Helper()

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	resp, err := http.Post(server.URL+"/mcp", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /mcp %s: %v", method, err)
	}
	defer resp.Body.Close()

	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode %s: %v", method, err)
	}
	if failure, bad := decoded["error"]; bad {
		t.Fatalf("%s failed: %v", method, failure)
	}
	result, _ := decoded["result"].(map[string]any)
	return result
}

// TestTheAgentSurfaceAnswers covers the contract's least negotiable
// requirement: every app is addressable by an agent, free, in every plan.
func TestTheAgentSurfaceAnswers(t *testing.T) {
	built, store := testApp(t)
	ctx := context.Background()

	form := contactForm()
	if err := store.CreateForm(ctx, form); err != nil {
		t.Fatalf("CreateForm: %v", err)
	}
	if err := store.Submit(ctx, &Submission{
		FormID: form.ID, Data: map[string]string{"your-name": "Ada"},
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	server := httptest.NewServer(built.handler())
	defer server.Close()

	if got := rpc(t, server, "initialize", map[string]any{"protocolVersion": "2025-06-18"}); got["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize returned %v", got)
	}

	listed := rpc(t, server, "tools/list", map[string]any{})
	tools, _ := listed["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("the app advertises no tools")
	}
	for _, entry := range tools {
		described, _ := entry.(map[string]any)
		if described["description"] == "" || described["inputSchema"] == nil {
			t.Errorf("tool %v is not usable by an agent: %v", described["name"], described)
		}
	}

	called := rpc(t, server, "tools/call", map[string]any{
		"name": "list_submissions", "arguments": map[string]any{"form": form.Slug},
	})
	content, _ := called["content"].([]any)
	if len(content) == 0 {
		t.Fatal("tools/call returned no content")
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	if !strings.Contains(text, "Ada") {
		t.Fatalf("the agent could not read the submission: %s", text)
	}

	// A tool that fails is a result the agent reads and acts on, not a
	// transport error it cannot see.
	missing := rpc(t, server, "tools/call", map[string]any{
		"name": "list_submissions", "arguments": map[string]any{"form": "no-such-form"},
	})
	if missing["isError"] != true {
		t.Fatalf("a failed tool call did not report isError: %v", missing)
	}
}
