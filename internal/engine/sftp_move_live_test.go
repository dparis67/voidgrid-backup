//go:build sftplive

package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

// TestLiveSFTPMove moves a real Kopia repository on a real SFTP server (a
// Hetzner Storage Box) with the engine's own MoveRepository, then checks
// what a settings edit relies on: the data is intact under the new path, the
// first backup after the move is incremental, the agent's cache followed the
// repository, and nothing is left at the old path. It writes only fake data
// under a directory of its own, VB_LIVE_SFTP_PATH plus "-move", which it
// removes at the end whether or not the test passed.
func TestLiveSFTPMove(t *testing.T) {
	ctx := context.Background()
	host := os.Getenv("VB_LIVE_SFTP_HOST")
	user := os.Getenv("VB_LIVE_SFTP_USER")
	base := os.Getenv("VB_LIVE_SFTP_PATH")
	if host == "" || user == "" || base == "" {
		t.Skip("VB_LIVE_SFTP_HOST, VB_LIVE_SFTP_USER and VB_LIVE_SFTP_PATH not set")
	}
	port, err := strconv.Atoi(os.Getenv("VB_LIVE_SFTP_PORT"))
	if err != nil {
		port = 22
	}
	root := base + "-move"
	if !strings.HasPrefix(root, "vb-live-test-") {
		t.Fatalf("refusing to work under %q: the path must be a throwaway vb-live-test-* directory", root)
	}
	sftpAt := func(p, knownHosts string) repocfg.Config {
		return repocfg.Config{Kind: repocfg.KindSFTP, SFTP: &repocfg.SFTP{
			Host: host, Port: port, User: user, Path: p, KnownHosts: knownHosts,
			KeyFile:  os.Getenv("VB_LIVE_SFTP_KEY_FILE"),
			Password: os.Getenv("VB_LIVE_SFTP_PASSWORD"),
		}}
	}

	// Remove everything this test created, found or not, even on failure.
	var knownHosts string // pinned by Init, used for every later connection
	defer func() {
		cli, closeFn, err := dialSFTP(ctx, *sftpAt(root, knownHosts).SFTP)
		if err != nil {
			t.Logf("cleanup: connect: %v", err)
			return
		}
		defer closeFn()
		if err := cli.RemoveAll(root); err != nil {
			t.Logf("cleanup: remove %s: %v", root, err)
			return
		}
		t.Logf("cleanup: removed %s", root)
	}()

	e := newEngine(t, "live-move-host")
	if err := e.SetCache(CacheConfig{Dir: filepath.Join(t.TempDir(), "cache"), MetadataBytes: 64 << 20, ContentBytes: 16 << 20}); err != nil {
		t.Fatal(err)
	}

	oldCfg := sftpAt(root+"/before", "")
	r := Repo{ID: "livemove", Password: "correct-horse-battery-staple", Config: oldCfg}
	created, kh, err := e.Init(ctx, r)
	if err != nil || !created {
		t.Fatalf("init: created=%v err=%v", created, err)
	}
	knownHosts = kh
	r.Config = sftpAt(root+"/before", knownHosts)
	if knownHosts == "" {
		t.Fatal("init returned no host key to pin")
	}

	// Fake data: a few dozen small files in a few directories.
	src := t.TempDir()
	files := map[string]string{}
	for d := 0; d < 4; d++ {
		for i := 0; i < 8; i++ {
			rel := fmt.Sprintf("dir%d/file%d.txt", d, i)
			files[rel] = strings.Repeat(fmt.Sprintf("fake data %d/%d\n", d, i), 50+d*10+i)
			writeFile(t, filepath.Join(src, rel), files[rel])
		}
	}

	for i := 0; i < 2; i++ {
		res, err := e.Backup(ctx, r, []string{src}, nil, Retention{Latest: 10})
		if err != nil || res[0].Err != nil {
			t.Fatalf("backup %d: %+v %v", i, res, err)
		}
		t.Logf("backup %d before the move: open=%s upload=%s cached=%d", i, res[0].Timings.Open, res[0].Timings.Upload, res[0].CachedFiles)
	}
	beforeCache := cacheFiles(t, e.cachingFor(connDir(e, r)).CacheDirectory)
	if len(beforeCache) == 0 {
		t.Fatal("no cache was written before the move")
	}
	snapsBefore, err := e.ListSnapshots(ctx, r, []string{src})
	if err != nil || len(snapsBefore) != 2 {
		t.Fatalf("snapshots before the move: %d %v", len(snapsBefore), err)
	}

	// The move itself, on the Storage Box.
	moved := Repo{ID: r.ID, Password: r.Password, Config: sftpAt(root+"/after/deeper", knownHosts)}
	t0 := time.Now()
	if err := e.MoveRepository(ctx, r.Config, moved.Config); err != nil {
		t.Fatalf("MoveRepository on the real server: %v", err)
	}
	t.Logf("move took %s", time.Since(t0))

	// Nothing at the old path, the repository at the new one.
	if _, err := e.TestRepository(ctx, r); err == nil {
		t.Fatal("the old path still holds a repository")
	}
	if _, err := e.TestRepository(ctx, moved); err != nil {
		t.Fatalf("the repository does not open at the new path: %v", err)
	}
	// A move onto an existing directory is refused and changes nothing.
	if err := e.MoveRepository(ctx, moved.Config, sftpAt(root+"/after", knownHosts)); err == nil {
		t.Fatal("a move onto an existing directory was not refused")
	}
	if _, err := e.TestRepository(ctx, moved); err != nil {
		t.Fatalf("the refused move disturbed the repository: %v", err)
	}

	// Same snapshots, an incremental backup, and the cache came along.
	snapsAfter, err := e.ListSnapshots(ctx, moved, []string{src})
	if err != nil || len(snapsAfter) != 2 {
		t.Fatalf("snapshots after the move: %d %v", len(snapsAfter), err)
	}
	res, err := e.Backup(ctx, moved, []string{src}, nil, Retention{Latest: 10})
	if err != nil || res[0].Err != nil {
		t.Fatalf("backup after the move: %+v %v", res, err)
	}
	t.Logf("backup after the move: open=%s upload=%s cached=%d of %d", res[0].Timings.Open, res[0].Timings.Upload, res[0].CachedFiles, len(files))
	if res[0].CachedFiles != len(files) {
		t.Fatalf("the backup after the move re-read files: cached %d of %d", res[0].CachedFiles, len(files))
	}
	afterCache := cacheFiles(t, e.cachingFor(connDir(e, moved)).CacheDirectory)
	for f := range beforeCache {
		if !afterCache[f] {
			t.Fatalf("cache entry %s did not follow the repository (%d before, %d after)", f, len(beforeCache), len(afterCache))
		}
	}
	if _, err := os.Stat(connDir(e, r)); !os.IsNotExist(err) {
		t.Fatalf("the replaced connection was kept: %v", err)
	}

	// Everything restores, byte for byte, from the moved repository.
	check, err := e.Check(ctx, moved, CheckOptions{VerifyPercent: 100, TestRestoreMaxBytes: 1 << 20})
	if err != nil || !check.OK() {
		t.Fatalf("check after the move: %+v %v", check, err)
	}
	out := filepath.Join(t.TempDir(), "out")
	if _, err := e.Restore(ctx, moved, snapsAfter[0].ID, "", out, false); err != nil {
		t.Fatal(err)
	}
	for rel, want := range files {
		if got := readFile(t, filepath.Join(out, rel)); got != want {
			t.Fatalf("%s differs after the move", rel)
		}
	}
}
