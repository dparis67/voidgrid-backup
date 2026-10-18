package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cacheFiles lists the files under dir, relative to it.
func cacheFiles(t *testing.T, dir string) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			got[rel] = true
		}
		return nil
	})
	return got
}

func TestMovedRepositoryKeepsItsCache(t *testing.T) {
	for _, apart := range []bool{false, true} {
		name := "inside the connection"
		if apart {
			name = "in a cache directory"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			e, r, src, oldDir := removeFixture(t)
			if apart {
				if err := e.SetCache(CacheConfig{Dir: filepath.Join(t.TempDir(), "cache"), MetadataBytes: 8 << 20, ContentBytes: 4 << 20}); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(connDir(e, r)); err != nil { // reconnect with the cache apart
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				if res, err := e.Backup(ctx, r, []string{src}, nil, keepOne); err != nil || res[0].Err != nil {
					t.Fatalf("backup %d: %+v %v", i, res, err)
				}
			}
			oldConn := connDir(e, r)
			oldCache := e.cachingFor(oldConn).CacheDirectory
			before := cacheFiles(t, oldCache)
			if len(before) == 0 {
				t.Fatalf("no cache was written under %s", oldCache)
			}

			moved := Repo{ID: r.ID, Password: r.Password, Config: fsConfig(filepath.Join(filepath.Dir(oldDir), "moved"))}
			if err := e.MoveRepository(ctx, r.Config, moved.Config); err != nil {
				t.Fatal(err)
			}
			res, err := e.Backup(ctx, moved, []string{src}, nil, keepOne)
			if err != nil || res[0].Err != nil {
				t.Fatalf("backup after the move: %+v %v", res, err)
			}
			if res[0].CachedFiles != 1 {
				t.Fatalf("the backup after the move was not incremental: %+v", res[0])
			}

			newCache := e.cachingFor(connDir(e, moved)).CacheDirectory
			if newCache == oldCache {
				t.Fatal("the connection did not change, so nothing was tested")
			}
			after := cacheFiles(t, newCache)
			for f := range before {
				if !after[f] {
					t.Fatalf("cache entry %s did not follow the repository (%d before, %d after)", f, len(before), len(after))
				}
			}
			if _, err := os.Stat(oldConn); !os.IsNotExist(err) {
				t.Fatalf("the replaced connection was kept: %v", err)
			}
		})
	}
}

// The same repository ID opened with settings that lead to another repository
// (a wrong path that opens because the password matches) must not take over
// the first repository's cache: serving its index for the other one fails.
func TestCacheOfAnotherRepositoryIsNotAdopted(t *testing.T) {
	ctx := context.Background()
	e, r, src, oldDir := removeFixture(t)
	if res, err := e.Backup(ctx, r, []string{src}, nil, keepOne); err != nil || res[0].Err != nil {
		t.Fatalf("backup: %+v %v", res, err)
	}
	oldConn := connDir(e, r)
	oldCache := e.cachingFor(oldConn).CacheDirectory
	before := cacheFiles(t, oldCache)

	otherDir := filepath.Join(filepath.Dir(oldDir), "other")
	other := Repo{ID: r.ID, Password: r.Password, Config: fsConfig(otherDir)}
	if _, _, err := e.Init(ctx, Repo{ID: "elsewhere", Password: r.Password, Config: fsConfig(otherDir)}); err != nil {
		t.Fatal(err)
	}
	if res, err := e.Backup(ctx, other, []string{src}, nil, keepOne); err != nil || res[0].Err != nil {
		t.Fatalf("backup into the other repository: %+v %v", res, err)
	}

	if got := cacheFiles(t, oldCache); len(got) != len(before) {
		t.Fatalf("the first repository's cache was taken: %d files before, %d after", len(before), len(got))
	}
	if _, err := os.Stat(oldConn); err != nil {
		t.Fatalf("the first connection was removed: %v", err)
	}
	newCache := cacheFiles(t, e.cachingFor(connDir(e, other)).CacheDirectory)
	for f := range before {
		if strings.HasPrefix(f, "indexes/") && newCache[f] {
			t.Fatalf("the other repository's cache holds %s from the first one", f)
		}
	}
}

func TestNoCacheIsAdoptedWithoutARecordedIdentity(t *testing.T) {
	ctx := context.Background()
	e, r, src, oldDir := removeFixture(t)
	if res, err := e.Backup(ctx, r, []string{src}, nil, keepOne); err != nil || res[0].Err != nil {
		t.Fatalf("backup: %+v %v", res, err)
	}
	oldConn := connDir(e, r)
	if err := os.Remove(filepath.Join(oldConn, identityFile)); err != nil { // as if made before identities were recorded
		t.Fatal(err)
	}
	moved := Repo{ID: r.ID, Password: r.Password, Config: fsConfig(filepath.Join(filepath.Dir(oldDir), "moved"))}
	if err := e.MoveRepository(ctx, r.Config, moved.Config); err != nil {
		t.Fatal(err)
	}
	if res, err := e.Backup(ctx, moved, []string{src}, nil, keepOne); err != nil || res[0].Err != nil {
		t.Fatalf("backup after the move: %+v %v", res, err)
	}
	if _, err := os.Stat(oldConn); err != nil {
		t.Fatalf("a connection without a recorded identity was replaced: %v", err)
	}
}
