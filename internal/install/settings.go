package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/dabbers/devex/internal/appsettings"
	"github.com/dabbers/devex/internal/domain"
	"github.com/dabbers/devex/internal/library"
	"github.com/dabbers/devex/internal/proxy/hostmux"
	"github.com/dabbers/devex/internal/store"
)

// ErrInvalid marks settings that were not accepted, as distinct from the
// platform failing to save them.
var ErrInvalid = errors.New("not accepted")

// SettingsFile is where an app finds its settings, relative to its root. The
// platform writes it, read-only, whenever they or the app's declaration
// change; an app re-reads it when it changes and needs no restart. Apps may
// take the location from DABBERZ_SETTINGS_FILE, which defaults to this.
const SettingsFile = ".dabberz/settings.json"

// Delivered is what an app reads from SettingsFile.
type Delivered struct {
	// Values is every declared setting's value, chosen or default, by path.
	Values map[string]any `json:"values"`
	// Images is, for each picture setting that has one, where the edge
	// serves it on the app's own hostname.
	Images map[string]DeliveredImage `json:"images"`
}

// DeliveredImage is a picture an app links to.
type DeliveredImage struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
}

// SettingsView is everything the panel needs to draw an app's settings.
type SettingsView struct {
	// Spec is nil when the app declares no settings.
	Spec *appsettings.Spec `json:"spec"`
	// Problem says why a declaration that exists could not be used.
	Problem string `json:"problem,omitempty"`
	// Values are the owner's choices for settings the app has now.
	Values map[string]any `json:"values"`
	// Effective is what the app is told, defaults included.
	Effective map[string]any `json:"effective"`
	// Images are the pictures a picture setting may use.
	Images []*store.LibraryFile `json:"images"`
}

// readSpec reads a workspace's declaration. No file is no settings, not an
// error; a file that does not parse is an error the caller reports.
func (i *Installer) readSpec(ctx context.Context, ws *domain.Workspace) (*appsettings.Spec, error) {
	raw, err := i.deps.Workspaces.ReadFile(ctx, ws, appsettings.File, appsettings.MaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return appsettings.Parse(raw)
}

func (i *Installer) servingWorkspace(ctx context.Context, installation *domain.Installation) (*domain.Workspace, error) {
	if installation.WorkspaceID == "" {
		return nil, fmt.Errorf("install: %s has no workspace", installation.Name)
	}
	return i.deps.Store.GetWorkspace(ctx, installation.WorkspaceID)
}

// pictures returns the library images an installation may use: the account's
// and its own.
func (i *Installer) pictures(ctx context.Context, installation *domain.Installation) ([]*store.LibraryFile, error) {
	files, err := i.deps.Store.ListLibraryFiles(ctx, installation.UserID, "", installation.ID)
	if err != nil {
		return nil, err
	}
	var out []*store.LibraryFile
	for _, f := range files {
		if f.Kind == string(library.KindImage) {
			out = append(out, f)
		}
	}
	return out, nil
}

// Settings returns an installation's settings as its live code declares them.
func (i *Installer) Settings(ctx context.Context, installation *domain.Installation) (*SettingsView, error) {
	ws, err := i.servingWorkspace(ctx, installation)
	if err != nil {
		return nil, err
	}
	view := &SettingsView{Values: map[string]any{}, Effective: map[string]any{}}
	if view.Images, err = i.pictures(ctx, installation); err != nil {
		return nil, err
	}
	spec, err := i.readSpec(ctx, ws)
	if err != nil {
		view.Problem = err.Error()
		return view, nil
	}
	if spec == nil {
		return view, nil
	}
	stored, err := i.deps.Store.SettingsValues(ctx, installation.ID)
	if err != nil {
		return nil, err
	}
	view.Spec = spec
	for _, c := range spec.Settings() {
		if v, ok := stored[c.Path]; ok {
			view.Values[c.Path] = v
		}
	}
	view.Effective = spec.Effective(stored)
	return view, nil
}

// SetSettings records an owner's choices and gives them to the running app.
// Values are for the settings the live code declares; nil clears one back to
// its default. Values kept for settings the app no longer has are left alone.
func (i *Installer) SetSettings(ctx context.Context, installation *domain.Installation, by string, submitted map[string]any) (*SettingsView, error) {
	ws, err := i.servingWorkspace(ctx, installation)
	if err != nil {
		return nil, err
	}
	spec, err := i.readSpec(ctx, ws)
	if err != nil {
		return nil, err
	}
	if spec == nil {
		return nil, fmt.Errorf("%w: %s declares no settings", ErrInvalid, installation.App)
	}
	pictures, err := i.pictures(ctx, installation)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, f := range pictures {
		allowed[f.ID] = true
	}
	checked, err := spec.Check(submitted, func(id string) bool { return allowed[id] })
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	stored, err := i.deps.Store.SettingsValues(ctx, installation.ID)
	if err != nil {
		return nil, err
	}
	for path, v := range submitted {
		if v == nil {
			delete(stored, path)
		} else {
			stored[path] = checked[path]
		}
	}
	if err := i.deps.Store.SetSettingsValues(ctx, installation.ID, by, stored); err != nil {
		return nil, err
	}
	if err := i.publishSettings(ctx, installation, ws); err != nil {
		return nil, fmt.Errorf("install: saved, but the app has not been given them yet: %w", err)
	}
	return i.Settings(ctx, installation)
}

// publishSettings writes what the app in ws should see: the owner's choices
// for the settings that code declares. ws is the installation's own or one of
// its changes', and each is told about its own declaration, so a change that
// adds a setting shows its default in the preview before anyone has chosen.
func (i *Installer) publishSettings(ctx context.Context, installation *domain.Installation, ws *domain.Workspace) error {
	spec, err := i.readSpec(ctx, ws)
	if err != nil {
		return err
	}
	delivered := Delivered{Values: map[string]any{}, Images: map[string]DeliveredImage{}}
	if spec != nil {
		stored, err := i.deps.Store.SettingsValues(ctx, installation.ID)
		if err != nil {
			return err
		}
		delivered.Values = spec.Effective(stored)
		pictures, err := i.pictures(ctx, installation)
		if err != nil {
			return err
		}
		byID := map[string]*store.LibraryFile{}
		for _, f := range pictures {
			byID[f.ID] = f
		}
		for _, c := range spec.Settings() {
			if c.Type != appsettings.TypeImage {
				continue
			}
			id, _ := delivered.Values[c.Path].(string)
			if f := byID[id]; f != nil {
				delivered.Images[c.Path] = DeliveredImage{
					URL:         "/_dabberz/asset/" + f.SHA256 + "." + imageExtension(f.ContentType),
					ContentType: f.ContentType,
				}
			} else {
				// A picture since deleted from the library: the app shows
				// its own rather than a broken link.
				delete(delivered.Values, c.Path)
			}
		}
	}
	raw, err := json.MarshalIndent(delivered, "", "  ")
	if err != nil {
		return err
	}
	return i.deps.Workspaces.WriteFile(ctx, ws, SettingsFile, raw)
}

// refreshSettings is publishSettings where a failure must not undo what was
// being done: an install, a promote, a change starting. It is logged; the app
// carries on with what it had, or its own defaults.
func (i *Installer) refreshSettings(ctx context.Context, installation *domain.Installation, ws *domain.Workspace) {
	if err := i.publishSettings(ctx, installation, ws); err != nil {
		i.deps.Logger.Warn("could not give an app its settings",
			"installation", installation.ID, "workspace", ws.ID, "error", err)
	}
}

// RefreshSettings gives the app in a workspace its settings again, after
// something in that workspace may have changed its declaration.
func (i *Installer) RefreshSettings(ctx context.Context, ws *domain.Workspace) {
	installation, err := i.installationFor(ctx, ws)
	if err != nil || installation == nil {
		return
	}
	i.refreshSettings(ctx, installation, ws)
}

// installationFor returns the installation a workspace serves or changes, or
// nil for a workspace that is neither.
func (i *Installer) installationFor(ctx context.Context, ws *domain.Workspace) (*domain.Installation, error) {
	if change, err := i.deps.Store.ChangeByWorkspace(ctx, ws.ID); err == nil {
		return i.deps.Store.GetInstallation(ctx, change.InstallationID)
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	installation, err := i.deps.Store.InstallationByWorkspace(ctx, ws.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return installation, err
}

// InstallationFor is installationFor, for callers outside the package.
func (i *Installer) InstallationFor(ctx context.Context, ws *domain.Workspace) (*domain.Installation, error) {
	return i.installationFor(ctx, ws)
}

func imageExtension(contentType string) string {
	switch contentType {
	case "image/jpeg":
		return "jpg"
	case "image/gif":
		return "gif"
	}
	return "png"
}

// Assets serves, at the edge, the pictures installations' settings use.
type Assets struct {
	Installer *Installer
}

// Asset implements hostmux.Assets. A picture is found only if a setting of
// the installation behind host points at it right now.
func (a Assets) Asset(ctx context.Context, host, name string) (string, []byte, error) {
	i := a.Installer
	if i.deps.Library == nil {
		return "", nil, hostmux.ErrNoAsset
	}
	workspaceID, err := i.deps.Store.WorkspaceForHostname(ctx, host)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil, hostmux.ErrNoAsset
	}
	if err != nil {
		return "", nil, err
	}
	ws, err := i.deps.Store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return "", nil, err
	}
	installation, err := i.installationFor(ctx, ws)
	if err != nil {
		return "", nil, err
	}
	if installation == nil {
		return "", nil, hostmux.ErrNoAsset
	}
	sha := strings.SplitN(name, ".", 2)[0]
	stored, err := i.deps.Store.SettingsValues(ctx, installation.ID)
	if err != nil {
		return "", nil, err
	}
	chosen := map[string]bool{}
	for _, v := range stored {
		if s, ok := v.(string); ok {
			chosen[s] = true
		}
	}
	pictures, err := i.pictures(ctx, installation)
	if err != nil {
		return "", nil, err
	}
	for _, f := range pictures {
		if f.SHA256 == sha && chosen[f.ID] {
			data, err := i.deps.Library.Read(f)
			if err != nil {
				return "", nil, err
			}
			return f.ContentType, data, nil
		}
	}
	return "", nil, hostmux.ErrNoAsset
}
