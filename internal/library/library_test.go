package library

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/vm"
)

// shellRunner runs commands with sh in a directory standing in for a
// workspace, which is what the local driver does.
type shellRunner struct{ dir string }

func (s shellRunner) Exec(ctx context.Context, _ string, cmd vm.Command) (*vm.ExecResult, error) {
	c := exec.CommandContext(ctx, cmd.Argv[0], cmd.Argv[1:]...)
	c.Dir = s.dir
	c.Stdin = bytes.NewReader(cmd.Stdin)
	var out, errs bytes.Buffer
	c.Stdout, c.Stderr = &out, &errs
	err := c.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		return nil, err
	}
	return &vm.ExecResult{ExitCode: code, Stdout: out.String(), Stderr: errs.String()}, nil
}

func newLibrary(t *testing.T) (*Library, *store.Store, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	user, err := st.EnsureUserByExternalID(ctx, "test:owner", "owner@example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	lib, err := New(st, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return lib, st, user.ID
}

// workspaceDir is a stand-in workspace. The library's copy in it is read-only
// on purpose, so it is made writable again before the test's cleanup removes
// it.
func workspaceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { exec.Command("chmod", "-R", "u+w", dir).Run() })
	return dir
}

// TestAnAgentReadsTheLibraryAndCannotRunIt: the copy in the workspace has the
// account's files and the app's, and none of it is executable or writable.
func TestAnAgentReadsTheLibraryAndCannotRunIt(t *testing.T) {
	ctx := context.Background()
	lib, _, user := newLibrary(t)
	if _, err := lib.Put(ctx, user, "", "brand/guide.md", []byte("Navy, never red.\n"), "person"); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Put(ctx, user, "ins_one", "decisions.md", []byte("Salary is optional.\n"), "person"); err != nil {
		t.Fatal(err)
	}
	if _, err := lib.Put(ctx, user, "ins_other", "secret.md", []byte("Another app's.\n"), "person"); err != nil {
		t.Fatal(err)
	}

	ws := workspaceDir(t)
	run := shellRunner{dir: ws}
	if err := lib.Sync(ctx, run, "vm", user, "ins_one"); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(ws, Dir, p))
		if err != nil {
			return ""
		}
		return string(b)
	}
	if read("account/brand/guide.md") != "Navy, never red.\n" || read("app/decisions.md") != "Salary is optional.\n" {
		t.Fatal("the workspace does not have the account's and the app's files")
	}
	if read("app/secret.md") != "" {
		t.Fatal("another app's files reached this workspace")
	}
	filepath.Walk(filepath.Join(ws, Dir), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && info.Mode().Perm()&0o333 != 0 {
			t.Errorf("%s is %v: writable or executable", p, info.Mode().Perm())
		}
		return nil
	})

	// A second sync replaces the copy, including what an agent changed in it.
	os.Chmod(filepath.Join(ws, Dir, "account/brand"), 0o755)
	os.WriteFile(filepath.Join(ws, Dir, "account/brand/planted.sh"), []byte("#!/bin/sh\n"), 0o755)
	if err := lib.Sync(ctx, run, "vm", user, "ins_one"); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if read("account/brand/planted.sh") != "" {
		t.Fatal("something an agent left in the copy survived a sync")
	}
}

// TestWhatAnAgentLeavesIsCheckedBeforeItIsKept: the outbox is admitted file by
// file, exactly like an upload, and a refusal says why.
func TestWhatAnAgentLeavesIsCheckedBeforeItIsKept(t *testing.T) {
	ctx := context.Background()
	lib, st, user := newLibrary(t)
	ws := workspaceDir(t)
	run := shellRunner{dir: ws}
	if err := lib.Sync(ctx, run, "vm", user, "ins_one"); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(ws, Outbox)
	write := func(p, body string, mode os.FileMode) {
		os.MkdirAll(filepath.Dir(filepath.Join(out, p)), 0o755)
		if err := os.WriteFile(filepath.Join(out, p), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("account/brand/colours.md", "Primary #1d2b4f\n", 0o644)
	write("app/notes/decision.md", "Kept the salary field.\n", 0o644)
	write("app/run.txt", "#!/bin/sh\nrm -rf /\n", 0o755)
	write("elsewhere.md", "no folder\n", 0o644)
	secret := filepath.Join(t.TempDir(), "host-secret.txt")
	os.WriteFile(secret, []byte("host secret\n"), 0o600)
	os.Symlink(secret, filepath.Join(out, "account", "link.txt"))

	got, err := lib.Collect(ctx, run, "vm", user, "ins_one", "agent:ws")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(got.Saved) != 2 {
		t.Fatalf("saved %d files, want 2; refused %v", len(got.Saved), got.Refused)
	}
	joined := strings.Join(got.Refused, "\n")
	for _, want := range []string{"run.txt", "elsewhere.md"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s was not reported as refused: %v", want, got.Refused)
		}
	}
	files, _ := st.ListLibraryFiles(ctx, user, "", "ins_one")
	for _, f := range files {
		if f.Path == "link.txt" {
			t.Fatal("a symlink out of the workspace was followed into the library")
		}
	}
	if f, err := st.LibraryFileAt(ctx, user, "ins_one", "notes/decision.md"); err != nil || f.CreatedBy != "agent:ws" {
		t.Fatalf("the app's note was not kept as the agent's: %v", err)
	}
	if entries, _ := os.ReadDir(out); len(entries) != 0 {
		t.Fatal("the outbox was not emptied")
	}
}

// TestReplacingAFileKeepsItsIdentity: a setting pointing at a logo follows a
// new version of it, and deleting the last use of some bytes deletes them.
func TestReplacingAFileKeepsItsIdentity(t *testing.T) {
	ctx := context.Background()
	lib, _, user := newLibrary(t)
	first, err := lib.Put(ctx, user, "", "notes.md", []byte("one\n"), "p")
	if err != nil {
		t.Fatal(err)
	}
	second, err := lib.Put(ctx, user, "", "notes.md", []byte("two\n"), "p")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.SHA256 == second.SHA256 {
		t.Fatal("a new version got a new identity, or kept the old bytes")
	}
	blob, _ := lib.blobPath(second.SHA256)
	if err := lib.Delete(ctx, user, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(blob); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the bytes of a deleted file were kept")
	}
}
