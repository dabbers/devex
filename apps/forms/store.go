package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound reports that a record does not exist.
var ErrNotFound = errors.New("forms: not found")

// Field is one item on a form: a question, a block of text, or the start of
// a new page. One ordered list rather than pages holding questions, so a form
// from before pages existed is simply a form with one page, and moving a
// question between pages is moving it in the list.
type Field struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	// Help is a line of guidance under a question, the body of a text block,
	// or a page's introduction.
	Help string `json:"help,omitempty"`
	// Options are a choice or dropdown's answers, in order.
	Options []string `json:"options,omitempty"`
	// ShowIf hides this item, or the whole page it starts, unless every
	// condition holds. Conditions may only look at questions before it.
	ShowIf []Condition `json:"show_if,omitempty"`
}

// Condition is one test of an earlier answer.
type Condition struct {
	Field string `json:"field"`
	Op    string `json:"op"`
	Value string `json:"value,omitempty"`
}

// fieldTypes are the question kinds a form may ask. Still deliberately few:
// a customer who needs another asks the agent for it.
var fieldTypes = map[string]bool{
	"text": true, "email": true, "textarea": true, "number": true, "checkbox": true,
	"choice": true, "dropdown": true, "date": true, "url": true,
}

// Items that ask nothing.
const (
	// TypePage starts a new page. Its label is the page's title.
	TypePage = "page"
	// TypeText is a heading with an optional paragraph, between questions.
	TypeText = "text-block"
)

// conditionOps are the tests a condition may make.
var conditionOps = map[string]bool{
	"equals": true, "not_equals": true, "contains": true, "answered": true, "not_answered": true,
}

// Limits on what one form may hold, so a form is something a person answers
// rather than a way to put a megabyte in every page view.
const (
	maxItems       = 300
	maxPages       = 50
	maxOptions     = 100
	maxLabel       = 300
	maxHelp        = 2000
	maxOptionLen   = 200
	maxDescription = 4000
	maxConditions  = 10
	maxAnswer      = 20000
)

// Audience is who may see and answer a form. Managing forms and reading what
// was sent is always for members, whatever the form's audience.
type Audience string

const (
	// AudiencePublic is anyone with the link.
	AudiencePublic Audience = "public"
	// AudienceInternal is this installation's members.
	AudienceInternal Audience = "internal"
)

func (a Audience) valid() bool { return a == AudiencePublic || a == AudienceInternal }

// Form is a questionnaire with an address and an audience.
type Form struct {
	ID          string    `json:"id"`
	Slug        string    `json:"slug"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Fields      []Field   `json:"fields"`
	Audience    Audience  `json:"audience"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Submission is one person's answers.
type Submission struct {
	ID        string            `json:"id"`
	FormID    string            `json:"form_id"`
	Data      map[string]string `json:"data"`
	CreatedAt time.Time         `json:"created_at"`
}

// Store is the app's data, in the Postgres the platform provisioned.
type Store struct{ db *sql.DB }

// migrations run in order, forward only, each recorded once. Forward only
// because a down migration against live customer data is a restore wearing a
// disguise, and the platform already snapshots before it promotes.
//
// Core owns these tables. An installation's own additions live behind the
// manifest's schema_prefix, so a core migration can never collide with one an
// agent wrote.
var migrations = []struct {
	name string
	sql  string
}{
	{"0001_forms", `
		CREATE TABLE forms (
			id          TEXT PRIMARY KEY,
			slug        TEXT NOT NULL UNIQUE,
			title       TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			fields      JSONB NOT NULL DEFAULT '[]'::jsonb,
			created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`},
	{"0002_submissions", `
		CREATE TABLE submissions (
			id         TEXT PRIMARY KEY,
			form_id    TEXT NOT NULL REFERENCES forms(id) ON DELETE CASCADE,
			data       JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`},
	{"0003_submissions_by_form", `
		CREATE INDEX submissions_by_form ON submissions (form_id, created_at DESC, id DESC)`},
	// Forms from before audiences were open to anyone with the link and stay
	// that way. A row written since without saying is for members.
	{"0004_form_audience", `
		ALTER TABLE forms ADD COLUMN audience TEXT NOT NULL DEFAULT 'public'
			CHECK (audience IN ('public', 'internal'));
		ALTER TABLE forms ALTER COLUMN audience SET DEFAULT 'internal'`},
}

// Migrate brings the schema up to date.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("forms: migration table: %w", err)
	}

	for _, migration := range migrations {
		var applied bool
		if err := s.db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`,
			migration.name).Scan(&applied); err != nil {
			return fmt.Errorf("forms: check migration %s: %w", migration.name, err)
		}
		if applied {
			continue
		}

		// Each migration in its own transaction: a failure leaves the ones
		// before it applied and recorded, so a re-run resumes rather than
		// starting over against a half-changed schema.
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migration.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("forms: migration %s: %w", migration.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (name) VALUES ($1)`, migration.name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("forms: record migration %s: %w", migration.name, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Ready reports whether the app can actually serve.
//
// A service that answers 200 before it can reach its database is worse than
// one that answers nothing, because the platform will route traffic to it.
func (s *Store) Ready(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return s.db.PingContext(ctx)
}

// CreateForm stores a new form.
func (s *Store) CreateForm(ctx context.Context, form *Form) error {
	if err := form.normalise(); err != nil {
		return err
	}
	if form.ID == "" {
		form.ID = newID("frm")
	}

	encoded, err := json.Marshal(form.Fields)
	if err != nil {
		return err
	}
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO forms (id, slug, title, description, fields, audience)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at, updated_at`,
		form.ID, form.Slug, form.Title, form.Description, encoded, form.Audience,
	).Scan(&form.CreatedAt, &form.UpdatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "forms_slug_key") {
			return fmt.Errorf("forms: a form already answers at /f/%s", form.Slug)
		}
		return fmt.Errorf("forms: create form: %w", err)
	}
	return nil
}

// UpdateForm replaces a form's title, description and items. Its address,
// audience and answers stay as they are: answers are kept by question key, so
// a question that keeps its key keeps its answers, and one that is removed
// still has them in the export.
func (s *Store) UpdateForm(ctx context.Context, form *Form) error {
	current, err := s.FormByID(ctx, form.ID)
	if err != nil {
		return err
	}
	form.Slug, form.Audience = current.Slug, current.Audience
	if err := form.normalise(); err != nil {
		return err
	}
	encoded, err := json.Marshal(form.Fields)
	if err != nil {
		return err
	}
	return s.db.QueryRowContext(ctx, `
		UPDATE forms SET title = $2, description = $3, fields = $4, updated_at = now()
		WHERE id = $1 RETURNING created_at, updated_at`,
		form.ID, form.Title, form.Description, encoded).Scan(&form.CreatedAt, &form.UpdatedAt)
}

// SetAudience changes who a form is for.
func (s *Store) SetAudience(ctx context.Context, id string, audience Audience) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE forms SET audience = $2, updated_at = now() WHERE id = $1`, id, audience)
	if err != nil {
		return fmt.Errorf("forms: set audience: %w", err)
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("forms: set audience: %w", err)
	}
	if changed == 0 {
		return ErrNotFound
	}
	return nil
}

// FormBySlug loads the form published at an address.
func (s *Store) FormBySlug(ctx context.Context, slug string) (*Form, error) {
	return s.form(ctx, `SELECT id, slug, title, description, fields, audience, created_at, updated_at
		FROM forms WHERE slug = $1`, slug)
}

// FormByID loads one form.
func (s *Store) FormByID(ctx context.Context, id string) (*Form, error) {
	return s.form(ctx, `SELECT id, slug, title, description, fields, audience, created_at, updated_at
		FROM forms WHERE id = $1`, id)
}

func (s *Store) form(ctx context.Context, query string, arg any) (*Form, error) {
	var (
		form   Form
		fields []byte
	)
	err := s.db.QueryRowContext(ctx, query, arg).Scan(
		&form.ID, &form.Slug, &form.Title, &form.Description, &fields, &form.Audience,
		&form.CreatedAt, &form.UpdatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("forms: load form: %w", err)
	}
	if err := json.Unmarshal(fields, &form.Fields); err != nil {
		return nil, fmt.Errorf("forms: unreadable fields on %s: %w", form.ID, err)
	}
	return &form, nil
}

// ListForms returns every form, newest first.
func (s *Store) ListForms(ctx context.Context) ([]*Form, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, slug, title, description, fields, audience, created_at, updated_at
		FROM forms ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("forms: list forms: %w", err)
	}
	defer rows.Close()

	forms := make([]*Form, 0, 8)
	for rows.Next() {
		var (
			form   Form
			fields []byte
		)
		if err := rows.Scan(&form.ID, &form.Slug, &form.Title, &form.Description,
			&fields, &form.Audience, &form.CreatedAt, &form.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(fields, &form.Fields); err != nil {
			return nil, err
		}
		forms = append(forms, &form)
	}
	return forms, rows.Err()
}

// CountSubmissions reports how many answers each form has, keyed by form id.
func (s *Store) CountSubmissions(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT form_id, count(*) FROM submissions GROUP BY form_id`)
	if err != nil {
		return nil, fmt.Errorf("forms: count submissions: %w", err)
	}
	defer rows.Close()

	counts := map[string]int{}
	for rows.Next() {
		var (
			id    string
			count int
		)
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		counts[id] = count
	}
	return counts, rows.Err()
}

// Submit records one person's answers.
func (s *Store) Submit(ctx context.Context, submission *Submission) error {
	if submission.ID == "" {
		submission.ID = newID("sub")
	}
	if submission.Data == nil {
		submission.Data = map[string]string{}
	}

	encoded, err := json.Marshal(submission.Data)
	if err != nil {
		return err
	}
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO submissions (id, form_id, data) VALUES ($1, $2, $3)
		RETURNING created_at`,
		submission.ID, submission.FormID, encoded).Scan(&submission.CreatedAt)
	if err != nil {
		return fmt.Errorf("forms: record submission: %w", err)
	}
	return nil
}

// ListSubmissions returns a form's answers, newest first.
func (s *Store) ListSubmissions(ctx context.Context, formID string, limit int) ([]*Submission, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, form_id, data, created_at FROM submissions
		WHERE form_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2`, formID, limit)
	if err != nil {
		return nil, fmt.Errorf("forms: list submissions: %w", err)
	}
	defer rows.Close()

	submissions := make([]*Submission, 0, limit)
	for rows.Next() {
		var (
			submission Submission
			data       []byte
		)
		if err := rows.Scan(&submission.ID, &submission.FormID, &data, &submission.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &submission.Data); err != nil {
			return nil, err
		}
		submissions = append(submissions, &submission)
	}
	return submissions, rows.Err()
}

// normalise checks a form and fills what it can.
func (f *Form) normalise() error {
	f.Slug = strings.ToLower(strings.TrimSpace(f.Slug))
	f.Title = strings.TrimSpace(f.Title)
	f.Description = strings.TrimSpace(f.Description)

	if f.Audience == "" {
		// Unsaid is private: the mistake that costs nothing.
		f.Audience = AudienceInternal
	}
	if !f.Audience.valid() {
		return fmt.Errorf("forms: a form is %q or %q, not %q", AudiencePublic, AudienceInternal, f.Audience)
	}
	if f.Title == "" {
		return errors.New("forms: a form needs a title")
	}
	if f.Slug == "" {
		f.Slug = slugify(f.Title)
	}
	if !slugOK(f.Slug) {
		return fmt.Errorf("forms: %q is not a usable address; use letters, numbers and hyphens", f.Slug)
	}
	if len([]rune(f.Title)) > maxLabel || len([]rune(f.Description)) > maxDescription {
		return errors.New("forms: the title or description is too long")
	}
	return normaliseFields(f.Fields)
}

// normaliseFields checks a form's items and fills in what it can: keys from
// labels, the default type, tidy options.
func normaliseFields(fields []Field) error { return checkFields(fields, true) }

// checkFields is normaliseFields, optionally allowing a form with no
// questions: an archive may hold one that predates the rule, and restoring it
// as it was beats refusing the whole archive.
func checkFields(fields []Field, needQuestion bool) error {
	if len(fields) > maxItems {
		return fmt.Errorf("forms: a form may have at most %d items", maxItems)
	}
	questions, pages := 0, 1
	// Each question seen so far, by key. A condition may only look back,
	// which keeps every form free of cycles and answerable in order.
	earlier := map[string]*Field{}
	seen := map[string]bool{}
	for i := range fields {
		field := &fields[i]
		field.Label = strings.TrimSpace(field.Label)
		field.Help = strings.TrimSpace(field.Help)
		if field.Type == "" {
			field.Type = "text"
		}
		asks := fieldTypes[field.Type]
		if !asks && field.Type != TypePage && field.Type != TypeText {
			return fmt.Errorf("forms: %q is not a question type this app knows", field.Type)
		}
		if field.Label == "" && field.Type != TypePage {
			return errors.New("forms: every question and heading needs a label")
		}
		if len([]rune(field.Label)) > maxLabel || len([]rune(field.Help)) > maxHelp {
			return fmt.Errorf("forms: %q is too long", truncate(field.Label, 40))
		}
		if field.Key == "" {
			base := slugify(field.Label)
			if base == "" || !asks {
				base = fmt.Sprintf("%s-%d", strings.TrimSuffix(field.Type, "-block"), i+1)
			}
			if len(base) > 56 {
				base = strings.Trim(base[:56], "-")
			}
			field.Key = base
			for n := 2; seen[field.Key]; n++ {
				field.Key = fmt.Sprintf("%s-%d", base, n)
			}
		}
		if !slugOK(field.Key) {
			return fmt.Errorf("forms: %q is not a usable field key", field.Key)
		}
		if seen[field.Key] {
			return fmt.Errorf("forms: two questions share the key %q, so one would overwrite the other", field.Key)
		}
		seen[field.Key] = true

		if field.Type == TypePage {
			pages++
			if pages > maxPages {
				return fmt.Errorf("forms: a form may have at most %d pages", maxPages)
			}
		}
		if !asks {
			field.Required = false
		}

		if field.Type == "choice" || field.Type == "dropdown" {
			var kept []string
			distinct := map[string]bool{}
			for _, option := range field.Options {
				option = strings.TrimSpace(option)
				if option == "" || distinct[option] {
					continue
				}
				if len([]rune(option)) > maxOptionLen {
					return fmt.Errorf("forms: an option of %q is too long", truncate(field.Label, 40))
				}
				distinct[option] = true
				kept = append(kept, option)
			}
			if len(kept) == 0 || len(kept) > maxOptions {
				return fmt.Errorf("forms: %q needs between 1 and %d options", truncate(field.Label, 40), maxOptions)
			}
			field.Options = kept
		} else {
			field.Options = nil
		}

		if len(field.ShowIf) > maxConditions {
			return fmt.Errorf("forms: %q has more than %d conditions", truncate(field.Label, 40), maxConditions)
		}
		for j := range field.ShowIf {
			c := &field.ShowIf[j]
			c.Value = strings.TrimSpace(c.Value)
			if earlier[c.Field] == nil {
				return fmt.Errorf("forms: %q depends on %q, which is not a question before it",
					truncate(nonEmpty(field.Label, "a page"), 40), c.Field)
			}
			if !conditionOps[c.Op] {
				return fmt.Errorf("forms: %q is not a condition this app knows", c.Op)
			}
			if len([]rune(c.Value)) > maxOptionLen {
				return errors.New("forms: a condition's value is too long")
			}
		}

		if asks {
			questions++
			earlier[field.Key] = field
		}
	}
	if questions == 0 && needQuestion {
		return errors.New("forms: a form with no questions would collect nothing")
	}
	return nil
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func nonEmpty(s, otherwise string) string {
	if s == "" {
		return otherwise
	}
	return s
}

func slugOK(s string) bool {
	if s == "" || len(s) > 64 || strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func slugify(s string) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen && b.Len() > 0 {
				b.WriteRune('-')
				lastHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// idAlphabet is Crockford-ish base32: no padding, no vowels to accidentally
// spell anything, and sortable because the timestamp leads.
var idAlphabet = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

// newID returns a prefixed, time-ordered identifier. Sortable ids mean a
// listing can page on the primary key instead of an offset.
func newID(prefix string) string {
	var raw [14]byte
	binary.BigEndian.PutUint64(raw[:8], uint64(time.Now().UTC().UnixMilli()))
	if _, err := rand.Read(raw[8:]); err != nil {
		// Unreachable on any platform this runs on; a predictable id is worse
		// than a crash here, because ids end up in public URLs.
		panic("forms: no randomness available: " + err.Error())
	}
	return prefix + "_" + idAlphabet.EncodeToString(raw[2:])
}
