package engine

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/blob"
	"github.com/kopia/kopia/snapshot"
	"github.com/kopia/kopia/snapshot/snapshotfs"

	"github.com/voidgrid/voidgrid-backup/internal/tidy"
)

// CheckOptions controls a repository check.
type CheckOptions struct {
	// VerifyPercent of file contents is downloaded, decrypted and hashed
	// (0-100). All snapshot metadata and directory listings are always
	// checked, as is the existence of every content blob.
	VerifyPercent float64
	// TestRestoreMaxBytes caps the test restore: the newest snapshot of this
	// host's smallest source at or under the cap is restored to a scratch
	// directory and compared with its recorded counts. 0 skips it.
	TestRestoreMaxBytes int64
}

type CheckResult struct {
	Snapshots        int
	ObjectsChecked   int64
	FilesRead        int64
	BytesRead        int64
	Errors           []string // first verify errors
	TestRestore      string   // what was test-restored, "" if skipped
	TestRestoreError string
}

// OK reports whether the check found nothing wrong.
func (r CheckResult) OK() bool { return len(r.Errors) == 0 && r.TestRestoreError == "" }

// Check verifies the repository and, optionally, does a test restore.
func (e *Engine) Check(ctx context.Context, r Repo, opt CheckOptions) (CheckResult, error) {
	var res CheckResult
	rep, err := e.open(ctx, r)
	if err != nil {
		return res, err
	}
	defer tidy.CloseCtx(ctx, "repository", rep)

	ids, err := snapshot.ListSnapshotManifests(ctx, rep, nil, nil)
	if err != nil {
		return res, err
	}
	mans, err := snapshot.LoadSnapshots(ctx, rep, ids)
	if err != nil {
		return res, err
	}
	res.Snapshots = len(mans)

	pct := opt.VerifyPercent
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	vopts := snapshotfs.VerifierOptions{VerifyFilesPercent: pct, MaxErrors: 20, Parallelism: 4}
	if dr, ok := rep.(repo.DirectRepository); ok {
		bm, err := blob.ReadBlobMap(ctx, dr.BlobReader())
		if err != nil {
			return res, fmt.Errorf("read blob list: %w", err)
		}
		vopts.BlobMap = bm
	}
	v := snapshotfs.NewVerifier(ctx, rep, vopts)
	vres, verr := v.InParallel(ctx, func(tw *snapshotfs.TreeWalker) error {
		for _, m := range mans {
			if m.RootEntry == nil {
				continue
			}
			root, err := snapshotfs.SnapshotRoot(rep, m)
			if err != nil {
				return err
			}
			name := fmt.Sprintf("%v@%v", m.Source, m.StartTime.ToTime().Format(time.RFC3339))
			if err := tw.Process(ctx, root, name); err != nil {
				// Errors found while walking are already counted by the
				// walker; only a refusal before the walk starts (an entry
				// with no object ID) is news.
				if _, n := tw.GetErrors(); n == 0 {
					tw.ReportError(ctx, name, err)
				}
			}
		}
		return nil
	})
	res.ObjectsChecked = vres.Stats.ProcessedObjectCount
	res.FilesRead = vres.Stats.ReadFileCount
	res.BytesRead = vres.Stats.ReadBytes
	res.Errors = append(res.Errors, vres.ErrorStrings...)
	if len(res.Errors) == 0 && verr != nil {
		res.Errors = append(res.Errors, verr.Error())
	}
	if len(res.Errors) > 10 {
		res.Errors = append(res.Errors[:10], fmt.Sprintf("... and %d more", len(res.Errors)-10))
	}

	if opt.TestRestoreMaxBytes > 0 {
		res.TestRestore, res.TestRestoreError = e.testRestore(ctx, rep, mans, opt.TestRestoreMaxBytes)
	}
	return res, nil
}

// testRestore restores the newest snapshot of this host's smallest source
// under maxBytes and checks the file count and total size match what the
// snapshot recorded. The scratch copy is always removed.
func (e *Engine) testRestore(ctx context.Context, rep repo.Repository, mans []*snapshot.Manifest, maxBytes int64) (what, problem string) {
	newest := map[snapshot.SourceInfo]*snapshot.Manifest{}
	for _, m := range mans {
		if m.Source.Host != e.hostname || m.RootEntry == nil || m.IncompleteReason != "" {
			continue
		}
		if cur := newest[m.Source]; cur == nil || m.StartTime.After(cur.StartTime) {
			newest[m.Source] = m
		}
	}
	var pick *snapshot.Manifest
	var pickSize int64
	for _, m := range newest {
		if _, size := recordedTotals(m); size > maxBytes {
			continue
		}
		if _, size := recordedTotals(m); pick == nil || size < pickSize {
			pick, pickSize = m, size
		}
	}
	if pick == nil {
		return "", ""
	}
	wantFiles, wantBytes := recordedTotals(pick)
	what = fmt.Sprintf("%s snapshot %s (%d files, %d bytes)", pick.Source.Path, shortManifest(pick), wantFiles, wantBytes)

	scratch := filepath.Join(e.dir, "test-restore-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	defer tidy.RemoveAll(scratch)
	root, err := snapshotfs.SnapshotRoot(rep, pick)
	if err != nil {
		return what, err.Error()
	}
	if _, err := e.restoreTo(ctx, rep, root, scratch, false); err != nil {
		return what, "restore failed: " + err.Error()
	}
	files, bytes, err := countFiles(scratch)
	if err != nil {
		return what, err.Error()
	}
	if files != wantFiles || bytes != wantBytes {
		return what, fmt.Sprintf("restored %d files / %d bytes, snapshot recorded %d / %d",
			files, bytes, wantFiles, wantBytes)
	}
	return what, ""
}

// recordedTotals is the file count and size of everything in the snapshot.
// The manifest's Stats.TotalFileCount counts only the files this run did not
// take from the previous snapshot, so it is 0 for a backup where nothing
// changed; the root directory's summary counts the whole tree.
func recordedTotals(m *snapshot.Manifest) (files, bytes int64) {
	if m.RootEntry != nil && m.RootEntry.DirSummary != nil {
		return m.RootEntry.DirSummary.TotalFileCount, m.RootEntry.DirSummary.TotalFileSize
	}
	return int64(m.Stats.TotalFileCount), m.Stats.TotalFileSize
}

func countFiles(dir string) (files, bytes int64, err error) {
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err != nil {
				return err
			}
			files++
			bytes += fi.Size()
		}
		return nil
	})
	return files, bytes, err
}

func shortManifest(m *snapshot.Manifest) string {
	id := string(m.ID)
	if len(id) > 8 {
		id = id[:8]
	}
	return id
}

// Recovery configures the engine for the standalone restore tool: the
// repository is opened read-only.
func (e *Engine) Recovery() { e.readOnly = true }

// HostSnapshot is a snapshot with the host it came from.
type HostSnapshot struct {
	Snapshot
	Host string
}

// ListAllSnapshots lists every snapshot in the repository, newest first.
func (e *Engine) ListAllSnapshots(ctx context.Context, r Repo) ([]HostSnapshot, error) {
	rep, err := e.open(ctx, r)
	if err != nil {
		return nil, err
	}
	defer tidy.CloseCtx(ctx, "repository", rep)
	ids, err := snapshot.ListSnapshotManifests(ctx, rep, nil, nil)
	if err != nil {
		return nil, err
	}
	mans, err := snapshot.LoadSnapshots(ctx, rep, ids)
	if err != nil {
		return nil, err
	}
	out := make([]HostSnapshot, 0, len(mans))
	for _, m := range mans {
		out = append(out, HostSnapshot{Host: m.Source.Host, Snapshot: Snapshot{
			ID: string(m.ID), Path: m.Source.Path,
			Start: m.StartTime.ToTime().Unix(), End: m.EndTime.ToTime().Unix(),
			Bytes: m.Stats.TotalFileSize, Files: int(m.Stats.TotalFileCount),
			Errors: int(m.Stats.ErrorCount), Incomplete: m.IncompleteReason,
		}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start > out[j].Start })
	return out, nil
}
