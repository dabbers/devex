package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// applicationForm is a three-page form whose second page and one question
// only appear for some answers.
func applicationForm() *Form {
	return &Form{
		Title:    "Apply",
		Audience: AudiencePublic,
		Fields: []Field{
			{Key: "name", Label: "Your name", Type: "text", Required: true},
			{Key: "role", Label: "Which role?", Type: "choice", Required: true,
				Options: []string{"Engineering", "Design", "Sales"}},
			{Key: "portfolio", Label: "Portfolio link", Type: "url", Required: true,
				ShowIf: []Condition{{Field: "role", Op: "equals", Value: "Design"}}},
			{Key: "tech", Type: TypePage, Label: "About your engineering",
				ShowIf: []Condition{{Field: "role", Op: "equals", Value: "Engineering"}}},
			{Key: "github", Label: "GitHub", Type: "url", Required: true},
			{Key: "languages", Label: "Languages", Type: "textarea"},
			{Key: "last", Type: TypePage, Label: "Last thing"},
			{Key: "intro", Type: TypeText, Label: "Nearly done", Help: "One more question."},
			{Key: "start", Label: "Earliest start date", Type: "date"},
			{Key: "agree", Label: "I agree to be contacted", Type: "checkbox", Required: true},
		},
	}
}

func values(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestTheFormHasPages: page breaks split a form; a form with none is one page.
func TestTheFormHasPages(t *testing.T) {
	form := applicationForm()
	if err := form.normalise(); err != nil {
		t.Fatal(err)
	}
	pages := form.Pages()
	if len(pages) != 3 || pages[1].Title() != "About your engineering" || len(pages[2].Items) != 3 {
		t.Fatalf("pages = %+v", pages)
	}
	if got := len(contactForm().Pages()); got != 1 {
		t.Fatalf("a form with no breaks has %d pages", got)
	}
	if got := len(form.Questions()); got != 7 {
		t.Fatalf("%d questions, want 7 (no page breaks or text blocks)", got)
	}
}

// TestOnlyWhatWasShownIsAskedOrKept: a hidden question is never required, and
// whatever a browser sends for it is not stored -- conditions are enforced
// here, not in the page's script.
func TestOnlyWhatWasShownIsAskedOrKept(t *testing.T) {
	form := applicationForm()
	if err := form.normalise(); err != nil {
		t.Fatal(err)
	}

	// A salesperson: no portfolio, no engineering page.
	data, err := form.Collect(values(map[string]string{
		"name": "Ada", "role": "Sales", "agree": "yes",
		"portfolio": "https://sneaked.in", "github": "https://github.com/sneaked",
	}))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, hidden := range []string{"portfolio", "github", "languages"} {
		if _, kept := data[hidden]; kept {
			t.Errorf("%s was stored although the respondent was never asked it", hidden)
		}
	}
	if data["role"] != "Sales" || data["start"] != "" {
		t.Errorf("data = %v", data)
	}

	// An engineer must answer the engineering page.
	_, err = form.Collect(values(map[string]string{"name": "Ada", "role": "Engineering", "agree": "yes"}))
	var problem *AnswerError
	if !errors.As(err, &problem) || problem.Field != "github" {
		t.Fatalf("an engineer skipped a required question on their page: %v", err)
	}

	// A designer must give a portfolio, and a valid one.
	_, err = form.Collect(values(map[string]string{"name": "Ada", "role": "Design", "agree": "yes", "portfolio": "javascript:alert(1)"}))
	if !errors.As(err, &problem) || problem.Field != "portfolio" {
		t.Fatalf("a portfolio that is not a web address was accepted: %v", err)
	}
}

func TestAnswersMustFitTheirQuestion(t *testing.T) {
	form := applicationForm()
	if err := form.normalise(); err != nil {
		t.Fatal(err)
	}
	base := map[string]string{"name": "Ada", "role": "Sales", "agree": "yes"}
	for key, bad := range map[string]string{
		"role": "CEO", "start": "next week", "agree": "sure", "name": "two\nlines",
	} {
		answers := map[string]string{}
		for k, v := range base {
			answers[k] = v
		}
		answers[key] = bad
		if _, err := form.Collect(values(answers)); err == nil {
			t.Errorf("%s = %q was accepted", key, bad)
		}
	}
	answers := map[string]string{"name": strings.Repeat("a", maxAnswer+1), "role": "Sales", "agree": "yes"}
	if _, err := form.Collect(values(answers)); err == nil {
		t.Error("an enormous answer was accepted")
	}
}

// TestAConditionOnlyLooksBack: a condition on a question that comes later, or
// does not exist, or is not a question, would make a form that cannot be
// answered in order; it is refused when the form is saved.
func TestAConditionOnlyLooksBack(t *testing.T) {
	for name, fields := range map[string][]Field{
		"forwards": {
			{Key: "a", Label: "A", ShowIf: []Condition{{Field: "b", Op: "answered"}}},
			{Key: "b", Label: "B"},
		},
		"itself": {{Key: "a", Label: "A", ShowIf: []Condition{{Field: "a", Op: "answered"}}}},
		"a heading": {
			{Key: "h", Label: "H", Type: TypeText},
			{Key: "a", Label: "A", ShowIf: []Condition{{Field: "h", Op: "answered"}}},
		},
		"an unknown test": {
			{Key: "a", Label: "A"},
			{Key: "b", Label: "B", ShowIf: []Condition{{Field: "a", Op: "matches-regex", Value: ".*"}}},
		},
		"a choice with no choices": {{Key: "a", Label: "A", Type: "choice"}},
		"only a heading":           {{Key: "h", Label: "H", Type: TypeText}},
		"a type it does not know":  {{Key: "a", Label: "A", Type: "file"}},
	} {
		if err := normaliseFields(fields); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestTheBuilderSavesWhatTheServerAccepts: the builder's save is checked like
// any other input, can create and edit, and keeps answers by key.
func TestTheBuilderSavesWhatTheServerAccepts(t *testing.T) {
	built, store := testApp(t)
	server := httptest.NewServer(built.handler())
	defer server.Close()

	save := func(body any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", server.URL+"/forms/save", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	status, out := save(applicationForm())
	if status != http.StatusOK {
		t.Fatalf("creating from the builder: %d %v", status, out)
	}
	id, _ := out["id"].(string)
	form, err := store.FormByID(context.Background(), id)
	if err != nil || form.Slug != "apply" || len(form.Pages()) != 3 {
		t.Fatalf("created %+v (%v)", form, err)
	}

	// A respondent through the real handler: an engineer, sending a
	// portfolio they were never asked for.
	resp, err := http.PostForm(server.URL+"/f/apply", url.Values{
		"name": {"Ada"}, "role": {"Engineering"}, "github": {"https://github.com/ada"},
		"agree": {"yes"}, "portfolio": {"https://sneaked.in"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("submitting = %d", resp.StatusCode)
	}
	stored, _ := store.ListSubmissions(context.Background(), id, 10)
	if len(stored) != 1 || stored[0].Data["github"] != "https://github.com/ada" {
		t.Fatalf("stored %v", stored)
	}
	if _, kept := stored[0].Data["portfolio"]; kept {
		t.Fatal("an answer to a hidden question was stored")
	}

	// Editing keeps the address, the audience and the answers.
	form.Title = "Apply to Acme"
	form.Slug, form.Audience = "renamed", AudienceInternal
	form.Fields = append(form.Fields, Field{Key: "referral", Label: "How did you hear of us?", Type: "dropdown",
		Options: []string{"A friend", "Online"}})
	if status, out := save(form); status != http.StatusOK {
		t.Fatalf("editing: %d %v", status, out)
	}
	again, _ := store.FormByID(context.Background(), id)
	if again.Title != "Apply to Acme" || again.Slug != "apply" || again.Audience != AudiencePublic || len(again.Questions()) != 8 {
		t.Fatalf("after editing: %+v", again)
	}
	if kept, _ := store.ListSubmissions(context.Background(), id, 10); len(kept) != 1 {
		t.Fatal("editing the form lost its answers")
	}

	// What the server refuses, the builder is told in words.
	for name, body := range map[string]any{
		"a forward condition": &Form{Title: "x", Fields: []Field{
			{Key: "a", Label: "A", ShowIf: []Condition{{Field: "b", Op: "answered"}}}, {Key: "b", Label: "B"}}},
		"a field the builder does not send": map[string]any{"title": "x", "fields": []any{}, "script": "alert(1)"},
	} {
		if status, out := save(body); status == http.StatusOK || out["error"] == "" {
			t.Errorf("%s: %d %v", name, status, out)
		}
	}
}

// TestTheBuilderAndPagesAreSafeToServe: every page says what it may load, and
// a form's own text never becomes markup.
func TestTheBuilderAndPagesAreSafeToServe(t *testing.T) {
	built, store := testApp(t)
	form := &Form{Title: `<img src=x onerror=alert(1)>`, Audience: AudiencePublic, Fields: []Field{
		{Key: "q", Label: `</script><script>alert(1)</script>`, Type: "choice", Options: []string{`"><b>x`}},
	}}
	if err := store.CreateForm(context.Background(), form); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(built.handler())
	defer server.Close()
	for _, path := range []string{"/f/" + form.Slug, "/forms/" + form.ID + "/edit", "/forms/new"} {
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d", path, resp.StatusCode)
		}
		if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") {
			t.Errorf("%s has no content policy", path)
		}
		for _, bad := range []string{"<img src=x", "<script>alert", `"><b>`} {
			if strings.Contains(string(body), bad) {
				t.Errorf("%s drew %q as markup", path, bad)
			}
		}
	}
	for _, file := range []string{"/static/answer.js", "/static/builder.js", "/static/builder.css"} {
		resp, err := http.Get(server.URL + file)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d", file, resp.StatusCode)
		}
	}
}
