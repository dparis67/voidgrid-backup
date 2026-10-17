// Package tidy closes and removes things whose failure nobody can act on: a
// read handle at the end of a function, a temporary file after it was used. A
// failure is written to the debug log instead of being dropped, so it can
// still be found.
package tidy

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
)

// Close closes c.
func Close(c io.Closer) {
	if err := c.Close(); err != nil {
		slog.Debug("close failed", "err", err)
	}
}

// Remove deletes the file at path. A file that is already gone is not a failure.
func Remove(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Debug("remove failed", "path", path, "err", err)
	}
}

// RemoveAll deletes path and everything under it.
func RemoveAll(path string) {
	if err := os.RemoveAll(path); err != nil {
		slog.Debug("remove failed", "path", path, "err", err)
	}
}
