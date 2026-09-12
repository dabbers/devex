// Package memory implements the out-of-repo notes store dabberz keeps per repo.
//
// It holds what agents need to know about a repo but cannot easily grep out of
// it: layout, coding standards, design language and UI patterns, colour
// schemes, architectural decisions, and stated user preferences. Keeping it
// outside the repo means dabberz can record its own understanding of a project
// without committing to it.
//
// The layout is a structured markdown folder per repo -- an index plus one file
// per finding -- so an agent can pull just the findings relevant to its task
// instead of loading everything. Each fork's coding agent writes directly as it
// learns; there is no serialization through the orchestrator, and concurrent
// writes to the same finding resolve last-write-wins, consistent with the rest
// of the system's tolerance for a messy dev-only environment.
package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrNotFound reports an unknown finding.
var ErrNotFound = errors.New("memory: finding not found")

// Layout constants.
const (
	findingsDir = "findings"
	indexFile   = "index.md"
	fileExt     = ".md"
)

// Finding is one topic dabberz has learned about a repo, stored as its own
// file so it can be retrieved without reading the whole store.
type Finding struct {
	// Slug is the finding's identity and its filename. Re-writing the same
	// slug replaces the finding.
	Slug string `yaml:"-" json:"slug"`
	// Title is a one-line human-readable heading.
	Title string `yaml:"title" json:"title"`
	// Tags let an agent pull a subset, such as everything tagged "design".
	Tags []string `yaml:"tags,omitempty" json:"tags,omitempty"`
	// Source records who wrote the finding, usually a fork id, so a surprising
	// note can be traced back to the workstream that recorded it.
	Source string `yaml:"source,omitempty" json:"source,omitempty"`
	// UpdatedAt is the last write time, which decides last-write-wins.
	UpdatedAt time.Time `yaml:"updated_at" json:"updated_at"`
	// Body is the markdown content below the front matter.
	Body string `yaml:"-" json:"body"`
}

// Store is a memory store rooted at a directory on disk.
type Store struct {
	root string
}

// New returns a store rooted at dir, creating it if needed.
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("memory: a root directory is required")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("memory: create root %s: %w", dir, err)
	}
	return &Store{root: dir}, nil
}

// Root reports the store's directory.
func (s *Store) Root() string { return s.root }

// RepoDir reports where a repo's memory lives, which is also what gets mounted
// into a fork's VM.
func (s *Store) RepoDir(repoID string) (string, error) {
	if err := validSegment(repoID); err != nil {
		return "", err
	}
	return filepath.Join(s.root, repoID), nil
}

// Put writes a finding, replacing any existing finding with the same slug.
//
// Writes are last-write-wins by design: forks write concurrently without
// coordinating, and a lock would serialize agents for no real benefit in a
// dev-only store.
func (s *Store) Put(ctx context.Context, repoID string, finding *Finding) error {
	if err := validSegment(repoID); err != nil {
		return err
	}
	slug := Slug(finding.Slug)
	if slug == "" {
		slug = Slug(finding.Title)
	}
	if slug == "" {
		return errors.New("memory: a finding needs a slug or a title")
	}
	if err := validSegment(slug); err != nil {
		return err
	}
	finding.Slug = slug
	if finding.Title == "" {
		finding.Title = slug
	}
	if finding.UpdatedAt.IsZero() {
		finding.UpdatedAt = time.Now().UTC()
	}

	path := filepath.Join(s.root, repoID, findingsDir, slug+fileExt)
	if err := writeFileAtomic(path, []byte(render(finding))); err != nil {
		return err
	}
	return s.RebuildIndex(ctx, repoID)
}

// Get returns one finding.
func (s *Store) Get(_ context.Context, repoID, slug string) (*Finding, error) {
	if err := validSegment(repoID); err != nil {
		return nil, err
	}
	if err := validSegment(slug); err != nil {
		return nil, err
	}

	path := filepath.Join(s.root, repoID, findingsDir, slug+fileExt)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("memory: %s in repo %s: %w", slug, repoID, ErrNotFound)
		}
		return nil, fmt.Errorf("memory: read finding %s: %w", slug, err)
	}
	return parse(slug, raw)
}

// List returns every finding for a repo, most recently updated first, so an
// agent reading only the first few gets the freshest understanding.
func (s *Store) List(_ context.Context, repoID string) ([]*Finding, error) {
	if err := validSegment(repoID); err != nil {
		return nil, err
	}

	dir := filepath.Join(s.root, repoID, findingsDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// A repo nothing has been learned about yet is empty, not broken.
			return nil, nil
		}
		return nil, fmt.Errorf("memory: list findings for %s: %w", repoID, err)
	}

	findings := make([]*Finding, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), fileExt) {
			continue
		}
		slug := strings.TrimSuffix(entry.Name(), fileExt)
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("memory: read finding %s: %w", slug, err)
		}
		finding, err := parse(slug, raw)
		if err != nil {
			return nil, err
		}
		findings = append(findings, finding)
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].UpdatedAt.Equal(findings[j].UpdatedAt) {
			return findings[i].Slug < findings[j].Slug
		}
		return findings[i].UpdatedAt.After(findings[j].UpdatedAt)
	})
	return findings, nil
}

// Search returns findings matching a free-text query or a tag, so an agent can
// pull only what bears on its task.
func (s *Store) Search(ctx context.Context, repoID, query string, tags ...string) ([]*Finding, error) {
	all, err := s.List(ctx, repoID)
	if err != nil {
		return nil, err
	}
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" && len(tags) == 0 {
		return all, nil
	}

	matched := make([]*Finding, 0, len(all))
	for _, finding := range all {
		if len(tags) > 0 && !hasAnyTag(finding, tags) {
			continue
		}
		if query != "" && !matchesText(finding, query) {
			continue
		}
		matched = append(matched, finding)
	}
	return matched, nil
}

// Delete removes a finding.
func (s *Store) Delete(ctx context.Context, repoID, slug string) error {
	if err := validSegment(repoID); err != nil {
		return err
	}
	if err := validSegment(slug); err != nil {
		return err
	}

	path := filepath.Join(s.root, repoID, findingsDir, slug+fileExt)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("memory: %s in repo %s: %w", slug, repoID, ErrNotFound)
		}
		return fmt.Errorf("memory: delete finding %s: %w", slug, err)
	}
	return s.RebuildIndex(ctx, repoID)
}

// RebuildIndex regenerates a repo's index from the findings on disk.
//
// The index is derived, never authoritative: rebuilding from the directory
// means concurrent writers converge without coordinating, and a torn or stale
// index repairs itself on the next write.
func (s *Store) RebuildIndex(ctx context.Context, repoID string) error {
	findings, err := s.List(ctx, repoID)
	if err != nil {
		return err
	}

	var b strings.Builder
	b.WriteString("# Repo memory\n\n")
	b.WriteString("What dabberz has learned about this repo. One file per finding under `")
	b.WriteString(findingsDir)
	b.WriteString("/`; pull only what bears on your task.\n\n")

	if len(findings) == 0 {
		b.WriteString("_Nothing recorded yet._\n")
	} else {
		byTag := map[string][]*Finding{}
		var untagged []*Finding
		for _, finding := range findings {
			if len(finding.Tags) == 0 {
				untagged = append(untagged, finding)
				continue
			}
			for _, tag := range finding.Tags {
				byTag[tag] = append(byTag[tag], finding)
			}
		}

		tags := make([]string, 0, len(byTag))
		for tag := range byTag {
			tags = append(tags, tag)
		}
		sort.Strings(tags)

		for _, tag := range tags {
			b.WriteString("## " + tag + "\n\n")
			for _, finding := range byTag[tag] {
				writeIndexEntry(&b, finding)
			}
			b.WriteString("\n")
		}
		if len(untagged) > 0 {
			b.WriteString("## untagged\n\n")
			for _, finding := range untagged {
				writeIndexEntry(&b, finding)
			}
		}
	}

	path := filepath.Join(s.root, repoID, indexFile)
	return writeFileAtomic(path, []byte(b.String()))
}

// Index returns the rendered index for a repo.
func (s *Store) Index(ctx context.Context, repoID string) (string, error) {
	if err := validSegment(repoID); err != nil {
		return "", err
	}
	path := filepath.Join(s.root, repoID, indexFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Build it on demand rather than reporting an error for a repo
			// nothing has been written to yet.
			if err := s.RebuildIndex(ctx, repoID); err != nil {
				return "", err
			}
			raw, err = os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("memory: read index for %s: %w", repoID, err)
			}
			return string(raw), nil
		}
		return "", fmt.Errorf("memory: read index for %s: %w", repoID, err)
	}
	return string(raw), nil
}

func writeIndexEntry(b *strings.Builder, finding *Finding) {
	fmt.Fprintf(b, "- [%s](%s/%s%s) — updated %s\n",
		finding.Title, findingsDir, finding.Slug, fileExt,
		finding.UpdatedAt.UTC().Format("2006-01-02"))
}

// render writes a finding as markdown with YAML front matter.
func render(finding *Finding) string {
	meta, err := yaml.Marshal(finding)
	if err != nil {
		// Finding has only plain scalar and slice fields, so this cannot fail
		// in practice; fall back to a minimal header rather than losing the body.
		meta = []byte("title: " + finding.Title + "\n")
	}
	body := strings.TrimRight(finding.Body, "\n")
	return "---\n" + string(meta) + "---\n\n" + body + "\n"
}

// parse reads a finding file back.
func parse(slug string, raw []byte) (*Finding, error) {
	finding := &Finding{Slug: slug}
	text := string(raw)

	if !strings.HasPrefix(text, "---\n") {
		// A finding written by hand without front matter is still usable.
		finding.Title = slug
		finding.Body = strings.TrimSpace(text)
		return finding, nil
	}

	rest := text[len("---\n"):]
	meta, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return nil, fmt.Errorf("memory: finding %s has unterminated front matter", slug)
	}
	if err := yaml.Unmarshal([]byte(meta), finding); err != nil {
		return nil, fmt.Errorf("memory: parse front matter of %s: %w", slug, err)
	}
	finding.Slug = slug
	if finding.Title == "" {
		finding.Title = slug
	}
	finding.Body = strings.TrimSpace(body)
	return finding, nil
}

func hasAnyTag(finding *Finding, tags []string) bool {
	for _, want := range tags {
		for _, have := range finding.Tags {
			if strings.EqualFold(have, want) {
				return true
			}
		}
	}
	return false
}

func matchesText(finding *Finding, query string) bool {
	return strings.Contains(strings.ToLower(finding.Title), query) ||
		strings.Contains(strings.ToLower(finding.Body), query) ||
		strings.Contains(strings.ToLower(strings.Join(finding.Tags, " ")), query)
}

// Slug reduces a title to a filename-safe identifier.
func Slug(in string) string {
	var b strings.Builder
	lastHyphen := true
	for _, r := range strings.ToLower(strings.TrimSpace(in)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// validSegment rejects path segments that could escape the store. Agents write
// here directly, so a slug is untrusted input.
func validSegment(segment string) error {
	if segment == "" {
		return errors.New("memory: empty path segment")
	}
	if segment == "." || segment == ".." ||
		strings.ContainsAny(segment, `/\`) ||
		strings.Contains(segment, "..") ||
		filepath.IsAbs(segment) {
		return fmt.Errorf("memory: %q is not a valid path segment", segment)
	}
	return nil
}

// writeFileAtomic writes through a temporary file and renames it into place, so
// a reader never sees a half-written finding and concurrent writers resolve to
// one complete file rather than an interleaved one.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("memory: create directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("memory: create temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup: a leftover temporary file is harmless, and the
	// rename below makes this a no-op on the success path.
	defer os.Remove(tmpName) //nolint:errcheck // nothing to do if cleanup fails

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("memory: write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("memory: close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return fmt.Errorf("memory: set permissions on %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("memory: install %s: %w", path, err)
	}
	return nil
}
