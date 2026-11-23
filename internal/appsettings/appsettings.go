// Package appsettings reads the settings an app says its owner may change, and
// checks the values chosen for them.
//
// An app declares them itself, in config/settings.json in its own code, and
// its agent changes that file as the app changes -- a new section, a new
// toggle -- through the same review as any other change. The panel does not
// know what any app's settings are. It draws whatever the declaration says,
// from a fixed catalog of components, and that catalog is the security
// boundary: a declaration is data, never markup. There is no component that
// takes HTML, no attribute that becomes a URL the panel navigates to, no
// style that is passed through, and every string is drawn as text. The shape
// follows A2UI's -- a flat list of components that refer to each other by ID,
// a root, values bound by path -- so moving to A2UI itself later is a
// translation, not a redesign.
package appsettings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

// File is where an app keeps its declaration, relative to its root.
const File = "config/settings.json"

// MaxBytes bounds a declaration.
const MaxBytes = 64 << 10

// The catalog. Nothing outside it is drawn.
const (
	TypeSection  = "section"  // a titled group of other components
	TypeNote     = "note"     // a paragraph of plain text
	TypeText     = "text"     // one line of text
	TypeTextarea = "textarea" // several lines of text
	TypeNumber   = "number"
	TypeToggle   = "toggle"
	TypeSelect   = "select"
	TypeColor    = "color" // #rrggbb
	TypeImage    = "image" // a picture from the library, by file ID
)

var valueTypes = map[string]bool{
	TypeText: true, TypeTextarea: true, TypeNumber: true, TypeToggle: true,
	TypeSelect: true, TypeColor: true, TypeImage: true,
}

// Option is one choice of a select.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Component is one entry in the catalog, as declared.
type Component struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"`
	Title       string   `json:"title,omitempty"`
	Label       string   `json:"label,omitempty"`
	Description string   `json:"description,omitempty"`
	Text        string   `json:"text,omitempty"`
	Children    []string `json:"children,omitempty"`
	Path        string   `json:"path,omitempty"`
	Default     any      `json:"default,omitempty"`
	Required    bool     `json:"required,omitempty"`
	MaxLength   int      `json:"maxLength,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	Step        *float64 `json:"step,omitempty"`
	Options     []Option `json:"options,omitempty"`
}

// Spec is an app's whole declaration.
type Spec struct {
	Version    int         `json:"version"`
	Title      string      `json:"title,omitempty"`
	Root       []string    `json:"root"`
	Components []Component `json:"components"`

	byID   map[string]*Component
	byPath map[string]*Component
}

var (
	idPattern    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
	pathPattern  = regexp.MustCompile(`^(/[a-z][a-z0-9_]{0,39}){1,4}$`)
	colorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
	filePattern  = regexp.MustCompile(`^lib_[0-9a-z]{26}$`)
)

const (
	maxComponents   = 150
	maxLabel        = 80
	maxDescription  = 600
	maxOptions      = 50
	defaultMaxText  = 200
	defaultMaxLines = 4000
	maxTextLimit    = 10000
	maxDepth        = 4
)

// Parse reads and checks a declaration. Unknown fields and unknown component
// types are refused rather than ignored, so a declaration cannot carry
// something the panel would silently drop today and draw tomorrow.
func Parse(raw []byte) (*Spec, error) {
	if len(raw) > MaxBytes {
		return nil, fmt.Errorf("settings: the declaration is larger than %d KiB", MaxBytes>>10)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var s Spec
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("settings: %w", err)
	}
	if dec.More() {
		return nil, errors.New("settings: more than one JSON value")
	}
	if err := s.check(); err != nil {
		return nil, err
	}
	return &s, nil
}

func plain(field, value string, limit int, lines bool) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not UTF-8", field)
	}
	if utf8.RuneCountInString(value) > limit {
		return fmt.Errorf("%s is longer than %d characters", field, limit)
	}
	for _, r := range value {
		if r == '\n' && lines {
			continue
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return fmt.Errorf("%s contains a control character", field)
		}
	}
	return nil
}

func (s *Spec) check() error {
	if s.Version != 1 {
		return fmt.Errorf("settings: version must be 1, not %d", s.Version)
	}
	if err := plain("title", s.Title, maxLabel, false); err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	if len(s.Components) > maxComponents {
		return fmt.Errorf("settings: more than %d components", maxComponents)
	}
	s.byID = map[string]*Component{}
	s.byPath = map[string]*Component{}
	for i := range s.Components {
		c := &s.Components[i]
		if err := c.check(); err != nil {
			return fmt.Errorf("settings: component %q: %w", c.ID, err)
		}
		if s.byID[c.ID] != nil {
			return fmt.Errorf("settings: two components are called %q", c.ID)
		}
		s.byID[c.ID] = c
		if c.Path != "" {
			if s.byPath[c.Path] != nil {
				return fmt.Errorf("settings: two settings are stored at %s", c.Path)
			}
			s.byPath[c.Path] = c
		}
	}

	// Every component is reached exactly once from the root, so the panel
	// draws a tree: no cycles, nothing orphaned, nothing drawn twice.
	seen := map[string]bool{}
	var walk func(ids []string, depth int) error
	walk = func(ids []string, depth int) error {
		if depth > maxDepth {
			return fmt.Errorf("settings: sections nest deeper than %d", maxDepth)
		}
		for _, cid := range ids {
			c := s.byID[cid]
			if c == nil {
				return fmt.Errorf("settings: %q is used but never declared", cid)
			}
			if seen[cid] {
				return fmt.Errorf("settings: %q is used more than once", cid)
			}
			seen[cid] = true
			if err := walk(c.Children, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if len(s.Root) == 0 {
		return errors.New("settings: root lists nothing")
	}
	if err := walk(s.Root, 0); err != nil {
		return err
	}
	for _, c := range s.Components {
		if !seen[c.ID] {
			return fmt.Errorf("settings: %q is declared but never used", c.ID)
		}
	}
	return nil
}

func (c *Component) check() error {
	if !idPattern.MatchString(c.ID) {
		return errors.New("an ID is lowercase letters, digits, - and _, starting with a letter")
	}
	for field, value := range map[string]string{"title": c.Title, "label": c.Label} {
		if err := plain(field, value, maxLabel, false); err != nil {
			return err
		}
	}
	for field, value := range map[string]string{"description": c.Description, "text": c.Text} {
		if err := plain(field, value, maxDescription, true); err != nil {
			return err
		}
	}

	isValue := valueTypes[c.Type]
	switch {
	case c.Type == TypeSection:
		if c.Title == "" {
			return errors.New("a section needs a title")
		}
	case c.Type == TypeNote:
		if c.Text == "" {
			return errors.New("a note needs text")
		}
	case isValue:
		if c.Label == "" {
			return errors.New("a setting needs a label")
		}
	default:
		return fmt.Errorf("%q is not in the catalog", c.Type)
	}
	if c.Type != TypeSection && len(c.Children) > 0 {
		return errors.New("only a section has children")
	}
	if isValue != (c.Path != "") {
		if isValue {
			return errors.New("a setting needs a path")
		}
		return errors.New("only a setting has a path")
	}
	if isValue && !pathPattern.MatchString(c.Path) {
		return fmt.Errorf("%q is not a path: up to four /parts of lowercase letters, digits and _", c.Path)
	}

	if c.MaxLength < 0 || c.MaxLength > maxTextLimit {
		return fmt.Errorf("maxLength must be between 0 and %d", maxTextLimit)
	}
	if c.MaxLength != 0 && c.Type != TypeText && c.Type != TypeTextarea {
		return errors.New("only text has a maxLength")
	}
	if (c.Min != nil || c.Max != nil || c.Step != nil) && c.Type != TypeNumber {
		return errors.New("only a number has min, max and step")
	}
	for _, n := range []*float64{c.Min, c.Max, c.Step} {
		if n != nil && (math.IsNaN(*n) || math.IsInf(*n, 0)) {
			return errors.New("min, max and step must be finite")
		}
	}
	if c.Min != nil && c.Max != nil && *c.Min > *c.Max {
		return errors.New("min is above max")
	}
	if c.Type == TypeSelect {
		if len(c.Options) == 0 || len(c.Options) > maxOptions {
			return fmt.Errorf("a select needs 1 to %d options", maxOptions)
		}
		values := map[string]bool{}
		for _, o := range c.Options {
			if err := plain("option value", o.Value, maxLabel, false); err != nil {
				return err
			}
			if err := plain("option label", o.Label, maxLabel, false); err != nil {
				return err
			}
			if o.Value == "" || o.Label == "" || values[o.Value] {
				return errors.New("each option needs its own value and a label")
			}
			values[o.Value] = true
		}
	} else if len(c.Options) > 0 {
		return errors.New("only a select has options")
	}
	if c.Default != nil {
		if c.Type == TypeImage {
			return errors.New("an image has no default: the app shows its own until one is chosen")
		}
		if _, err := c.normalise(c.Default); err != nil {
			return fmt.Errorf("default: %w", err)
		}
	}
	return nil
}

// normalise checks one value against its setting and returns it in the form
// it is kept in.
func (c *Component) normalise(v any) (any, error) {
	switch c.Type {
	case TypeText, TypeTextarea:
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("must be text")
		}
		limit := c.MaxLength
		if limit == 0 {
			limit = defaultMaxText
			if c.Type == TypeTextarea {
				limit = defaultMaxLines
			}
		}
		s = strings.ReplaceAll(s, "\r\n", "\n")
		if err := plain("the text", s, limit, c.Type == TypeTextarea); err != nil {
			return nil, err
		}
		return s, nil
	case TypeNumber:
		n, ok := v.(float64)
		if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, errors.New("must be a number")
		}
		if c.Min != nil && n < *c.Min {
			return nil, fmt.Errorf("must be at least %v", *c.Min)
		}
		if c.Max != nil && n > *c.Max {
			return nil, fmt.Errorf("must be at most %v", *c.Max)
		}
		return n, nil
	case TypeToggle:
		b, ok := v.(bool)
		if !ok {
			return nil, errors.New("must be on or off")
		}
		return b, nil
	case TypeSelect:
		s, ok := v.(string)
		if !ok {
			return nil, errors.New("must be one of the options")
		}
		for _, o := range c.Options {
			if o.Value == s {
				return s, nil
			}
		}
		return nil, errors.New("must be one of the options")
	case TypeColor:
		s, ok := v.(string)
		if !ok || !colorPattern.MatchString(s) {
			return nil, errors.New("must be a colour like #1d2b4f")
		}
		return strings.ToLower(s), nil
	case TypeImage:
		s, ok := v.(string)
		if !ok || !filePattern.MatchString(s) {
			return nil, errors.New("must be a picture from the library")
		}
		return s, nil
	}
	return nil, fmt.Errorf("%s holds no value", c.Type)
}

// Setting returns the setting stored at a path.
func (s *Spec) Setting(path string) *Component { return s.byPath[path] }

// Settings returns every setting, in declaration order.
func (s *Spec) Settings() []*Component {
	var out []*Component
	for i := range s.Components {
		if s.Components[i].Path != "" {
			out = append(out, &s.Components[i])
		}
	}
	return out
}

// Check validates values chosen for this declaration. A value for a setting
// the declaration does not have is an error here: what an owner submits must
// be for the settings they were shown. imageOK says whether a library file may
// be used as a picture by this installation.
func (s *Spec) Check(values map[string]any, imageOK func(fileID string) bool) (map[string]any, error) {
	out := map[string]any{}
	for path, v := range values {
		c := s.byPath[path]
		if c == nil {
			return nil, fmt.Errorf("settings: %s is not a setting this app has", path)
		}
		if v == nil {
			continue // cleared: the default applies
		}
		n, err := c.normalise(v)
		if err != nil {
			return nil, fmt.Errorf("settings: %s: %w", c.Label, err)
		}
		if c.Type == TypeImage && (imageOK == nil || !imageOK(n.(string))) {
			return nil, fmt.Errorf("settings: %s: that picture is not in this app's library", c.Label)
		}
		out[path] = n
	}
	for _, c := range s.Settings() {
		if c.Required && out[c.Path] == nil && c.Default == nil {
			return nil, fmt.Errorf("settings: %s is required", c.Label)
		}
	}
	return out, nil
}

// Effective is what an app is told: each declared setting's chosen value, or
// its default. Stored values for settings the declaration no longer has, or
// that no longer fit it, are left out rather than passed on.
func (s *Spec) Effective(stored map[string]any) map[string]any {
	out := map[string]any{}
	for _, c := range s.Settings() {
		if v, ok := stored[c.Path]; ok && v != nil {
			if n, err := c.normalise(v); err == nil {
				out[c.Path] = n
				continue
			}
		}
		if c.Default != nil {
			n, _ := c.normalise(c.Default)
			out[c.Path] = n
		}
	}
	return out
}
