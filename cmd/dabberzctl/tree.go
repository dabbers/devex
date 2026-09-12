package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// maxTreeEntries bounds what is sent to the discovery pass, so a large repo
// does not produce a prompt that cannot be read.
const maxTreeEntries = 2000

// skipDirs are directories that say nothing about a repo's project layout.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "target": true,
	"dist": true, "build": true, ".next": true, ".venv": true,
	"__pycache__": true, ".idea": true, ".cache": true,
}

// listTree collects a repo-relative file listing for monorepo discovery.
//
// No manifest file is required, so the listing is what the discovery pass
// reads: the layout itself is the evidence.
func listTree(root string) ([]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("could not read %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}

	var entries []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subdirectory should not abandon the whole walk.
			return nil //nolint:nilerr // partial listings are still useful
		}
		if len(entries) >= maxTreeEntries {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil //nolint:nilerr // skip anything we cannot name
		}
		entries = append(entries, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("%s contains no files to inspect", root)
	}
	return entries, nil
}

// treeSummary renders a listing for display.
func treeSummary(entries []string) string {
	if len(entries) <= 10 {
		return strings.Join(entries, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(entries[:10], ", "), len(entries)-10)
}
