package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/repo/format"
	"github.com/kopia/kopia/repo/maintenance"
	"github.com/kopia/kopia/repo/manifest"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotmaintenance"

	"github.com/voidgrid/voidgrid-backup/internal/tidy"
)

// DeleteSnapshot forgets one snapshot. Only this agent's own snapshots can be
// deleted. The data it referenced stays in storage until a later full
// maintenance cycle garbage-collects it.
func (e *Engine) DeleteSnapshot(ctx context.Context, r Repo, snapshotID string) error {
	return e.deleteSnapshots(ctx, r, []string{snapshotID}, false, e.hostname)
}

// DeleteSnapshotsOf forgets several snapshots that host made in one repository
// session, skipping any already gone. If any of them belongs to another host
// none is deleted.
func (e *Engine) DeleteSnapshotsOf(ctx context.Context, r Repo, host string, snapshotIDs []string) error {
	return e.deleteSnapshots(ctx, r, snapshotIDs, true, host)
}

// DeleteSnapshotOf forgets one snapshot that host made, which is how the server
// deletes an agent's snapshot through its own engine when the agent cannot
// reach the repository. A snapshot of any other host is refused.
func (e *Engine) DeleteSnapshotOf(ctx context.Context, r Repo, host, snapshotID string) error {
	return e.deleteSnapshots(ctx, r, []string{snapshotID}, false, host)
}

// DeleteSnapshots forgets several snapshots in one repository session. A
// snapshot that is already gone is skipped, since the aim is that it is not
// there (the server's list can be behind the repository). Every snapshot that
// is there is checked to be this agent's own before any is deleted, so a
// refusal leaves them all in place.
func (e *Engine) DeleteSnapshots(ctx context.Context, r Repo, snapshotIDs []string) error {
	return e.deleteSnapshots(ctx, r, snapshotIDs, true, e.hostname)
}

func (e *Engine) deleteSnapshots(ctx context.Context, r Repo, snapshotIDs []string, skipMissing bool, host string) error {
	if len(snapshotIDs) == 0 {
		return nil
	}
	rep, err := e.open(ctx, r)
	if err != nil {
		return err
	}
	defer tidy.CloseCtx(ctx, "repository", rep)

	ids := make([]manifest.ID, 0, len(snapshotIDs))
	for _, sid := range snapshotIDs {
		id := manifest.ID(sid)
		man, err := snapshot.LoadSnapshot(ctx, rep, id)
		if skipMissing && errors.Is(err, snapshot.ErrSnapshotNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("load snapshot %s: %w", sid, err)
		}
		if man.Source.Host != host || man.Source.UserName != Username {
			return fmt.Errorf("snapshot %s belongs to %s@%s, not to this agent", sid, man.Source.UserName, man.Source.Host)
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	return repo.WriteSession(ctx, rep, repo.WriteSessionOptions{Purpose: "voidgrid-backup:delete-snapshot"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			for _, id := range ids {
				if err := w.DeleteManifest(ctx, id); err != nil {
					return err
				}
			}
			return nil
		})
}

// Wipe deletes every blob of the repository at r's storage location and this
// agent's local cache of it. It needs no repository password. It refuses when
// no repository is found there (a wrong path would otherwise delete
// nothing, or something else), and deletes the repository's format blob last,
// so a wipe that fails halfway can be run again. Returns what was deleted.
// Only Kopia's own blob files are listed, so unrelated files survive, and
// the empty directories of a file-based store remain.
func (e *Engine) Wipe(ctx context.Context, r Repo) (blobs, bytes int64, err error) {
	if err := r.Config.Validate(); err != nil {
		return 0, 0, err
	}
	if err := noRepositoryAt(ctx, r.Config); errors.Is(err, errNoRepository) {
		return 0, 0, errors.New("no repository was found at this location; nothing was deleted")
	} else if err != nil {
		return 0, 0, err
	}
	st, err := storage(ctx, r.Config, false)
	if err != nil {
		return 0, 0, err
	}
	defer tidy.CloseCtx(ctx, "storage", st)

	if _, err := st.GetMetadata(ctx, format.KopiaRepositoryBlobID); errors.Is(err, blob.ErrBlobNotFound) {
		return 0, 0, errors.New("no repository was found at this location; nothing was deleted")
	} else if err != nil {
		return 0, 0, fmt.Errorf("look for the repository: %w", err)
	}

	var all []blob.Metadata
	if err := st.ListBlobs(ctx, "", func(bm blob.Metadata) error {
		all = append(all, bm)
		return ctx.Err()
	}); err != nil {
		return 0, 0, fmt.Errorf("list blobs: %w", err)
	}
	sort.SliceStable(all, func(i, j int) bool {
		return all[j].BlobID == format.KopiaRepositoryBlobID && all[i].BlobID != format.KopiaRepositoryBlobID
	})
	for i, bm := range all {
		if err := ctx.Err(); err != nil {
			return blobs, bytes, err
		}
		if err := st.DeleteBlob(ctx, bm.BlobID); err != nil && !errors.Is(err, blob.ErrBlobNotFound) {
			return blobs, bytes, fmt.Errorf("delete %s after %d of %d blobs: %w", bm.BlobID, i, len(all), err)
		}
		blobs++
		bytes += bm.Length
		if blobs%1000 == 0 {
			slog.Info("repository wipe progress", "repo", r.ID, "deleted", blobs, "of", len(all))
		}
	}

	// Drop this agent's cached connection(s) to it, and its cache when that
	// lives apart from the connection (a configured cache directory).
	stale, _ := filepath.Glob(filepath.Join(e.dir, r.ID+"-*"))
	if e.cache.Dir != "" {
		apart, _ := filepath.Glob(filepath.Join(e.cache.Dir, r.ID+"-*"))
		stale = append(stale, apart...)
	}
	for _, d := range stale {
		if err := os.RemoveAll(d); err != nil {
			slog.Warn("remove local repository cache", "dir", d, "err", err)
		}
	}
	return blobs, bytes, nil
}

// Maintain runs a full maintenance cycle now. Kopia lets only the repository's
// maintenance owner do it: when this agent is not the owner nothing runs and
// the owner ("user@host", the host being the owning agent's ID) is returned.
// A full cycle does not free recently deleted data at once: Kopia's safety
// delays still apply.
func (e *Engine) Maintain(ctx context.Context, r Repo) (ran bool, owner string, err error) {
	rep, err := e.open(ctx, r)
	if err != nil {
		return false, "", err
	}
	defer tidy.CloseCtx(ctx, "repository", rep)
	dr, ok := rep.(repo.DirectRepository)
	if !ok {
		return false, "", errors.New("repository does not support maintenance")
	}
	if err := claimMaintenance(ctx, rep); err != nil {
		return false, "", err
	}
	err = repo.DirectWriteSession(ctx, dr, repo.WriteSessionOptions{Purpose: "voidgrid-backup:maintenance"},
		func(ctx context.Context, dw repo.DirectRepositoryWriter) error {
			return snapshotmaintenance.Run(ctx, dw, maintenance.ModeFull, false, maintenance.SafetyFull)
		})
	var notOwner maintenance.NotOwnedError
	if errors.As(err, &notOwner) {
		return false, notOwner.Owner, nil
	}
	return err == nil, "", err
}
