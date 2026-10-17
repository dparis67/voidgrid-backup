package tidy

import (
	"errors"
	"os"
	"path/filepath"
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
