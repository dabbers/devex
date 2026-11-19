//go:build linux

package local

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/dabbers/devex/internal/vm"
)

// nobody returns an unprivileged account to run as. Dropping privileges is
// only possible from root, so without root there is nothing to test: run with
// sudo to exercise it.
func nobody(t *testing.T) *User {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("running as another account needs root; run this package under sudo")
	}
	account, err := user.Lookup("nobody")
	if err != nil {
		t.Skipf("no 'nobody' account: %v", err)
	}
	uid, _ := strconv.ParseUint(account.Uid, 10, 32)
	gid, _ := strconv.ParseUint(account.Gid, 10, 32)
	return &User{UID: uint32(uid), GID: uint32(gid)}
}

// openRoot is a driver root the unprivileged account could reach at all: a
// test's temporary directory sits under root's, which nobody else can enter,
// and that would make every assertion below fail for the wrong reason.
func openRoot(t *testing.T) string {
	t.Helper()
	parent, err := os.MkdirTemp("/tmp", "dabberz-runas-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(parent, "vms")
}

// TestCommandsRunAsTheConfiguredAccount is what lets the agent run at all on
// a host whose control plane is root: Claude Code will not skip its
// permission prompts as root, and on dbz-alpha the first real turn failed in
// half a second for exactly that reason.
func TestCommandsRunAsTheConfiguredAccount(t *testing.T) {
	runAs := nobody(t)
	ctx := context.Background()

	d, err := New(Options{Root: openRoot(t), Total: vm.Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 20}, RunAs: runAs})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	inst, err := d.Create(ctx, smallSpec("unprivileged"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	res, err := d.Exec(ctx, inst.ID, vm.Command{Argv: []string{"sh", "-c",
		`id -u; id -G; printf '%s\n' "$HOME"; touch made-here && echo wrote`}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if res.ExitCode != 0 || len(lines) != 4 {
		t.Fatalf("exit %d, stdout %q, stderr %q", res.ExitCode, res.Stdout, res.Stderr)
	}
	if want := strconv.Itoa(int(runAs.UID)); lines[0] != want {
		t.Fatalf("ran as uid %s, want %s", lines[0], want)
	}
	// Root's supplementary groups, kept, would be root by another name.
	if want := strconv.Itoa(int(runAs.GID)); lines[1] != want {
		t.Fatalf("groups are %q, want only %s", lines[1], want)
	}
	if want := homeDir(filepath.Dir(inst.Workspace)); lines[2] != want {
		t.Fatalf("HOME is %q, want the instance's own %q", lines[2], want)
	}
	if lines[3] != "wrote" {
		t.Fatal("the account cannot write its own workspace")
	}

	// The record of what the driver believes about the instance is not the
	// workload's to rewrite.
	res, err = d.Exec(ctx, inst.ID, vm.Command{Argv: []string{"sh", "-c", "echo x >> ../instance.json"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("the workload could write the driver's instance record")
	}
}

// TestAnInstanceFromBeforeIsHandedOver: a host that ran without RunAs has
// workspaces owned by root, and turning it on must not leave those failing on
// their first write.
func TestAnInstanceFromBeforeIsHandedOver(t *testing.T) {
	runAs := nobody(t)
	ctx := context.Background()
	root := openRoot(t)
	total := vm.Resources{VCPUs: 4, MemoryMiB: 4096, DiskGiB: 20}

	before, err := New(Options{Root: root, Total: total})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	inst, err := before.Create(ctx, smallSpec("legacy"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	built := filepath.Join(inst.Workspace, "bin")
	if err := os.WriteFile(built, []byte("root wrote this"), 0o644); err != nil {
		t.Fatal(err)
	}

	after, err := New(Options{Root: root, Total: total, RunAs: runAs})
	if err != nil {
		t.Fatalf("New with RunAs: %v", err)
	}
	info, err := os.Stat(built)
	if err != nil {
		t.Fatal(err)
	}
	if owner := info.Sys().(*syscall.Stat_t).Uid; owner != runAs.UID {
		t.Fatalf("a file from before is owned by %d, want %d", owner, runAs.UID)
	}
	res, err := after.Exec(ctx, inst.ID, vm.Command{Argv: []string{"sh", "-c", "echo rebuilt > bin"}})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("rewriting a file from before: %v %+v", err, res)
	}
}
