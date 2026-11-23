package main

import (
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Page is the items between two page breaks, as a respondent sees them.
type Page struct {
	// Index counts from zero.
	Index int
	// Break is the page-break item that starts this page, or nil for the first.
	Break *Field
	Items []Field
}

// Title is the page's own heading, if its break has one.
func (p Page) Title() string {
	if p.Break == nil {
		return ""
	}
	return p.Break.Label
}

// Pages splits a form's items at its page breaks.
func (f *Form) Pages() []Page {
	pages := []Page{{Index: 0}}
	for i := range f.Fields {
		field := f.Fields[i]
		if field.Type == TypePage {
			brk := field
			pages = append(pages, Page{Index: len(pages), Break: &brk})
			continue
		}
		last := &pages[len(pages)-1]
		last.Items = append(last.Items, field)
	}
	return pages
}

// Questions returns only the items that collect an answer, in order.
func (f *Form) Questions() []Field {
	var out []Field
	for _, field := range f.Fields {
		if fieldTypes[field.Type] {
			out = append(out, field)
		}
	}
	return out
}

// holds reports whether one condition is true of the answers.
func (c Condition) holds(answers map[string]string) bool {
	got := answers[c.Field]
	switch c.Op {
	case "equals":
		return strings.EqualFold(got, c.Value)
	case "not_equals":
		return !strings.EqualFold(got, c.Value)
	case "contains":
		return c.Value != "" && strings.Contains(strings.ToLower(got), strings.ToLower(c.Value))
	case "answered":
		return got != ""
	case "not_answered":
		return got == ""
	}
	// An operator this code does not know hides rather than shows: a
	// question nobody meant to ask is worse than one that is not asked.
	return false
}

func allHold(conditions []Condition, answers map[string]string) bool {
	for _, c := range conditions {
		if !c.holds(answers) {
			return false
		}
	}
	return true
}

// shown decides which items a respondent with these answers is asked. It
// walks the form in order, so a question hidden by an earlier answer counts
// as unanswered for anything after it -- the same as a person would see.
func (f *Form) shown(raw map[string]string) (visible map[string]bool, answers map[string]string) {
	visible = map[string]bool{}
	answers = map[string]string{}
	pageShown := true
	for _, field := range f.Fields {
		if field.Type == TypePage {
			pageShown = allHold(field.ShowIf, answers)
			visible[field.Key] = pageShown
			continue
		}
		show := pageShown && allHold(field.ShowIf, answers)
		visible[field.Key] = show
		if show && fieldTypes[field.Type] {
			answers[field.Key] = raw[field.Key]
		}
	}
	return visible, answers
}

// AnswerError is a problem with one answer, said so a respondent can fix it.
type AnswerError struct {
	Field   string
	Label   string
	Message string
}

func (e *AnswerError) Error() string { return e.Label + ": " + e.Message }

// Collect checks a respondent's answers against the form, applying its
// conditions, and returns exactly what to store: one value for each question
// they were shown, and nothing for any question they were not, whatever the
// browser sent.
func (f *Form) Collect(value func(key string) string) (map[string]string, error) {
	raw := map[string]string{}
	for _, field := range f.Questions() {
		v := strings.TrimSpace(strings.ReplaceAll(value(field.Key), "\r\n", "\n"))
		if len(v) > maxAnswer {
			return nil, &AnswerError{field.Key, field.Label, "is too long"}
		}
		raw[field.Key] = v
	}
	visible, answers := f.shown(raw)
	data := make(map[string]string, len(answers))
	for _, field := range f.Questions() {
		if !visible[field.Key] {
			continue
		}
		v := answers[field.Key]
		if v == "" {
			if field.Required {
				return nil, &AnswerError{field.Key, field.Label, "is required"}
			}
			data[field.Key] = ""
			continue
		}
		if msg := checkAnswer(field, v); msg != "" {
			return nil, &AnswerError{field.Key, field.Label, msg}
		}
		data[field.Key] = v
	}
	return data, nil
}

// checkAnswer says what is wrong with an answer of the right shape for its
// question, or "" when nothing is.
func checkAnswer(field Field, v string) string {
	switch field.Type {
	case "email":
		if addr, err := mail.ParseAddress(v); err != nil || addr.Address != v {
			return "is not an email address"
		}
	case "number":
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			return "is not a number"
		}
	case "date":
		if _, err := time.Parse("2006-01-02", v); err != nil {
			return "is not a date"
		}
	case "url":
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "is not a web address starting with http:// or https://"
		}
	case "checkbox":
		if v != "yes" {
			return "is not ticked or unticked"
		}
	case "choice", "dropdown":
		for _, option := range field.Options {
			if option == v {
				return ""
			}
		}
		return "is not one of the choices"
	case "text":
		if strings.Contains(v, "\n") {
			return "must be one line"
		}
	}
	return ""
}
