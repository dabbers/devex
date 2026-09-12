package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, context.Background()
}

func TestNewRequiresADirectory(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Error("New(\"\") should fail")
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	s, ctx := newStore(t)
	want := &Finding{
		Slug:   "design-language",
		Title:  "Design language",
		Tags:   []string{"design", "ui"},
		Source: "fork_abc",
		Body:   "Buttons use the accent colour.\n\nSpacing is on a 4px grid.",
	}
	if err := s.Put(ctx, "repo_1", want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get(ctx, "repo_1", "design-language")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != want.Title || got.Source != want.Source {
		t.Fatalf("metadata not round-tripped: %+v", got)
	}
	if got.Body != want.Body {
		t.Fatalf("body = %q, want %q", got.Body, want.Body)
	}
	if len(got.Tags) != 2 || got.Tags[0] != "design" {
		t.Fatalf("tags = %v", got.Tags)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt was not stamped")
	}
}

func TestPutIsLastWriteWins(t *testing.T) {
	s, ctx := newStore(t)
	if err := s.Put(ctx, "repo_1", &Finding{Slug: "layout", Title: "Layout", Body: "first"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// A second fork learns something better about the same topic and simply
	// overwrites; there is no coordination and no conflict to resolve.
	if err := s.Put(ctx, "repo_1", &Finding{Slug: "layout", Title: "Layout", Body: "second", Source: "fork_b"}); err != nil {
		t.Fatalf("Put (overwrite): %v", err)
	}

	got, err := s.Get(ctx, "repo_1", "layout")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Body != "second" || got.Source != "fork_b" {
		t.Fatalf("last write did not win: %+v", got)
	}

	all, err := s.List(ctx, "repo_1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d findings, want the overwrite to replace rather than add", len(all))
	}
}

func TestPutDerivesASlugFromTheTitle(t *testing.T) {
	s, ctx := newStore(t)
	finding := &Finding{Title: "Colour scheme & tokens!"}
	if err := s.Put(ctx, "repo_1", finding); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if finding.Slug != "colour-scheme-tokens" {
		t.Fatalf("slug = %q", finding.Slug)
	}
	if _, err := s.Get(ctx, "repo_1", "colour-scheme-tokens"); err != nil {
		t.Fatalf("Get by derived slug: %v", err)
	}
}

func TestPutRequiresAnIdentity(t *testing.T) {
	s, ctx := newStore(t)
	if err := s.Put(ctx, "repo_1", &Finding{Body: "orphan"}); err == nil {
		t.Error("a finding with neither slug nor title should be rejected")
	}
}

func TestPathTraversalIsRejected(t *testing.T) {
	s, ctx := newStore(t)
	// Agents write here directly, so slugs and repo ids are untrusted input.
	// A hostile slug must either be refused or sanitised into a plain name --
	// never turned into a path that leaves the repo's findings directory.
	findings := filepath.Join(s.Root(), "repo_1", findingsDir)
	for _, bad := range []string{"../escape", "..", ".", "nested/slug", `back\slash`, "/absolute", "", "a/../../b"} {
		finding := &Finding{Slug: bad, Title: "x", Body: "y"}
		if err := s.Put(ctx, "repo_1", finding); err == nil {
			written := filepath.Join(findings, finding.Slug+fileExt)
			if _, err := os.Stat(written); err != nil {
				t.Errorf("Put(slug=%q) reported success but wrote nothing at %s", bad, written)
			}
			if dir := filepath.Dir(written); dir != findings {
				t.Errorf("Put(slug=%q) wrote outside the findings directory: %s", bad, written)
			}
		}
		// Repo ids are not sanitised, only validated, so these must be refused.
		if _, err := s.Get(ctx, bad, "slug"); err == nil {
			t.Errorf("Get(repo=%q) should have been rejected", bad)
		}
		if _, err := s.RepoDir(bad); err == nil {
			t.Errorf("RepoDir(%q) should have been rejected", bad)
		}
	}

	// validSegment is the last line of defence behind Slug; check it directly.
	for _, bad := range []string{"..", ".", "a/b", `a\b`, "/abs", "", "x..y"} {
		if err := validSegment(bad); err == nil {
			t.Errorf("validSegment(%q) = nil, want an error", bad)
		}
	}

	// Nothing may exist outside the store root.
	entries, err := os.ReadDir(filepath.Dir(s.Root()))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != filepath.Base(s.Root()) {
			t.Fatalf("a write escaped the store root: %s", entry.Name())
		}
	}
}

func TestGetUnknownFinding(t *testing.T) {
	s, ctx := newStore(t)
	if _, err := s.Get(ctx, "repo_1", "nothing-here"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want ErrNotFound", err)
	}
}

func TestListOnAnUnknownRepoIsEmptyNotAnError(t *testing.T) {
	s, ctx := newStore(t)
	got, err := s.List(ctx, "repo_never_seen")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d findings for an unseen repo", len(got))
	}
}

func TestListIsFreshestFirst(t *testing.T) {
	s, ctx := newStore(t)
	base := time.Now().UTC().Add(-time.Hour)
	for i, slug := range []string{"oldest", "middle", "newest"} {
		f := &Finding{Slug: slug, Title: slug, Body: "b", UpdatedAt: base.Add(time.Duration(i) * time.Minute)}
		if err := s.Put(ctx, "repo_1", f); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	got, err := s.List(ctx, "repo_1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// An agent reading only the first few findings should get the freshest
	// understanding of the repo.
	if got[0].Slug != "newest" || got[2].Slug != "oldest" {
		t.Fatalf("ordering = %s, %s, %s", got[0].Slug, got[1].Slug, got[2].Slug)
	}
}

func TestSearchByTagAndText(t *testing.T) {
	s, ctx := newStore(t)
	findings := []*Finding{
		{Slug: "colour-scheme", Title: "Colour scheme", Tags: []string{"design"}, Body: "Accent is teal."},
		{Slug: "test-layout", Title: "Test layout", Tags: []string{"testing"}, Body: "Tests live beside sources."},
		{Slug: "button-patterns", Title: "Button patterns", Tags: []string{"design", "ui"}, Body: "Primary buttons are teal."},
	}
	for _, f := range findings {
		if err := s.Put(ctx, "repo_1", f); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	design, err := s.Search(ctx, "repo_1", "", "design")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(design) != 2 {
		t.Fatalf("tag search returned %d findings, want 2", len(design))
	}

	teal, err := s.Search(ctx, "repo_1", "teal")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(teal) != 2 {
		t.Fatalf("text search returned %d findings, want 2", len(teal))
	}

	// Tag and text together narrow further.
	both, err := s.Search(ctx, "repo_1", "teal", "ui")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(both) != 1 || both[0].Slug != "button-patterns" {
		t.Fatalf("combined search = %+v", both)
	}

	// An empty query returns everything, so callers can use one code path.
	all, err := s.Search(ctx, "repo_1", "")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("empty search returned %d findings, want all 3", len(all))
	}
}

func TestIndexListsFindingsByTag(t *testing.T) {
	s, ctx := newStore(t)
	for _, f := range []*Finding{
		{Slug: "colour-scheme", Title: "Colour scheme", Tags: []string{"design"}, Body: "b"},
		{Slug: "module-layout", Title: "Module layout", Tags: []string{"architecture"}, Body: "b"},
		{Slug: "loose-note", Title: "Loose note", Body: "b"},
	} {
		if err := s.Put(ctx, "repo_1", f); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	index, err := s.Index(ctx, "repo_1")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	for _, want := range []string{"## architecture", "## design", "## untagged", "Colour scheme", "findings/colour-scheme.md"} {
		if !strings.Contains(index, want) {
			t.Errorf("index is missing %q\n---\n%s", want, index)
		}
	}
}

func TestIndexIsBuiltOnDemandForAnEmptyRepo(t *testing.T) {
	s, ctx := newStore(t)
	index, err := s.Index(ctx, "repo_fresh")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if !strings.Contains(index, "Nothing recorded yet") {
		t.Fatalf("unexpected empty index: %q", index)
	}
}

func TestIndexIsRebuiltAfterDeletion(t *testing.T) {
	s, ctx := newStore(t)
	if err := s.Put(ctx, "repo_1", &Finding{Slug: "temp", Title: "Temporary", Body: "b"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, "repo_1", "temp"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	index, err := s.Index(ctx, "repo_1")
	if err != nil {
		t.Fatalf("Index: %v", err)
	}
	if strings.Contains(index, "Temporary") {
		t.Fatal("the index still lists a deleted finding")
	}
	if err := s.Delete(ctx, "repo_1", "temp"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete = %v, want ErrNotFound", err)
	}
}

func TestRepoMemoriesAreIsolated(t *testing.T) {
	s, ctx := newStore(t)
	if err := s.Put(ctx, "repo_a", &Finding{Slug: "note", Title: "A note", Body: "a"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.List(ctx, "repo_b")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("repo_b sees repo_a's memory: %+v", got)
	}
}

func TestHandWrittenFindingsWithoutFrontMatterAreReadable(t *testing.T) {
	s, ctx := newStore(t)
	dir := filepath.Join(s.Root(), "repo_1", findingsDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manual.md"), []byte("Just some notes."), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := s.Get(ctx, "repo_1", "manual")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Body != "Just some notes." || got.Title != "manual" {
		t.Fatalf("hand-written finding not handled: %+v", got)
	}
}

func TestMalformedFrontMatterIsReported(t *testing.T) {
	s, ctx := newStore(t)
	dir := filepath.Join(s.Root(), "repo_1", findingsDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.md"), []byte("---\ntitle: unterminated\n"), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := s.Get(ctx, "repo_1", "broken"); err == nil {
		t.Fatal("expected unterminated front matter to be reported")
	}
}

func TestConcurrentWritesLeaveEveryFileIntact(t *testing.T) {
	s, ctx := newStore(t)

	// Forks write directly and concurrently with no coordination; the
	// requirement is only that every file ends up complete and parseable.
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := range 5 {
				f := &Finding{
					Slug:   "shared-topic",
					Title:  "Shared topic",
					Tags:   []string{"design"},
					Source: "fork_" + string(rune('a'+i)),
					Body:   strings.Repeat("content ", 64) + string(rune('0'+round)),
				}
				if err := s.Put(ctx, "repo_1", f); err != nil {
					t.Errorf("Put: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	got, err := s.Get(ctx, "repo_1", "shared-topic")
	if err != nil {
		t.Fatalf("Get after concurrent writes: %v", err)
	}
	if got.Title != "Shared topic" || !strings.HasPrefix(got.Body, "content ") {
		t.Fatalf("a concurrent write left a torn file: %+v", got)
	}

	// No temporary files may be left behind.
	entries, err := os.ReadDir(filepath.Join(s.Root(), "repo_1", findingsDir))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Errorf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Design Language":  "design-language",
		"  padded  ":       "padded",
		"UPPER_snake.case": "upper-snake-case",
		"---":              "",
		"../escape":        "escape",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRepoDir(t *testing.T) {
	s, _ := newStore(t)
	dir, err := s.RepoDir("repo_1")
	if err != nil {
		t.Fatalf("RepoDir: %v", err)
	}
	if filepath.Base(dir) != "repo_1" || !strings.HasPrefix(dir, s.Root()) {
		t.Fatalf("RepoDir = %q", dir)
	}
}
