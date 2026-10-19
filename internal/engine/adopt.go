package engine

import (
	"context"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kopia/kopia/repo"

	"github.com/voidgrid/voidgrid-backup/internal/confined"
)

// identityFile, in a connection directory, holds the Kopia unique ID of the
// repository that connection was made to. It is written by the first open that
// succeeds, and is what lets a later connection prove that it is to the same
// repository before it takes over this one's cache.
const identityFile = "unique-id"

// recordIdentity writes the repository's unique ID into the connection
// directory if it is not there yet.
func recordIdentity(dir string, rep repo.Repository) {
	dr, ok := rep.(repo.DirectRepository)
	if !ok {
		return
	}
	p := filepath.Join(dir, identityFile)
	if _, err := os.Stat(p); err == nil {
		return
	}
	if err := os.WriteFile(p, []byte(hex.EncodeToString(dr.UniqueID())), 0o600); err != nil {
		slog.Warn("record the repository's identity", "dir", dir, "err", err)
	}
}

// adoptCache hands the cache of an earlier connection to r's repository to the
// connection in dir, which was just made. An earlier connection is the same
// repository ID under another config fingerprint, as after its path or
// credentials were edited. Kopia keys cached entries by blob and content ID,
// which a relocated repository keeps, so the cache stays valid and the first
// run after the edit need not read the index and directories from storage
// again.
//
// The same repository ID can also be opened with settings that lead to a
// different repository (a wrong path that happens to open), whose cache must
// never be served for this one. So the cache moves only when the earlier
// connection recorded a repository ID and the new connection is to the
// repository with that ID; without a record nothing is adopted.
func (e *Engine) adoptCache(ctx context.Context, r Repo, dir, cfgFile string) {
	if e.cache.ContentBytes <= 0 {
		return
	}
	olds, _ := filepath.Glob(filepath.Join(e.dir, r.ID+"-*"))
	var from, fromConn, fromID string
	var newest time.Time
	for _, old := range olds {
		if old == dir {
			continue
		}
		id, err := confined.ReadFile(old, identityFile)
		if err != nil {
			continue
		}
		c := e.cachingFor(old).CacheDirectory
		fi, err := os.Stat(c)
		if err != nil || !fi.IsDir() {
			continue
		}
		if from == "" || fi.ModTime().After(newest) {
			newest, from, fromConn, fromID = fi.ModTime(), c, old, strings.TrimSpace(string(id))
		}
	}
	if from == "" {
		return
	}
	rep, err := repo.Open(ctx, cfgFile, r.Password, &repo.Options{})
	if err != nil {
		return
	}
	dr, ok := rep.(repo.DirectRepository)
	same := ok && hex.EncodeToString(dr.UniqueID()) == fromID
	if err := rep.Close(ctx); err != nil {
		slog.Warn("close the repository after checking its identity", "repo", r.ID, "err", err)
	}
	if !same {
		return
	}
	want := e.cachingFor(dir).CacheDirectory
	if err := os.RemoveAll(want); err != nil {
		slog.Warn("keep the repository cache across the edit", "dir", want, "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(want), 0o700); err != nil {
		return
	}
	if err := os.Rename(from, want); err != nil {
		slog.Warn("keep the repository cache across the edit", "from", from, "to", want, "err", err)
		return
	}
	slog.Info("repository cache kept across a settings change", "repo", r.ID)
	if err := os.RemoveAll(fromConn); err != nil {
		slog.Warn("remove the replaced repository connection", "dir", fromConn, "err", err)
	}
}
