package store

import (
	"fmt"
	"os"
)

// ensureDir creates a directory tree if it is missing.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("store: create directory %s: %w", dir, err)
	}
	return nil
}
