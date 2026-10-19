// Package confined reads a file that must be inside one directory. The read
// goes through os.OpenRoot, which refuses a name that climbs out of the
// directory with ".." and a symlink that points outside it, so a planted link
// cannot make the program read some other file.
package confined

import (
	"io"
	"os"

	"github.com/voidgrid/voidgrid-backup/internal/tidy"
)

// ReadFile reads dir/name, where name is a plain path inside dir. A missing
// directory or file gives an error that satisfies errors.Is(err, fs.ErrNotExist).
func ReadFile(dir, name string) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer tidy.CloseWarn("directory "+dir, root)
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer tidy.Close(f)
	return io.ReadAll(f)
}
