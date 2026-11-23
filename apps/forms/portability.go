package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Export and import, which are the same feature seen from two sides.
//
// A platform for rescuing people from SaaS must not become the thing they were
// escaping, and the usual lock-in is not a refusal to export -- it is an export
// that technically exists and does not quite work. So the contract holds this
// to a property that can be checked rather than promised: an app's export must
// be a valid import to that same app. The round trip is in the check suite, and
// it runs every release.

// exportFormat names the shape, so a reader years from now can tell what it is
// looking at and an importer can refuse what it does not understand.
const exportFormat = "dabberz.forms.export"

// exportVersion changes only when the shape changes incompatibly. 2 added each
// form's audience: an importer that dropped it would publish internal forms,
// so one that only knows 1 has to refuse. 3 added pages, choices and
// conditions: an importer that dropped conditions would ask everyone every
// question, and one that did not know the new types would reject the form.
const exportVersion = 3

// Archive is everything this installation holds.
type Archive struct {
	Format      string        `json:"format"`
	Version     int           `json:"version"`
	App         string        `json:"app"`
	ExportedAt  time.Time     `json:"exported_at"`
	Forms       []*Form       `json:"forms"`
	Submissions []*Submission `json:"submissions"`
}

// Export gathers the whole installation.
func (s *Store) Export(ctx context.Context) (*Archive, error) {
	forms, err := s.ListForms(ctx)
	if err != nil {
		return nil, err
	}

	archive := &Archive{
		Format:     exportFormat,
		Version:    exportVersion,
		App:        "forms",
		ExportedAt: time.Now().UTC(),
		Forms:      forms,
		// Never nil: an empty installation exports an empty list, not null,
		// so an importer does not have to special-case the first customer.
		Submissions: []*Submission{},
	}
	for _, form := range forms {
		// No limit: an export that silently stops at two hundred rows is the
		// kind of "technically you can export" this exists to prevent.
		submissions, err := s.allSubmissions(ctx, form.ID)
		if err != nil {
			return nil, err
		}
		archive.Submissions = append(archive.Submissions, submissions...)
	}
	return archive, nil
}

// allSubmissions pages through a form's answers.
func (s *Store) allSubmissions(ctx context.Context, formID string) ([]*Submission, error) {
	var (
		all   []*Submission
		after string
	)
	for {
		rows, err := s.db.QueryContext(ctx, `
			SELECT id, form_id, data, created_at FROM submissions
			WHERE form_id = $1 AND ($2 = '' OR id > $2)
			ORDER BY id LIMIT 1000`, formID, after)
		if err != nil {
			return nil, fmt.Errorf("forms: export submissions: %w", err)
		}

		page := 0
		for rows.Next() {
			var (
				submission Submission
				data       []byte
			)
			if err := rows.Scan(&submission.ID, &submission.FormID, &data, &submission.CreatedAt); err != nil {
				rows.Close()
				return nil, err
			}
			if err := json.Unmarshal(data, &submission.Data); err != nil {
				rows.Close()
				return nil, err
			}
			all = append(all, &submission)
			after = submission.ID
			page++
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if page == 0 {
			return all, nil
		}
	}
}

// Import restores an archive into an empty installation.
//
// Deliberately only into an empty one. Merging somebody's export into data
// that already exists means deciding what wins, and quietly picking is how an
// import eats a week of submissions.
func (s *Store) Import(ctx context.Context, archive *Archive) error {
	if archive.Format != exportFormat {
		return fmt.Errorf("forms: this is a %q archive, not %q", archive.Format, exportFormat)
	}
	if archive.Version > exportVersion {
		return fmt.Errorf("forms: archive version %d is newer than this app understands (%d)",
			archive.Version, exportVersion)
	}

	for _, form := range archive.Forms {
		if form == nil {
			return fmt.Errorf("forms: the archive holds an empty form")
		}
		if archive.Version < 2 && form.Audience == "" {
			// Version 1 was written when every form was open to anyone.
			form.Audience = AudiencePublic
		}
		if !form.Audience.valid() {
			return fmt.Errorf("forms: %s does not say who it is for", form.Slug)
		}
		// Checked as if typed in: an archive is input like any other,
		// and a condition pointing forwards would make a form that cannot
		// be answered in order.
		if err := checkFields(form.Fields, false); err != nil {
			return fmt.Errorf("forms: %s: %w", form.Slug, err)
		}
	}

	var existing int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM forms`).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return fmt.Errorf("forms: this installation already holds %d form(s); import restores into an empty one", existing)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // a committed transaction rolls back to nothing

	for _, form := range archive.Forms {
		fields, err := json.Marshal(form.Fields)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO forms (id, slug, title, description, fields, audience, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			form.ID, form.Slug, form.Title, form.Description, fields, form.Audience,
			form.CreatedAt, form.UpdatedAt); err != nil {
			return fmt.Errorf("forms: import form %s: %w", form.Slug, err)
		}
	}
	for _, submission := range archive.Submissions {
		data, err := json.Marshal(submission.Data)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO submissions (id, form_id, data, created_at) VALUES ($1, $2, $3, $4)`,
			submission.ID, submission.FormID, data, submission.CreatedAt); err != nil {
			return fmt.Errorf("forms: import submission %s: %w", submission.ID, err)
		}
	}
	return tx.Commit()
}

// --------------------------------------------------------------- handlers

func (a *app) export(w http.ResponseWriter, r *http.Request) {
	archive, err := a.store.Export(r.Context())
	if err != nil {
		a.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition",
		`attachment; filename="forms-`+archive.ExportedAt.Format("20060102")+`.json"`)

	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(archive); err != nil {
		a.logger.Error("export", "error", err)
	}
}

// importLimit bounds an uploaded archive. Generous, because it is somebody's
// whole installation, and bounded, because this reads it into memory.
const importLimit = 256 << 20

func (a *app) importData(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, importLimit))
	if err != nil {
		a.fail(w, r, http.StatusBadRequest, "That upload could not be read", err.Error())
		return
	}

	var archive Archive
	if err := json.Unmarshal(raw, &archive); err != nil {
		a.fail(w, r, http.StatusBadRequest, "That is not an archive", err.Error())
		return
	}
	if err := a.store.Import(r.Context(), &archive); err != nil {
		a.fail(w, r, http.StatusConflict, "That archive was not imported", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"imported_forms":       len(archive.Forms),
		"imported_submissions": len(archive.Submissions),
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func urlSafe(s string) string { return url.QueryEscape(s) }
