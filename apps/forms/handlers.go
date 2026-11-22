package main

import (
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// theme is the extension surface, compiled in. An installation's agent edits
// these files; a change goes through preview and the check suite like any
// other, so a broken template cannot reach production by itself.
//
//go:embed theme/*.html
var theme embed.FS

// userHeader carries the person the platform authenticated.
//
// The app does not implement login -- the contract forbids it, because five
// apps each implementing sign-in is five chances to get it wrong and five
// passwords for the customer. The platform terminates identity and passes the
// result on, which is only safe while this app is unreachable except through
// the platform. DABBERZ_TRUSTED_PROXY is the platform asserting exactly
// that, and without it the admin surface serves no one.
const (
	userHeader  = "X-Dabberz-User"
	emailHeader = "X-Dabberz-Email"
	// signInHeader, on a 401, asks the platform to sign the visitor in, or to
	// tell them they are not a member. The app never redirects to a login
	// itself: it cannot tell a stranger from a signed-in non-member, and would
	// send the second round for ever.
	signInHeader = "X-Dabberz-Signin"
)

type app struct {
	store     *Store
	cfg       config
	logger    *slog.Logger
	templates *template.Template
}

func newApp(store *Store, cfg config, logger *slog.Logger) (*app, error) {
	templates, err := template.ParseFS(theme, "theme/*.html")
	if err != nil {
		return nil, err
	}
	return &app{store: store, cfg: cfg, logger: logger, templates: templates}, nil
}

func (a *app) handler() http.Handler {
	// The site is open at the platform's door, so this is where who may reach
	// what is decided. Everything is for members unless it is on this short
	// list: a route added to admin below is private without anyone having to
	// remember to make it so.
	open := http.NewServeMux()

	// Health is open by definition: the platform asks it before it would have
	// anyone to authenticate.
	open.HandleFunc("GET /healthz", a.health)

	// Each form says who it is for; formFor asks it.
	open.HandleFunc("GET /f/{slug}", a.publicForm)
	open.HandleFunc("POST /f/{slug}", a.submit)

	// The admin surface, behind the platform's identity.
	admin := http.NewServeMux()
	open.Handle("/", a.requireUser(admin.ServeHTTP))

	admin.HandleFunc("GET /{$}", a.listForms)
	admin.HandleFunc("POST /forms", a.createForm)
	admin.HandleFunc("GET /forms/{id}", a.showSubmissions)
	admin.HandleFunc("POST /forms/{id}/audience", a.changeAudience)
	admin.HandleFunc("GET /export", a.export)
	admin.HandleFunc("POST /import", a.importData)

	// Agent access. Declared in the manifest, so it is not optional.
	admin.HandleFunc("POST /mcp", a.mcp)

	// Scheduled work the platform invokes. Not public: it arrives with the
	// platform's own identity like anything else.
	admin.HandleFunc("POST /jobs/digest", a.digest)

	return a.logRequests(neverCached(open))
}

// neverCached keeps every answer out of shared caches. Who may see a page can
// change with a click, and a copy at a CDN would not hear about it.
func neverCached(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		next.ServeHTTP(w, r)
	})
}

// requireUser refuses anything the platform has not attributed to a member.
func (a *app) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if member, ok := a.member(w, r); ok {
			next(w, member)
		}
	}
}

// sameOrigin refuses a member's browser being used by another site. The
// session cookie's SameSite is no help: every other customer's app is the
// same site as this one until the preview domain is a public suffix.
var sameOrigin = http.NewCrossOriginProtection()

// member returns the request as the member making it, or answers it and
// reports false.
func (a *app) member(w http.ResponseWriter, r *http.Request) (*http.Request, bool) {
	if !a.cfg.identityReady() {
		a.fail(w, r, http.StatusServiceUnavailable, "No identity configured",
			"This installation cannot tell who is asking, so it will not serve what is for its members.")
		return nil, false
	}

	user := a.cfg.devUser
	if a.cfg.trustProxy {
		if named := strings.TrimSpace(r.Header.Get(userHeader)); named != "" {
			user = named
		}
	}
	if user == "" {
		w.Header().Set(signInHeader, "required")
		a.fail(w, r, http.StatusUnauthorized, "Sign in", "This page is for this app's members.")
		return nil, false
	}
	if err := sameOrigin.Check(r); err != nil {
		a.fail(w, r, http.StatusForbidden, "That came from another site",
			"Open the page here and try again.")
		return nil, false
	}
	return r.WithContext(withUser(r.Context(), user, r.Header.Get(emailHeader))), true
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Ready(r.Context()); err != nil {
		// Honest readiness: answering 200 here while the database is
		// unreachable would have the platform route traffic to a service that
		// cannot serve it.
		http.Error(w, `{"status":"unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// --------------------------------------------------------------- admin

type formRow struct {
	Form  *Form
	Count int
}

func (a *app) listForms(w http.ResponseWriter, r *http.Request) {
	forms, err := a.store.ListForms(r.Context())
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	counts, err := a.store.CountSubmissions(r.Context())
	if err != nil {
		a.serverError(w, r, err)
		return
	}

	rows := make([]formRow, 0, len(forms))
	for _, form := range forms {
		rows = append(rows, formRow{Form: form, Count: counts[form.ID]})
	}
	a.render(w, r, "admin.html", map[string]any{
		"Title": "Forms",
		"Forms": rows,
		"Error": r.URL.Query().Get("error"),
	})
}

func (a *app) createForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		a.fail(w, r, http.StatusBadRequest, "That form could not be read", err.Error())
		return
	}

	form := &Form{
		Title:       r.PostFormValue("title"),
		Description: r.PostFormValue("description"),
		Fields:      parseFieldLines(r.PostFormValue("fields")),
		Audience:    Audience(r.PostFormValue("audience")),
	}
	if err := a.store.CreateForm(r.Context(), form); err != nil {
		// Back to the page that asked, with the reason, rather than a dead end.
		http.Redirect(w, r, "/?error="+urlSafe(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/forms/"+form.ID, http.StatusSeeOther)
}

// changeAudience opens a form to anyone or closes it to members.
func (a *app) changeAudience(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	audience := Audience(r.PostFormValue("audience"))
	if !audience.valid() {
		a.fail(w, r, http.StatusBadRequest, "That is not an audience",
			"A form is for anyone with the link, or for members only.")
		return
	}
	err := a.store.SetAudience(r.Context(), id, audience)
	if errors.Is(err, ErrNotFound) {
		a.fail(w, r, http.StatusNotFound, "No such form", "It may have been deleted.")
		return
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/forms/"+id, http.StatusSeeOther)
}

// parseFieldLines reads the one-per-line question format: "Label | type | required".
//
// A textarea rather than a field builder, deliberately. The elaborate
// no-code editor is the thing customers are paying incumbents for, and here
// the agent is that editor: "add a dropdown for team size" is a sentence.
func parseFieldLines(raw string) []Field {
	var fields []Field
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		field := Field{Label: strings.TrimSpace(parts[0])}
		if len(parts) > 1 {
			field.Type = strings.ToLower(strings.TrimSpace(parts[1]))
		}
		if len(parts) > 2 {
			switch strings.ToLower(strings.TrimSpace(parts[2])) {
			case "required", "yes", "true":
				field.Required = true
			}
		}
		fields = append(fields, field)
	}
	return fields
}

type submissionRow struct {
	When   string
	Values []string
}

func (a *app) showSubmissions(w http.ResponseWriter, r *http.Request) {
	form, err := a.store.FormByID(r.Context(), r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		a.fail(w, r, http.StatusNotFound, "No such form", "It may have been deleted.")
		return
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}

	submissions, err := a.store.ListSubmissions(r.Context(), form.ID, 200)
	if err != nil {
		a.serverError(w, r, err)
		return
	}

	rows := make([]submissionRow, 0, len(submissions))
	for _, submission := range submissions {
		values := make([]string, 0, len(form.Fields))
		for _, field := range form.Fields {
			values = append(values, submission.Data[field.Key])
		}
		rows = append(rows, submissionRow{
			When:   submission.CreatedAt.UTC().Format("2006-01-02 15:04"),
			Values: values,
		})
	}
	a.render(w, r, "submissions.html", map[string]any{
		"Title":       form.Title,
		"Form":        form,
		"Submissions": rows,
	})
}

// --------------------------------------------------------------- answering

// formFor loads the form being asked for, if this visitor is in its audience.
// Otherwise it answers the request and reports false. Only AudiencePublic is
// open, so an audience this code does not know is closed.
func (a *app) formFor(w http.ResponseWriter, r *http.Request) (*Form, bool) {
	form, err := a.store.FormBySlug(r.Context(), r.PathValue("slug"))
	if errors.Is(err, ErrNotFound) {
		a.fail(w, r, http.StatusNotFound, "No such form", "Check the address.")
		return nil, false
	}
	if err != nil {
		a.serverError(w, r, err)
		return nil, false
	}
	if form.Audience == AudiencePublic {
		return form, true
	}
	if _, ok := a.member(w, r); !ok {
		return nil, false
	}
	return form, true
}

func (a *app) publicForm(w http.ResponseWriter, r *http.Request) {
	form, ok := a.formFor(w, r)
	if !ok {
		return
	}
	a.render(w, r, "public.html", map[string]any{
		"Title": form.Title,
		"Form":  form,
		"Error": r.URL.Query().Get("error"),
	})
}

func (a *app) submit(w http.ResponseWriter, r *http.Request) {
	form, ok := a.formFor(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		a.fail(w, r, http.StatusBadRequest, "That could not be read", err.Error())
		return
	}

	data := make(map[string]string, len(form.Fields))
	for _, field := range form.Fields {
		// Only declared fields are stored. Anything else a client sends is
		// discarded rather than written: a submission is the shape the form
		// asked for, not whatever arrived.
		value := strings.TrimSpace(r.PostFormValue(field.Key))
		if field.Required && value == "" {
			http.Redirect(w, r, "/f/"+form.Slug+"?error="+urlSafe(field.Label+" is required"),
				http.StatusSeeOther)
			return
		}
		data[field.Key] = value
	}

	if err := a.store.Submit(r.Context(), &Submission{FormID: form.ID, Data: data}); err != nil {
		a.serverError(w, r, err)
		return
	}
	a.render(w, r, "thanks.html", map[string]any{"Title": "Thank you", "Form": form})
}

// digest is the scheduled job the manifest declares.
func (a *app) digest(w http.ResponseWriter, r *http.Request) {
	forms, err := a.store.ListForms(r.Context())
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	counts, err := a.store.CountSubmissions(r.Context())
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	// Mail is a platform block and is not wired up yet, so this reports what
	// it would have sent rather than pretending to have sent it.
	total := 0
	for _, form := range forms {
		total += counts[form.ID]
	}
	a.logger.Info("digest", "forms", len(forms), "submissions", total)
	writeJSON(w, http.StatusOK, map[string]any{
		"forms": len(forms), "submissions": total, "sent": false,
		"note": "mail is not wired up yet; this reports rather than sends",
	})
}

// --------------------------------------------------------------- plumbing

func (a *app) render(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	// Clone so one request's page definition cannot leak into another's.
	templates, err := a.templates.Clone()
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	if _, err := templates.ParseFS(theme, "theme/"+page); err != nil {
		a.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, "layout", data); err != nil {
		// The response has already started, so there is nothing to send but a
		// log line.
		a.logger.Error("render", "page", page, "error", err)
	}
}

func (a *app) fail(w http.ResponseWriter, r *http.Request, status int, title, message string) {
	w.WriteHeader(status)
	a.render(w, r, "error.html", map[string]any{"Title": title, "Message": message})
}

func (a *app) serverError(w http.ResponseWriter, r *http.Request, err error) {
	a.logger.Error("request failed", "path", r.URL.Path, "error", err)
	// The reason goes to the log, not to the page: an internal error message
	// is for an operator and may name things a visitor should not see.
	a.fail(w, r, http.StatusInternalServerError, "Something went wrong",
		"This has been logged. Try again in a moment.")
}

func (a *app) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		a.logger.Info("request", "method", r.Method, "path", r.URL.Path,
			"ms", time.Since(started).Milliseconds())
	})
}
