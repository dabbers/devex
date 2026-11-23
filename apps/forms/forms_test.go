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
		Audience:    AudiencePublic,
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
	body, _ := io.ReadAll(resp.Body)
	// The form again, saying what was missing and keeping what was typed --
	// on a long form, losing every answer to one missed question is how
	// people give up.
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want the form shown again", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Your name: is required") || !strings.Contains(string(body), `value="a@b.c"`) {
		t.Fatalf("the form did not say what was missing or lost what was typed:\n%s", body)
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

// behindPlatform is the app as installed: identity is whatever header the edge
// asserts, and nobody by default.
func behindPlatform(t *testing.T) (http.Handler, *Store) {
	t.Helper()
	store := testStore(t)
	built, err := newApp(store, config{trustProxy: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newApp: %v", err)
	}
	return built.handler(), store
}

func ask(h http.Handler, method, target, as string, body url.Values, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://forms.example"+target, strings.NewReader(body.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if as != "" {
		r.Header.Set(userHeader, as)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestEachFormHasItsAudience: forms on one site target different people, and
// changing one moves only that one.
func TestEachFormHasItsAudience(t *testing.T) {
	h, store := behindPlatform(t)
	ctx := context.Background()

	create := func(title, audience string) *Form {
		t.Helper()
		values := url.Values{"title": {title}, "fields": {"Name"}}
		if audience != "" {
			values.Set("audience", audience)
		}
		if w := ask(h, "POST", "/forms", "usr_member", values); w.Code != http.StatusSeeOther {
			t.Fatalf("create %s = %d", title, w.Code)
		}
		form, err := store.FormBySlug(ctx, slugify(title))
		if err != nil {
			t.Fatalf("%s was not created: %v", title, err)
		}
		return form
	}
	jobs, review, unsaid := create("Jobs", "public"), create("Review", "internal"), create("Unsaid", "")

	expect := func(form *Form, anyone bool) {
		t.Helper()
		for _, method := range []string{"GET", "POST"} {
			w := ask(h, method, "/f/"+form.Slug, "", url.Values{"name": {"A stranger"}})
			if anyone && w.Code != http.StatusOK {
				t.Errorf("%s public %s = %d for a stranger", method, form.Slug, w.Code)
			}
			if !anyone {
				if w.Code != http.StatusUnauthorized || w.Header().Get(signInHeader) != "required" {
					t.Errorf("%s internal %s = %d for a stranger, want a request to sign in", method, form.Slug, w.Code)
				}
				if strings.Contains(w.Body.String(), form.Title) {
					t.Errorf("%s internal %s showed a stranger the form", method, form.Slug)
				}
			}
			if !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
				t.Errorf("%s %s can be cached", method, form.Slug)
			}
		}
		if !anyone {
			if rows, _ := store.ListSubmissions(ctx, form.ID, 10); len(rows) != 0 {
				t.Errorf("a stranger's answer to internal %s was stored", form.Slug)
			}
		}
		if w := ask(h, "GET", "/f/"+form.Slug, "usr_member", nil); w.Code != http.StatusOK {
			t.Errorf("a member got %d from %s", w.Code, form.Slug)
		}
	}
	expect(jobs, true)
	expect(review, false)
	expect(unsaid, false)

	set := func(form *Form, as, audience string) int {
		return ask(h, "POST", "/forms/"+form.ID+"/audience", as, url.Values{"audience": {audience}}).Code
	}
	if got := set(review, "", "public"); got != http.StatusUnauthorized {
		t.Errorf("a stranger changing an audience got %d", got)
	}
	if got := set(review, "usr_member", "everyone"); got != http.StatusBadRequest {
		t.Errorf("an unknown audience got %d", got)
	}
	expect(review, false)
	if got := set(review, "usr_member", "public"); got != http.StatusSeeOther {
		t.Fatalf("opening a form got %d", got)
	}
	expect(review, true)
	expect(unsaid, false)
	if got := set(&Form{ID: "frm_none"}, "usr_member", "public"); got != http.StatusNotFound {
		t.Errorf("a missing form got %d", got)
	}
}

// TestOnlyTheOpenListIsOpen: the site's door is open, so every route not meant
// for strangers has to refuse them here, including one nobody has written yet.
func TestOnlyTheOpenListIsOpen(t *testing.T) {
	h, _ := behindPlatform(t)
	for _, route := range []string{
		"GET /", "POST /forms", "GET /forms/x", "POST /forms/x/audience",
		"GET /export", "POST /import", "POST /mcp", "POST /jobs/digest",
		"GET /a-route-written-tomorrow", "DELETE /f/x", "POST /healthz",
		"GET /f/", "GET /f/x/more",
	} {
		method, target, _ := strings.Cut(route, " ")
		w := ask(h, method, target, "", nil)
		if w.Code != http.StatusUnauthorized || w.Header().Get(signInHeader) != "required" {
			t.Errorf("%s = %d for a stranger, want a request to sign in", route, w.Code)
		}
	}
	if w := ask(h, "GET", "/healthz", "", nil); w.Code != http.StatusOK {
		t.Errorf("health = %d for a stranger", w.Code)
	}
}

// TestAnotherSiteCannotActAsAMember: every other customer's app is same-site
// with this one, so a member's cookie alone must not be enough to write.
func TestAnotherSiteCannotActAsAMember(t *testing.T) {
	h, store := behindPlatform(t)
	form := &Form{Title: "Review", Fields: []Field{{Label: "Name"}}}
	if err := store.CreateForm(context.Background(), form); err != nil {
		t.Fatal(err)
	}
	open := url.Values{"audience": {"public"}}
	for _, headers := range [][]string{
		{"Sec-Fetch-Site", "same-site"},
		{"Sec-Fetch-Site", "cross-site"},
		{"Origin", "https://other.example"},
	} {
		for _, target := range []string{"/forms/" + form.ID + "/audience", "/f/" + form.Slug, "/import"} {
			if w := ask(h, "POST", target, "usr_member", open, headers...); w.Code != http.StatusForbidden {
				t.Errorf("POST %s with %v = %d, want 403", target, headers, w.Code)
			}
		}
	}
	if w := ask(h, "POST", "/forms/"+form.ID+"/audience", "usr_member", open,
		"Sec-Fetch-Site", "same-origin", "Origin", "https://forms.example"); w.Code != http.StatusSeeOther {
		t.Errorf("the app's own page = %d", w.Code)
	}
}

// TestAnArchiveKeepsItsAudiences: losing one on the way through publishes it.
func TestAnArchiveKeepsItsAudiences(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	archive := func(version int, audience Audience) *Archive {
		return &Archive{Format: exportFormat, Version: version, Forms: []*Form{{
			ID: "frm_kept", Slug: "kept", Title: "Kept", Audience: audience, Fields: []Field{},
		}}}
	}
	if err := store.Import(ctx, archive(2, "")); err == nil || !strings.Contains(err.Error(), "who it is for") {
		t.Fatalf("a version 2 archive with no audience was not refused as one: %v", err)
	}
	for _, c := range []struct {
		version    int
		said, want Audience
	}{{1, "", AudiencePublic}, {2, AudienceInternal, AudienceInternal}, {2, AudiencePublic, AudiencePublic}} {
		if _, err := store.db.Exec(`TRUNCATE submissions, forms CASCADE`); err != nil {
			t.Fatal(err)
		}
		if err := store.Import(ctx, archive(c.version, c.said)); err != nil {
			t.Fatalf("import v%d: %v", c.version, err)
		}
		out, err := store.Export(ctx)
		if err != nil || out.Forms[0].Audience != c.want {
			t.Errorf("v%d %q came back as %+v (%v), want %q", c.version, c.said, out, err, c.want)
		}
	}
}

// TestFormsFromBeforeAudiencesStayPublic: upgrading must not close a form
// somebody has already handed out.
func TestFormsFromBeforeAudiencesStayPublic(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	for _, q := range []string{
		`ALTER TABLE forms DROP COLUMN audience`,
		`DELETE FROM schema_migrations WHERE name = '0004_form_audience'`,
		`INSERT INTO forms (id, slug, title) VALUES ('frm_old', 'old', 'Old')`,
	} {
		if _, err := store.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO forms (id, slug, title) VALUES ('frm_new', 'new', 'New')`); err != nil {
		t.Fatal(err)
	}
	for slug, want := range map[string]Audience{"old": AudiencePublic, "new": AudienceInternal} {
		if form, err := store.FormBySlug(ctx, slug); err != nil || form.Audience != want {
			t.Errorf("%s = %+v (%v), want %q", slug, form, err, want)
		}
	}
}
