// Package tidy closes and removes things whose failure nobody can act on: a
// read handle at the end of a function, a temporary file after it was used. A
// failure is written to the debug log instead of being dropped, so it can
// still be found.
package tidy

import (
	"context"
	"database/sql"
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

// The helpers above are for handles whose failure nobody can act on and which
// are noise below the warning level. The ones below are for the rest: a
// connection, a database, a repository, a session. A failed close there is
// worth knowing about (a database that did not close cleanly, a connection that
// was already dead, a double close in our own code), so it is logged at warning
// level with what was being closed, and not hidden. None of them filters
// "already closed" errors: one of those means the code closed something twice,
// which is a fault to fix, not noise to ignore. The single exception is
// Rollback, below.

// Check logs err at warning level, naming what was being done, if it is not nil.
func Check(what string, err error) {
	if err != nil {
		slog.Warn("cleanup failed", "what", what, "err", err)
	}
}

// CloseWarn closes c and logs a failure at warning level.
func CloseWarn(what string, c io.Closer) { Check("close "+what, c.Close()) }

// CloseCtx closes something whose Close takes a context (a Kopia repository or
// storage) and logs a failure at warning level.
func CloseCtx(ctx context.Context, what string, c interface{ Close(context.Context) error }) {
	Check("close "+what, c.Close(ctx))
}

// Rollback ends a transaction that did not commit and logs a failure at
// warning level. It is written for "defer Rollback straight after Begin": once
// Commit has succeeded, database/sql answers Rollback with sql.ErrTxDone, which
// means "nothing is left to undo" and is how every successful transaction
// ends, so that one answer is not a failure and is not logged. Anything else
// is.
func Rollback(tx *sql.Tx) {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		slog.Warn("cleanup failed", "what", "roll back a transaction", "err", err)
	}
}
