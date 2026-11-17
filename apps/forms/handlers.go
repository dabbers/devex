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
	mux := http.NewServeMux()

	// Health is open by definition: the platform asks it before it would have
	// anyone to authenticate.
	mux.HandleFunc("GET /healthz", a.health)

	// Public. These are the whole point of a form.
	mux.HandleFunc("GET /f/{slug}", a.publicForm)
	mux.HandleFunc("POST /f/{slug}", a.submit)

	// The admin surface, behind the platform's identity.
	mux.HandleFunc("GET /{$}", a.requireUser(a.listForms))
	mux.HandleFunc("POST /forms", a.requireUser(a.createForm))
	mux.HandleFunc("GET /forms/{id}", a.requireUser(a.showSubmissions))
	mux.HandleFunc("GET /export", a.requireUser(a.export))
	mux.HandleFunc("POST /import", a.requireUser(a.importData))

	// Agent access. Declared in the manifest, so it is not optional.
	mux.HandleFunc("POST /mcp", a.requireUser(a.mcp))

	// Scheduled work the platform invokes. Not public: it arrives with the
	// platform's own identity like anything else.
	mux.HandleFunc("POST /jobs/digest", a.requireUser(a.digest))

	return a.logRequests(mux)
}

// requireUser refuses anything the platform has not attributed to a person.
func (a *app) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.cfg.identityReady() {
			a.fail(w, r, http.StatusServiceUnavailable, "No identity configured",
				"This installation cannot tell who is asking, so it will not serve its admin surface.")
			return
		}

		user := a.cfg.devUser
		if a.cfg.trustProxy {
			if named := strings.TrimSpace(r.Header.Get(userHeader)); named != "" {
				user = named
			}
		}
		if user == "" {
			a.fail(w, r, http.StatusUnauthorized, "Sign in", "This page belongs to an account.")
			return
		}
		next(w, r.WithContext(withUser(r.Context(), user, r.Header.Get(emailHeader))))
	}
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
	}
	if err := a.store.CreateForm(r.Context(), form); err != nil {
		// Back to the page that asked, with the reason, rather than a dead end.
		http.Redirect(w, r, "/?error="+urlSafe(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/forms/"+form.ID, http.StatusSeeOther)
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

// --------------------------------------------------------------- public

func (a *app) publicForm(w http.ResponseWriter, r *http.Request) {
	form, err := a.store.FormBySlug(r.Context(), r.PathValue("slug"))
	if errors.Is(err, ErrNotFound) {
		a.fail(w, r, http.StatusNotFound, "No such form", "Check the address.")
		return
	}
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	a.render(w, r, "public.html", map[string]any{
		"Title": form.Title,
		"Form":  form,
		"Error": r.URL.Query().Get("error"),
	})
}

func (a *app) submit(w http.ResponseWriter, r *http.Request) {
	form, err := a.store.FormBySlug(r.Context(), r.PathValue("slug"))
	if errors.Is(err, ErrNotFound) {
		a.fail(w, r, http.StatusNotFound, "No such form", "Check the address.")
		return
	}
	if err != nil {
		a.serverError(w, r, err)
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
