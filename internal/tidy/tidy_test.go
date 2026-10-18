package tidy

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type failingCloser struct{ closed bool }

func (f *failingCloser) Close() error { f.closed = true; return errors.New("boom") }

func TestTidy(t *testing.T) {
	c := &failingCloser{}
	Close(c) // must not panic or return the error
	if !c.closed {
		t.Fatal("Close did not close")
	}

	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	Remove(file)
	Remove(file) // already gone: no failure
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("file still there: %v", err)
	}

	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	RemoveAll(filepath.Join(dir, "a"))
	if _, err := os.Stat(sub); !os.IsNotExist(err) {
		t.Fatalf("tree still there: %v", err)
	}
}

// captureLog returns what slog wrote while fn ran.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)
	fn()
	return buf.String()
}

type ctxCloser struct{ err error }

func (c ctxCloser) Close(context.Context) error { return c.err }

func TestWarnHelpersLogEveryFailure(t *testing.T) {
	// An error that looks like "already closed" is still logged: it means a
	// double close in our own code.
	closed := fmt.Errorf("use of closed network connection: %w", net.ErrClosed)
	for name, fn := range map[string]func(){
		"Check":     func() { Check("x", closed) },
		"CloseWarn": func() { CloseWarn("x", &failingCloser{}) },
		"CloseCtx":  func() { CloseCtx(context.Background(), "x", ctxCloser{closed}) },
	} {
		if out := captureLog(t, fn); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "x") {
			t.Errorf("%s did not log a warning naming what failed: %q", name, out)
		}
	}
	for name, fn := range map[string]func(){
		"Check":     func() { Check("x", nil) },
		"CloseCtx":  func() { CloseCtx(context.Background(), "x", ctxCloser{}) },
		"CloseWarn": func() { CloseWarn("x", closerFunc(func() error { return nil })) },
	} {
		if out := captureLog(t, fn); out != "" {
			t.Errorf("%s logged for a clean close: %q", name, out)
		}
	}
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// A driver whose transactions fail to roll back, to see a real failure logged.
type failDriver struct{}

type failConn struct{}

type failTx struct{}

func (failDriver) Open(string) (driver.Conn, error)  { return failConn{}, nil }
func (failConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not used") }
func (failConn) Close() error                        { return nil }
func (failConn) Begin() (driver.Tx, error)           { return failTx{}, nil }
func (failTx) Commit() error                         { return nil }
func (failTx) Rollback() error                       { return errors.New("the connection is gone") }

func init() { sql.Register("tidy-fail", failDriver{}) }

func TestRollbackIgnoresOnlyAnAlreadyFinishedTransaction(t *testing.T) {
	// A committed transaction: the deferred rollback finds nothing to undo.
	db, err := sql.Open("tidy-fail", "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if out := captureLog(t, func() { Rollback(tx) }); out != "" {
		t.Errorf("a rollback after a commit was logged: %q", out)
	}

	// A rollback that fails for real is logged.
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if out := captureLog(t, func() { Rollback(tx) }); !strings.Contains(out, "the connection is gone") || !strings.Contains(out, "level=WARN") {
		t.Errorf("a failed rollback was not logged: %q", out)
	}
}
