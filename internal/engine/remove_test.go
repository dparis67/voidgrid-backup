package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

func removeFixture(t *testing.T) (*Engine, Repo, string, string) {
	t.Helper()
	repoDir := filepath.Join(t.TempDir(), "repo")
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), "one")
	e := newEngine(t, "host-a")
	r := Repo{ID: "r1", Password: "pw", Config: repocfg.Config{
		Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: repoDir}}}
	if _, _, err := e.Init(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return e, r, src, repoDir
}

func TestDeleteSnapshot(t *testing.T) {
	ctx := context.Background()
	e, r, src, _ := removeFixture(t)
	res, err := e.Backup(ctx, r, []string{src}, nil, Retention{Latest: 10})
	if err != nil || res[0].Err != nil {
		t.Fatalf("backup: %+v %v", res, err)
	}
	id := res[0].SnapshotID
	if err := e.DeleteSnapshot(ctx, r, id); err != nil {
		t.Fatal(err)
	}
	snaps, err := e.ListSnapshots(ctx, r, []string{src})
	if err != nil || len(snaps) != 0 {
		t.Fatalf("snapshot still listed: %+v %v", snaps, err)
	}
	if err := e.DeleteSnapshot(ctx, r, id); err == nil {
		t.Fatal("deleting a snapshot twice must fail")
	}
}

func TestDeleteSnapshotRefusesOtherHosts(t *testing.T) {
	ctx := context.Background()
	e, r, src, _ := removeFixture(t)
	res, err := e.Backup(ctx, r, []string{src}, nil, Retention{Latest: 10})
	if err != nil || res[0].Err != nil {
		t.Fatalf("backup: %+v %v", res, err)
	}
	other := newEngine(t, "host-b")
	err = other.DeleteSnapshot(ctx, r, res[0].SnapshotID)
	if err == nil || !strings.Contains(err.Error(), "not to this agent") {
		t.Fatalf("a snapshot of another host must be refused, got %v", err)
	}
	if snaps, _ := e.ListSnapshots(ctx, r, []string{src}); len(snaps) != 1 {
		t.Fatal("the snapshot must survive a refused delete")
	}
}

func TestWipe(t *testing.T) {
	ctx := context.Background()
	e, r, src, repoDir := removeFixture(t)
	if res, err := e.Backup(ctx, r, []string{src}, nil, keepOne); err != nil || res[0].Err != nil {
		t.Fatalf("backup: %+v %v", res, err)
	}
	stray := filepath.Join(repoDir, "notes.txt")
	writeFile(t, stray, "not a kopia file")

	blobs, bytes, err := e.Wipe(ctx, r)
	if err != nil || blobs == 0 || bytes == 0 {
		t.Fatalf("wipe: blobs=%d bytes=%d err=%v", blobs, bytes, err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("a file that is not a Kopia blob must survive: %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(e.dir, "r1-*")); len(m) != 0 {
		t.Fatalf("local cache not removed: %v", m)
	}
	if _, _, err := e.Wipe(ctx, r); err == nil || !strings.Contains(err.Error(), "nothing was deleted") {
		t.Fatalf("a second wipe must refuse, got %v", err)
	}
	// The location can host a new repository afterwards.
	if created, _, err := e.Init(ctx, r); err != nil || !created {
		t.Fatalf("re-init after wipe: created=%v err=%v", created, err)
	}
}

func TestWipeRemovesACacheKeptApartFromTheConnection(t *testing.T) {
	ctx := context.Background()
	e, r, src, _ := removeFixture(t)
	base := filepath.Join(t.TempDir(), "cache")
	if err := e.SetCache(CacheConfig{Dir: base, MetadataBytes: 8 << 20, ContentBytes: 4 << 20}); err != nil {
		t.Fatal(err)
	}
	otherCache := filepath.Join(base, "r9-keep")
	writeFile(t, filepath.Join(otherCache, "x"), "another repository's cache")
	// removeFixture connected before the cache moved; a fresh connection puts
	// its cache under base.
	if err := os.RemoveAll(connDir(e, r)); err != nil {
		t.Fatal(err)
	}
	if res, err := e.Backup(ctx, r, []string{src}, nil, keepOne); err != nil || res[0].Err != nil {
		t.Fatalf("backup: %+v %v", res, err)
	}
	if m, _ := filepath.Glob(filepath.Join(base, "r1-*")); len(m) != 1 {
		t.Fatalf("expected the cache under %s, got %v", base, m)
	}

	if _, _, err := e.Wipe(ctx, r); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(filepath.Join(base, "r1-*")); len(m) != 0 {
		t.Fatalf("cache kept apart from the connection not removed: %v", m)
	}
	if _, err := os.Stat(otherCache); err != nil {
		t.Fatalf("another repository's cache must survive: %v", err)
	}
}

func TestWipeRefusesWhereThereIsNoRepository(t *testing.T) {
	e := newEngine(t, "host-a")
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep.txt")
	writeFile(t, keep, "x")
	r := Repo{ID: "r2", Config: repocfg.Config{Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: dir}}}
	if _, _, err := e.Wipe(context.Background(), r); err == nil {
		t.Fatal("wipe of a directory with no repository must fail")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal(err)
	}
}

func TestMaintainRunsForTheOwner(t *testing.T) {
	ctx := context.Background()
	e, r, _, _ := removeFixture(t) // Init made this client the maintenance owner
	ran, owner, err := e.Maintain(ctx, r)
	if err != nil || !ran || owner != "" {
		t.Fatalf("owner: ran=%v owner=%q err=%v", ran, owner, err)
	}
	other := newEngine(t, "host-b")
	ran, owner, err = other.Maintain(ctx, r)
	if err != nil || ran || !strings.HasSuffix(owner, "@host-a") {
		t.Fatalf("non-owner: ran=%v owner=%q err=%v", ran, owner, err)
	}
}

func TestDeleteSnapshotsBatch(t *testing.T) {
	ctx := context.Background()
	e, r, src, _ := removeFixture(t)
	var ids []string
	for i := 0; i < 2; i++ {
		res, err := e.Backup(ctx, r, []string{src}, nil, Retention{Latest: 10})
		if err != nil || res[0].Err != nil {
			t.Fatalf("backup %d: %+v %v", i, res, err)
		}
		ids = append(ids, res[0].SnapshotID)
		time.Sleep(1100 * time.Millisecond) // snapshots need distinct start times
	}
	// Another host's snapshots refuse the whole batch and delete nothing.
	other := newEngine(t, "host-b")
	if err := other.DeleteSnapshots(ctx, r, ids); err == nil || !strings.Contains(err.Error(), "not to this agent") {
		t.Fatalf("a batch of another host's snapshots must be refused, got %v", err)
	}
	if snaps, _ := e.ListSnapshots(ctx, r, []string{src}); len(snaps) != 2 {
		t.Fatalf("a refused batch deleted snapshots: %d left, want 2", len(snaps))
	}
	// A snapshot that is already gone is skipped, and the rest are deleted.
	if err := e.DeleteSnapshots(ctx, r, append([]string{"already-gone"}, ids...)); err != nil {
		t.Fatalf("a batch with a missing snapshot: %v", err)
	}
	if snaps, err := e.ListSnapshots(ctx, r, []string{src}); err != nil || len(snaps) != 0 {
		t.Fatalf("snapshots still listed: %+v %v", snaps, err)
	}
	if err := e.DeleteSnapshots(ctx, r, nil); err != nil {
		t.Fatalf("an empty batch is nothing to do: %v", err)
	}
	if err := e.DeleteSnapshots(ctx, r, ids); err != nil {
		t.Fatalf("a batch of snapshots that are all gone: %v", err)
	}
	if err := e.DeleteSnapshot(ctx, r, ids[0]); err == nil {
		t.Fatal("deleting a single missing snapshot is still an error")
	}
}
