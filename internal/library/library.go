package library

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/dabbers/devex/internal/store"
	"github.com/dabbers/devex/internal/vm"
)

// Where the library appears in a workspace, relative to its root. Under
// .dabberz/, which the platform keeps out of git, so nothing here is ever
// committed into an app.
const (
	// Dir is the read-only copy an agent reads: account/ and app/.
	Dir = ".dabberz/library"
	// Outbox is where an agent leaves files for the library. Nothing here is
	// trusted: each is admitted exactly as an upload would be.
	Outbox = ".dabberz/library-outbox"
)

// Limits on what one agent turn may hand in.
const (
	maxOutboxFiles = 50
	maxOutboxBytes = 40 << 20
)

// Scope names as they appear in a workspace.
const (
	accountFolder = "account"
	appFolder     = "app"
)

// Runner runs a command in a workspace machine; vm.Driver satisfies it.
type Runner interface {
	Exec(ctx context.Context, instanceID string, cmd vm.Command) (*vm.ExecResult, error)
}

// Library keeps the files and hands them to workspaces.
type Library struct {
	store *store.Store
	blobs string
}

// New returns a library keeping its bytes under dir.
func New(st *store.Store, dir string) (*Library, error) {
	blobs := filepath.Join(dir, "blobs")
	if err := os.MkdirAll(blobs, 0o750); err != nil {
		return nil, fmt.Errorf("library: %w", err)
	}
	return &Library{store: st, blobs: blobs}, nil
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (l *Library) blobPath(sha string) (string, error) {
	if !shaPattern.MatchString(sha) {
		return "", fmt.Errorf("library: %q is not a content hash", sha)
	}
	return filepath.Join(l.blobs, sha[:2], sha), nil
}

// Put admits a file and keeps it at path in the scope ("" for the account).
func (l *Library) Put(ctx context.Context, userID, scope, path string, data []byte, by string) (*store.LibraryFile, error) {
	admitted, err := Admit(path, data)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(admitted.Data)
	sha := hex.EncodeToString(sum[:])
	target, err := l.blobPath(sha)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return nil, err
		}
		tmp, err := os.CreateTemp(filepath.Dir(target), ".incoming-*")
		if err != nil {
			return nil, err
		}
		_, werr := tmp.Write(admitted.Data)
		cerr := tmp.Close()
		if werr != nil || cerr != nil {
			os.Remove(tmp.Name())
			return nil, errors.Join(werr, cerr)
		}
		if err := os.Chmod(tmp.Name(), 0o440); err != nil {
			os.Remove(tmp.Name())
			return nil, err
		}
		if err := os.Rename(tmp.Name(), target); err != nil {
			os.Remove(tmp.Name())
			return nil, err
		}
	}
	f := &store.LibraryFile{
		UserID: userID, Scope: scope, Path: admitted.Path, Kind: string(admitted.Kind),
		ContentType: admitted.ContentType, Size: int64(len(admitted.Data)), SHA256: sha,
		CreatedBy: by,
	}
	if err := l.store.PutLibraryFile(ctx, f); err != nil {
		return nil, err
	}
	return f, nil
}

// Read returns a file's bytes.
func (l *Library) Read(f *store.LibraryFile) ([]byte, error) {
	p, err := l.blobPath(f.SHA256)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// Delete removes a file, and its bytes once nothing else uses them.
func (l *Library) Delete(ctx context.Context, userID, fileID string) error {
	sha, stillUsed, err := l.store.DeleteLibraryFile(ctx, userID, fileID)
	if err != nil || stillUsed {
		return err
	}
	p, err := l.blobPath(sha)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Sync puts a read-only copy of what an agent working on this installation
// may read into the workspace: the account's files under account/, the app's
// own under app/. Whatever was there is replaced, so a copy an agent edited
// in place is not mistaken for the library.
func (l *Library) Sync(ctx context.Context, run Runner, instanceID, userID, installationID string) error {
	files, err := l.store.ListLibraryFiles(ctx, userID, "")
	if err != nil {
		return err
	}
	var own []*store.LibraryFile
	if installationID != "" {
		if own, err = l.store.ListLibraryFiles(ctx, userID, installationID); err != nil {
			return err
		}
	}

	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	add := func(folder string, f *store.LibraryFile) error {
		data, err := l.Read(f)
		if err != nil {
			return fmt.Errorf("library: read %s: %w", f.Path, err)
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: folder + "/" + f.Path, Mode: 0o444, Size: int64(len(data)),
			ModTime: f.UpdatedAt, Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	}
	for _, f := range files {
		if err := add(accountFolder, f); err != nil {
			return err
		}
	}
	for _, f := range own {
		if err := add(appFolder, f); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}

	// Unpacked by tar as the workspace's own user, with nothing but regular
	// files in the archive and every mode bit for executing cleared after.
	// The copy is marked read-only, which an agent could undo; what keeps
	// the library safe is that nothing is ever read back from here.
	script := `set -e
d=` + Dir + `
[ -d "$d" ] && chmod -R u+w "$d"
rm -rf "$d"
mkdir -p "$d/` + accountFolder + `" "$d/` + appFolder + `" ` + Outbox + `
tar -x --no-same-owner --no-same-permissions -f - -C "$d"
find "$d" -type f -exec chmod 0444 {} +
find "$d" -type d -exec chmod 0555 {} +`
	res, err := run.Exec(ctx, instanceID, vm.Command{
		Argv: []string{"sh", "-c", script}, Stdin: archive.Bytes(), Timeout: time.Minute,
	})
	if err != nil {
		return fmt.Errorf("library: copy into the workspace: %w", err)
	}
	if !res.OK() {
		return fmt.Errorf("library: copy into the workspace: %s", strings.TrimSpace(res.Stderr))
	}
	return nil
}

// Collected is what an agent left in the outbox.
type Collected struct {
	Saved   []*store.LibraryFile
	Refused []string
}

// Collect takes what an agent left in the outbox into the library and empties
// it. account/... goes to the account, app/... to the installation; each file
// is admitted as an upload would be, and a refused one is reported, not kept.
func (l *Library) Collect(ctx context.Context, run Runner, instanceID, userID, installationID, by string) (*Collected, error) {
	// Only regular files are packed (find -type f), so a symlink an agent
	// left pointing at something else on the machine is not followed.
	script := `set -e
o=` + Outbox + `
[ -d "$o" ] || exit 0
cd "$o"
find . -type f -print0 | tar -c --null -T - -f - | base64 -w0
cd - >/dev/null
rm -rf "$o"
mkdir -p "$o"`
	res, err := run.Exec(ctx, instanceID, vm.Command{
		Argv: []string{"sh", "-c", script}, Timeout: time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("library: read the outbox: %w", err)
	}
	if !res.OK() {
		return nil, fmt.Errorf("library: read the outbox: %s", strings.TrimSpace(res.Stderr))
	}
	out := &Collected{}
	raw := strings.TrimSpace(res.Stdout)
	if raw == "" {
		return out, nil
	}
	if base64.StdEncoding.DecodedLen(len(raw)) > maxOutboxBytes+(1<<20) {
		return nil, fmt.Errorf("library: the outbox holds more than %d MiB", maxOutboxBytes>>20)
	}
	packed, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("library: unreadable outbox: %w", err)
	}

	tr := tar.NewReader(bytes.NewReader(packed))
	for count := 0; ; count++ {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, fmt.Errorf("library: unreadable outbox: %w", err)
		}
		if count >= maxOutboxFiles {
			out.Refused = append(out.Refused, fmt.Sprintf("more than %d files at once; the rest were not read", maxOutboxFiles))
			return out, nil
		}
		name := strings.TrimPrefix(h.Name, "./")
		if h.Typeflag != tar.TypeReg {
			out.Refused = append(out.Refused, name+": not a regular file")
			continue
		}
		if h.Size > MaxFileBytes {
			out.Refused = append(out.Refused, fmt.Sprintf("%s: larger than %d MiB", name, MaxFileBytes>>20))
			continue
		}
		folder, rest, _ := strings.Cut(name, "/")
		scope := ""
		switch {
		case folder == accountFolder && rest != "":
		case folder == appFolder && rest != "" && installationID != "":
			scope = installationID
		default:
			out.Refused = append(out.Refused, name+": put files under account/ or app/")
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, MaxFileBytes+1))
		if err != nil {
			return out, err
		}
		f, err := l.Put(ctx, userID, scope, rest, data, by)
		if err != nil {
			out.Refused = append(out.Refused, name+": "+strings.TrimPrefix(err.Error(), ErrRefused.Error()+": "))
			continue
		}
		out.Saved = append(out.Saved, f)
	}
}
