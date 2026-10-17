package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kopia/kopia/repo"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

func cacheRepo(t *testing.T) Repo {
	t.Helper()
	return Repo{ID: "rc", Password: "pw", Config: repocfg.Config{
		Kind: repocfg.KindFilesystem, ECC: true,
		Filesystem: &repocfg.Filesystem{Path: filepath.Join(t.TempDir(), "repo")},
	}}
}

func connDir(e *Engine, r Repo) string { return filepath.Join(e.dir, r.ID+"-"+r.Config.Fingerprint()) }

// Kopia's library, unlike its command line, has no default cache size, and a size
// of zero means no cache at all: the engine has to say how big the cache is.
func TestCacheIsOnByDefaultAndLivesWithTheConnection(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, "host-a")
	r := cacheRepo(t)
	if _, _, err := e.Init(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetCachingOptions(ctx, filepath.Join(connDir(e, r), "repository.config"))
	if err != nil {
		t.Fatal(err)
	}
	def := DefaultCache()
	if got.CacheDirectory != filepath.Join(connDir(e, r), "cache") ||
		got.ContentCacheSizeBytes != def.ContentBytes || got.MetadataCacheSizeBytes != def.MetadataBytes {
		t.Fatalf("default caching options: %+v", got)
	}
}

func TestCacheSettingsApplyAndFollowChanges(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), "one")
	writeFile(t, filepath.Join(src, "sub", "b.txt"), "bee")

	base := filepath.Join(t.TempDir(), "ssd")
	e := newEngine(t, "host-a")
	if err := e.SetCache(CacheConfig{Dir: base, MetadataBytes: 8 << 20, ContentBytes: 4 << 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("SetCache did not create the base directory: %v", err)
	}
	r := cacheRepo(t)
	if _, _, err := e.Init(ctx, r); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(connDir(e, r), "repository.config")
	got, err := repo.GetCachingOptions(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	wantDir := filepath.Join(base, filepath.Base(connDir(e, r)))
	if got.CacheDirectory != wantDir || got.ContentCacheSizeBytes != 4<<20 || got.MetadataCacheSizeBytes != 8<<20 {
		t.Fatalf("caching options: %+v, want dir %s", got, wantDir)
	}
	// Both sizes are hard limits too: entries newer than the sweep age are kept
	// past a soft size, which would let the cache outgrow what was asked for.
	if got.ContentCacheSizeLimitBytes != 4<<20 || got.MetadataCacheSizeLimitBytes != 8<<20 {
		t.Fatalf("hard limits not set: %+v", got)
	}
	if _, err := e.Backup(ctx, r, []string{src}, nil, keepOne); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wantDir); err != nil {
		t.Fatalf("no cache directory after a backup: %v", err)
	}

	// A connection made with other settings (or none) follows the engine's the
	// next time the repository is opened.
	if err := e.SetCache(CacheConfig{Dir: base, MetadataBytes: 16 << 20, ContentBytes: 2 << 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ListSnapshots(ctx, r, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ = repo.GetCachingOptions(ctx, cfg); got.MetadataCacheSizeBytes != 16<<20 || got.ContentCacheSizeBytes != 2<<20 {
		t.Fatalf("settings were not applied to the existing connection: %+v", got)
	}

	// Zero turns the cache off.
	if err := e.SetCache(CacheConfig{Dir: base}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ListSnapshots(ctx, r, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ = repo.GetCachingOptions(ctx, cfg); got.CacheDirectory != "" || got.ContentCacheSizeBytes != 0 {
		t.Fatalf("cache not turned off: %+v", got)
	}
}

func TestCacheCoversAConnectionMadeWithoutOne(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t, "host-a")
	r := cacheRepo(t)
	// How every existing install was connected before the cache settings existed.
	if err := e.SetCache(CacheConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Init(ctx, r); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(connDir(e, r), "repository.config")
	if got, _ := repo.GetCachingOptions(ctx, cfg); got.CacheDirectory != "" {
		t.Fatalf("expected no cache, got %+v", got)
	}
	if err := e.SetCache(DefaultCache()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ListSnapshots(ctx, r, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.GetCachingOptions(ctx, cfg); got.ContentCacheSizeBytes != DefaultCache().ContentBytes || got.CacheDirectory == "" {
		t.Fatalf("an existing empty-cache connection was not upgraded: %+v", got)
	}
}

// The phases of a backup are all recorded: a missing one is the bug that made
// the earlier "commit" figure cover the whole session.
func TestBackupRecordsEveryPhase(t *testing.T) {
	ctx := context.Background()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), "one")
	e := newEngine(t, "host-a")
	r := cacheRepo(t)
	if _, _, err := e.Init(ctx, r); err != nil {
		t.Fatal(err)
	}
	res, err := e.Backup(ctx, r, []string{src}, nil, keepOne)
	if err != nil || res[0].Err != nil {
		t.Fatalf("backup: %+v %v", res, err)
	}
	tm := res[0].Timings
	if tm.Open <= 0 || tm.Prepare <= 0 || tm.Upload <= 0 || tm.Save <= 0 || tm.Flush <= 0 || tm.Close <= 0 || tm.Maintenance <= 0 {
		t.Fatalf("a phase was not recorded: %+v", tm)
	}
	if tm.Retention < 0 || tm.Commit() != tm.Save+tm.Flush+tm.Close {
		t.Fatalf("inconsistent timings: %+v", tm)
	}
	// A second run takes the unchanged file from the previous snapshot.
	res, err = e.Backup(ctx, r, []string{src}, nil, keepOne)
	if err != nil || res[0].CachedFiles != 1 {
		t.Fatalf("second backup: cached=%d %v", res[0].CachedFiles, err)
	}
}
