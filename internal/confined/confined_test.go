package confined

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestReadFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("deep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadFile(dir, "a.txt"); err != nil || string(b) != "hello" {
		t.Fatalf("a file in the directory: %q %v", b, err)
	}
	if b, err := ReadFile(dir, "sub/b.txt"); err != nil || string(b) != "deep" {
		t.Fatalf("a file in a subdirectory: %q %v", b, err)
	}
	// A missing file and a missing directory both read as "does not exist", so
	// callers that create a file on first use keep working.
	if _, err := ReadFile(dir, "nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing file: %v", err)
	}
	if _, err := ReadFile(filepath.Join(dir, "nodir"), "a.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing directory: %v", err)
	}
}

func TestReadFileRefusesWhatLeavesTheDirectory(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("do not read"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(secret, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "dirlink")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"link", "dirlink/secret", "../" + filepath.Base(outside) + "/secret", "../../etc/passwd"} {
		if b, err := ReadFile(dir, name); err == nil {
			t.Errorf("ReadFile(%q) escaped the directory and read %q", name, b)
		}
	}
	// A symlink that stays inside the directory is fine.
	if err := os.WriteFile(filepath.Join(dir, "real"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadFile(dir, "alias"); err != nil || string(b) != "ok" {
		t.Fatalf("a link inside the directory: %q %v", b, err)
	}
}
