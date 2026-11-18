package install

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dabbers/devex/internal/appspec"
)

// Catalog is the set of apps a customer can install.
//
// A directory of app sources for now, because the platform does not host
// repositories yet. The shape it will take -- "read the app at a pinned
// version from somewhere" -- is the same either way, which is why the rest of
// the installer is written against this and not against a filesystem.
type Catalog struct {
	dir string
}

// App is one entry.
type App struct {
	Spec *appspec.Spec
	// Dir is where this app's source is, which the installer seeds a
	// repository from.
	Dir string
}

// OpenCatalog reads a directory of apps.
func OpenCatalog(dir string) (*Catalog, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("install: catalog %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("install: catalog %s is not a directory", dir)
	}
	return &Catalog{dir: dir}, nil
}

// List returns every app that satisfies the contract.
//
// An app whose manifest does not validate is left out rather than returned
// broken, and the reason is reported, because an app nobody can install is
// better than one that fails halfway through installing.
func (c *Catalog) List() ([]App, []error) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return nil, []error{fmt.Errorf("install: read catalog: %w", err)}
	}

	var (
		apps     []App
		problems []error
	)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(c.dir, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, appspec.Filename)); err != nil {
			// A directory with no manifest is not an app, which is not an
			// error: the catalog directory may hold other things.
			continue
		}
		spec, err := appspec.Load(dir)
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", entry.Name(), err))
			continue
		}
		apps = append(apps, App{Spec: spec, Dir: dir})
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Spec.App < apps[j].Spec.App })
	return apps, problems
}

// Get returns one app by its slug.
func (c *Catalog) Get(slug string) (App, error) {
	slug = strings.TrimSpace(slug)
	apps, _ := c.List()
	for _, app := range apps {
		if app.Spec.App == slug {
			return app, nil
		}
	}

	names := make([]string, 0, len(apps))
	for _, app := range apps {
		names = append(names, app.Spec.App)
	}
	if len(names) == 0 {
		return App{}, fmt.Errorf("install: no app called %q, and the catalog is empty", slug)
	}
	return App{}, fmt.Errorf("install: no app called %q; the catalog has %s",
		slug, strings.Join(names, ", "))
}
