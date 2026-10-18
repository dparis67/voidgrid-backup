//go:build sftplive

package engine

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kopia/kopia/repo"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

// TestLiveSFTPPackBench measures what the maximum pack size does to an upload
// over a real SFTP server (a Storage Box). It writes 256 MiB of random data (it
// does not compress, so every byte is uploaded) into a fresh repository for each
// pack size and reports how long the upload took and how many blobs resulted.
// One run per size, on one link, with large files: it shows whether the pack
// size limits a transfer, not how a tree of many small files behaves. It only
// runs when VB_LIVE_BENCH is set; everything it writes is under a directory of
// its own, VB_LIVE_SFTP_PATH plus "-bench", removed afterwards. (An earlier
// version also timed one raw SFTP stream with pkg/sftp's File.Write; that sends
// one packet per round trip and took 20 minutes for 256 MiB, so it measured
// latency, not the link, and was dropped.)
func TestLiveSFTPPackBench(t *testing.T) {
	if os.Getenv("VB_LIVE_BENCH") == "" {
		t.Skip("set VB_LIVE_BENCH to run the pack size benchmark")
	}
	ctx := context.Background()
	host, user, base := os.Getenv("VB_LIVE_SFTP_HOST"), os.Getenv("VB_LIVE_SFTP_USER"), os.Getenv("VB_LIVE_SFTP_PATH")
	if host == "" || user == "" || base == "" {
		t.Skip("VB_LIVE_SFTP_HOST, VB_LIVE_SFTP_USER and VB_LIVE_SFTP_PATH not set")
	}
	port, err := strconv.Atoi(os.Getenv("VB_LIVE_SFTP_PORT"))
	if err != nil {
		port = 22
	}
	root := base + "-bench"
	if !strings.HasPrefix(root, "vb-live-test-") {
		t.Fatalf("refusing to work under %q: the path must be a throwaway vb-live-test-* directory", root)
	}
	knownHosts, err := FetchHostKey(ctx, host, port)
	if err != nil {
		t.Fatal(err)
	}
	cfgAt := func(p string) repocfg.Config {
		return repocfg.Config{Kind: repocfg.KindSFTP, SFTP: &repocfg.SFTP{
			Host: host, Port: port, User: user, Path: p, KnownHosts: knownHosts,
			KeyFile: os.Getenv("VB_LIVE_SFTP_KEY_FILE"), Password: os.Getenv("VB_LIVE_SFTP_PASSWORD"),
		}}
	}
	defer func() {
		cli, closeFn, err := dialSFTP(ctx, *cfgAt(root).SFTP)
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

	const mib = 1 << 20
	const files, perFile = 16, 16 * mib // 256 MiB
	src := t.TempDir()
	rng := rand.New(rand.NewSource(7))
	buf := make([]byte, perFile)
	for i := 0; i < files; i++ {
		rng.Read(buf)
		if err := os.WriteFile(filepath.Join(src, "f"+strconv.Itoa(i)+".bin"), buf, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	total := float64(files * perFile)

	for _, packMiB := range []int{20, 40, 80} {
		e := newEngine(t, "bench-host")
		r := Repo{ID: "bench" + strconv.Itoa(packMiB), Password: "pw", Config: cfgAt(root + "/pack" + strconv.Itoa(packMiB))}
		st, err := storage(ctx, r.Config, true)
		if err != nil {
			t.Fatal(err)
		}
		opts := &repo.NewRepositoryOptions{}
		opts.BlockFormat.MaxPackSize = packMiB * mib
		if err := repo.Initialize(ctx, st, opts, r.Password); err != nil {
			t.Fatal(err)
		}
		st.Close(ctx)

		t0 := time.Now()
		res, err := e.Backup(ctx, r, []string{src}, nil, keepOne)
		if err != nil || res[0].Err != nil {
			t.Fatalf("backup with %d MiB packs: %+v %v", packMiB, res, err)
		}
		whole := time.Since(t0)
		stats, err := e.RepoStats(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		up := res[0].Timings.Upload
		t.Logf("%d MiB packs: upload %s = %.1f MB/s (whole backup %s), %d blobs, %.0f MB stored",
			packMiB, up.Round(time.Millisecond), total/1e6/up.Seconds(), whole.Round(time.Millisecond), stats.Blobs, float64(stats.StoredBytes)/1e6)
	}
}
