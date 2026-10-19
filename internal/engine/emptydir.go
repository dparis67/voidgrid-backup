package engine

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/voidgrid/voidgrid-backup/internal/tidy"
)

// emptyOrMissing says whether a restore target is a directory with nothing in
// it, or does not exist yet. The target is the path the operator chose for the
// restore, checked by guard.RestoreTarget before it gets here, so opening it is
// the point, not a hazard; it is in its own file so that .golangci.yml can
// excuse G304 for this file alone.
func emptyOrMissing(dir string) (bool, error) {
	f, err := os.Open(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer tidy.Close(f)
	names, err := f.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("restore target %s: %w", dir, err)
	}
	return len(names) == 0, nil
}
