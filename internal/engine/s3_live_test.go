//go:build s3live

package engine

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/kopia/kopia/repo/blob"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

// liveS3 reads the connection scripts/s3-live-test.sh passes in and returns a
// repository config under its own sub-prefix of the throwaway prefix, with a
// cleanup that deletes every object left under it, whatever the test did.
func liveS3(t *testing.T, sub string) (repocfg.Config, func()) {
	t.Helper()
	endpoint := os.Getenv("VB_LIVE_S3_ENDPOINT")
	bucket := os.Getenv("VB_LIVE_S3_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("VB_LIVE_S3_ENDPOINT, VB_LIVE_S3_BUCKET (and region/keys/prefix) not set")
	}
	prefix := os.Getenv("VB_LIVE_S3_PREFIX")
	if prefix == "" {
		t.Fatal("VB_LIVE_S3_PREFIX must be set to a throwaway prefix (scripts/s3-live-test.sh sets one)")
	}
	insecure, _ := strconv.ParseBool(os.Getenv("VB_LIVE_S3_INSECURE"))
	cfg := repocfg.Config{Kind: repocfg.KindS3, S3: &repocfg.S3{
		Endpoint:        endpoint,
		Bucket:          bucket,
		Prefix:          prefix + sub,
		Region:          os.Getenv("VB_LIVE_S3_REGION"),
		AccessKeyID:     os.Getenv("VB_LIVE_S3_ACCESS_KEY"),
		SecretAccessKey: os.Getenv("VB_LIVE_S3_SECRET_KEY"),
		Insecure:        insecure,
	}}
	return cfg, func() {
		ctx := context.Background()
		st, err := storage(ctx, cfg, false)
		if err != nil {
			t.Logf("cleanup: open storage: %v", err)
			return
		}
		defer st.Close(ctx)
		var ids []blob.ID
		if err := st.ListBlobs(ctx, "", func(m blob.Metadata) error {
			ids = append(ids, m.BlobID)
			return nil
		}); err != nil {
			t.Logf("cleanup: list blobs: %v", err)
			return
		}
		for _, id := range ids {
			if err := st.DeleteBlob(ctx, id); err != nil {
				t.Logf("cleanup: delete %s: %v", id, err)
			}
		}
		t.Logf("cleanup: removed %d leftover blobs under prefix %s", len(ids), cfg.S3.Prefix)
	}
}

// TestLiveS3Suite exercises the engine against a real S3-compatible bucket
// (Backblaze B2's S3 API in practice), in one repository so that it stays within
// a free account's daily request allowance: a few kilobytes of fake data, no
// repeated initialisation. It covers what an S3 repository does that SFTP and a
// local directory do not prove: creating and re-connecting, incremental
// backups, listing, browsing, full verification with a test restore, a
// byte-for-byte restore, deleting a snapshot, measuring, maintenance, a wrong
// secret key being refused without writing anything, and wiping.
//
// An S3 repository is never moved: there is no rename on S3, and the edit form
// does not offer to change a bucket, endpoint or prefix.
func TestLiveS3Suite(t *testing.T) {
	ctx := context.Background()
	cfg, cleanup := liveS3(t, "suite/")
	defer cleanup()
	r := Repo{ID: "live-s3", Password: "correct-horse-battery-staple", Config: cfg}
	e := newEngine(t, "live-s3-host")

	// Create it, then connect to what is already there.
	created, _, err := e.Init(ctx, r)
	if err != nil || !created {
		t.Fatalf("init: created=%v err=%v", created, err)
	}
	if created, _, err := e.Init(ctx, r); err != nil || created {
		t.Fatalf("a second init must connect, not create: created=%v err=%v", created, err)
	}

	// A wrong secret key is refused and nothing is written with it.
	bad := r
	badS3 := *cfg.S3
	badS3.SecretAccessKey = "not-the-secret"
	bad.Config.S3 = &badS3
	if _, err := e.Inspect(ctx, bad); err == nil {
		t.Fatal("a wrong secret key opened the repository")
	}

	// Fake data: a few small files and one 64 KiB file that does not compress.
	src := t.TempDir()
	files := map[string]string{
		"a.txt":     "first file",
		"dir/b.txt": "second file, in a directory",
		"dir/c.txt": "third file",
	}
	big := make([]byte, 64<<10)
	rand.New(rand.NewSource(1)).Read(big)
	files["random.bin"] = string(big)
	for rel, content := range files {
		writeFile(t, filepath.Join(src, rel), content)
	}
	keep := Retention{Latest: 10}

	first, err := e.Backup(ctx, r, []string{src}, nil, keep)
	if err != nil || len(first) != 1 || first[0].Err != nil {
		t.Fatalf("backup 1: %+v %v", first, err)
	}
	t.Logf("backup 1: open=%s upload=%s", first[0].Timings.Open, first[0].Timings.Upload)

	// Nothing changed: every file is taken from the previous snapshot.
	second, err := e.Backup(ctx, r, []string{src}, nil, keep)
	if err != nil || second[0].Err != nil || second[0].CachedFiles != len(files) {
		t.Fatalf("backup 2 should reuse all %d files: %+v %v", len(files), second, err)
	}
	t.Logf("backup 2: open=%s upload=%s cached=%d", second[0].Timings.Open, second[0].Timings.Upload, second[0].CachedFiles)

	// One new file: only it is read.
	files["new.txt"] = "added later"
	writeFile(t, filepath.Join(src, "new.txt"), files["new.txt"])
	third, err := e.Backup(ctx, r, []string{src}, nil, keep)
	if err != nil || third[0].Err != nil || third[0].CachedFiles != len(files)-1 {
		t.Fatalf("backup 3 should read only the new file: %+v %v", third, err)
	}

	// Listing and browsing.
	snaps, err := e.ListSnapshots(ctx, r, []string{src})
	if err != nil || len(snaps) != 3 {
		t.Fatalf("snapshots: %d %v", len(snaps), err)
	}
	entries, err := e.ListDirectory(ctx, r, third[0].SnapshotID, "")
	if err != nil || len(entries) == 0 {
		t.Fatalf("browse: %+v %v", entries, err)
	}

	// Full verification with a test restore (an unchanged newest snapshot used
	// to fail this: the count compared must be the whole tree's).
	check, err := e.Check(ctx, r, CheckOptions{VerifyPercent: 100, TestRestoreMaxBytes: 1 << 20})
	if err != nil || !check.OK() {
		t.Fatalf("check: %+v %v", check, err)
	}
	if check.TestRestore == "" {
		t.Fatalf("check did not test-restore: %+v", check)
	}

	// Restore the newest snapshot and compare every file.
	out := filepath.Join(t.TempDir(), "out")
	if _, err := e.Restore(ctx, r, third[0].SnapshotID, "", out, false); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for rel, want := range files {
		if got := readFile(t, filepath.Join(out, rel)); got != want {
			t.Fatalf("%s differs after the restore", rel)
		}
	}

	// Delete the oldest snapshot.
	if err := e.DeleteSnapshot(ctx, r, first[0].SnapshotID); err != nil {
		t.Fatalf("delete snapshot: %v", err)
	}
	if left, _ := e.ListSnapshots(ctx, r, []string{src}); len(left) != 2 {
		t.Fatalf("snapshots after the delete: %d", len(left))
	}

	// Measure it, and run a full maintenance cycle as its owner.
	st, err := e.RepoStats(ctx, r)
	if err != nil || st.Blobs == 0 || st.StoredBytes == 0 || st.FSTotal != 0 {
		t.Fatalf("stats: %+v %v", st, err)
	}
	t.Logf("stats: %d blobs, %d bytes", st.Blobs, st.StoredBytes)
	ran, owner, err := e.Maintain(ctx, r)
	if err != nil || !ran {
		t.Fatalf("maintenance: ran=%v owner=%q err=%v", ran, owner, err)
	}

	// Wipe it: every blob goes, and the location no longer holds a repository.
	blobs, bytes, err := e.Wipe(ctx, r)
	if err != nil || blobs == 0 || bytes == 0 {
		t.Fatalf("wipe: blobs=%d bytes=%d err=%v", blobs, bytes, err)
	}
	t.Logf("wipe removed %d blobs, %d bytes", blobs, bytes)
	if _, err := e.TestRepository(ctx, r); err == nil {
		t.Fatal("the wiped location still opens as a repository")
	}
}
