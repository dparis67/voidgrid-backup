package docker

import (
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

func sqliteHeader() []byte { return append([]byte("SQLite format 3\x00"), make([]byte, 64)...) }

func duckHeader() []byte {
	b := make([]byte, 80)
	copy(b[8:], "DUCK")
	return b
}

func TestFindFileDatabases(t *testing.T) {
	mount := t.TempDir()
	write := func(rel string, b []byte) string {
		p := filepath.Join(mount, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	sq := write("app/data.db", sqliteHeader())
	dk := write("analytics/events.duckdb", duckHeader())
	write("notes.db", []byte("this is not a database header at all"))
	write("short.db", []byte("tiny"))
	write("readme.txt", sqliteHeader()) // right header, wrong name: not looked at

	// A symlink to a real database outside the mount, a symlink to one inside
	// it, and a named pipe: none is reported or followed.
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.db")
	if err := os.WriteFile(secret, sqliteHeader(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(mount, "outside.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sq, filepath.Join(mount, "alias.db")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(mount, "pipe.db"), 0o644); err != nil {
		t.Fatal(err)
	}

	type result struct{ sqlite, duckdb []string }
	done := make(chan result, 1)
	go func() {
		s, d := findFileDatabases(mount, 4, 10)
		done <- result{s, d}
	}()
	select {
	case r := <-done:
		slices.Sort(r.sqlite)
		if !slices.Equal(r.sqlite, []string{sq}) || !slices.Equal(r.duckdb, []string{dk}) {
			t.Fatalf("found sqlite=%v duckdb=%v, want only %s and %s", r.sqlite, r.duckdb, sq, dk)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the scan hung on a named pipe")
	}

	// A mount that does not exist finds nothing and does not fail.
	if s, d := findFileDatabases(filepath.Join(mount, "missing"), 4, 10); len(s)+len(d) != 0 {
		t.Fatalf("a missing mount: %v %v", s, d)
	}
}
