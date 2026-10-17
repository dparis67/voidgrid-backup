package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

// hasFormatBlob reports whether dir still holds a Kopia repository (its
// format blob is stored as kopia.repository plus a file extension).
func hasFormatBlob(t *testing.T, dir string) bool {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "kopia.repository*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(m) > 0
}

func TestDeleteSnapshot(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	for i := 0; i < 2; i++ {
		if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil {
			t.Fatalf("backup %d: %+v %v", i, run, err)
		}
		time.Sleep(1100 * time.Millisecond) // snapshots need distinct start times
	}
	snaps, err := c.Catalog.ListSnapshots(ctx, job.ID)
	if err != nil || len(snaps) != 2 {
		t.Fatalf("snapshots: %+v %v", snaps, err)
	}
	if err := c.DeleteSnapshot(ctx, job.ID, "not-a-snapshot"); err == nil {
		t.Fatal("an unknown snapshot must be refused")
	}
	if err := c.DeleteSnapshot(ctx, job.ID, snaps[0].ID); err != nil {
		t.Fatal(err)
	}
	left, err := c.Catalog.ListSnapshots(ctx, job.ID)
	if err != nil || len(left) != 1 || left[0].ID != snaps[1].ID {
		t.Fatalf("after delete: %+v %v", left, err)
	}
	// The repository agrees: a refresh from it lists one snapshot.
	listed, err := c.Snapshots(ctx, job.ID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("repository still lists the deleted snapshot: %+v %v", listed, err)
	}
}

func TestRemoveRepositoryKeepsItsData(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if _, err := c.RunJob(ctx, job.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	repo, err := c.Catalog.Repository(ctx, job.RepositoryID)
	if err != nil {
		t.Fatal(err)
	}

	var inUse RepoInUseError
	if err := c.RemoveRepository(ctx, repo.ID); !errors.As(err, &inUse) || len(inUse.Jobs) != 1 {
		t.Fatalf("a repository with jobs must not be removed, got %v", err)
	}
	if err := c.Catalog.DeleteJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveRepository(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Catalog.Repository(ctx, repo.ID); err == nil {
		t.Fatal("record still there")
	}
	if !hasFormatBlob(t, repo.Config.Filesystem.Path) {
		t.Fatal("removing the record must not touch the data")
	}
}

func TestWipeRepository(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if _, err := c.RunJob(ctx, job.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	repo, err := c.Catalog.Repository(ctx, job.RepositoryID)
	if err != nil {
		t.Fatal(err)
	}

	var inUse RepoInUseError
	if err := c.StartWipe(ctx, repo.ID, job.AgentID); !errors.As(err, &inUse) {
		t.Fatalf("a repository with jobs must not be wiped, got %v", err)
	}
	if err := c.Catalog.DeleteJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.StartWipe(ctx, repo.ID, job.AgentID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for c.IsRunning(repoWipeKey(repo.ID)) {
		if time.Now().After(deadline) {
			t.Fatal("wipe did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := c.Catalog.Repository(ctx, repo.ID); err == nil {
		t.Fatalf("record still there, wipe error: %q", c.WipeError(ctx, repo.ID))
	}
	if hasFormatBlob(t, repo.Config.Filesystem.Path) {
		t.Fatal("repository data still in storage")
	}
}

func TestWipeFailureKeepsTheRecord(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	repo, err := c.Catalog.Repository(ctx, job.RepositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Catalog.DeleteJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	// Point at where the data used to be: nothing to wipe there.
	if err := os.RemoveAll(repo.Config.Filesystem.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repo.Config.Filesystem.Path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.StartWipe(ctx, repo.ID, job.AgentID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for c.IsRunning(repoWipeKey(repo.ID)) {
		if time.Now().After(deadline) {
			t.Fatal("wipe did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := c.Catalog.Repository(ctx, repo.ID); err != nil {
		t.Fatalf("a failed wipe must keep the record: %v", err)
	}
	if got := c.WipeError(ctx, repo.ID); !strings.Contains(got, "nothing was deleted") {
		t.Fatalf("wipe error not recorded: %q", got)
	}
}

// Every destructive route insists on the typed name or ID before it does anything.
func TestRemovalsNeedTheTypedName(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if _, err := c.RunJob(ctx, job.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	snaps, err := c.Catalog.ListSnapshots(ctx, job.ID)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots: %+v %v", snaps, err)
	}
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	post := func(path string, form url.Values) string {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.Request.URL.Query().Get("notice")
	}

	if n := post("/jobs/"+job.ID+"/snapshots/"+snaps[0].ID+"/delete", url.Values{"confirm": {"nope"}}); !strings.Contains(n, "Type the snapshot ID") {
		t.Fatalf("snapshot delete without the ID: %q", n)
	}
	if n := post("/repositories/"+job.RepositoryID+"/remove", url.Values{"confirm": {"nope"}}); !strings.Contains(n, "Type the repository name") {
		t.Fatalf("remove without the name: %q", n)
	}
	if n := post("/repositories/"+job.RepositoryID+"/wipe", url.Values{"confirm": {"nope"}, "agent": {job.AgentID}}); !strings.Contains(n, "Type the repository name") {
		t.Fatalf("wipe without the name: %q", n)
	}
	if n := post("/repositories/"+job.RepositoryID+"/wipe", url.Values{"confirm": {"local"}, "agent": {job.AgentID}}); !strings.Contains(n, "still use this repository") {
		t.Fatalf("wipe of a repository with jobs: %q", n)
	}
	if left, _ := c.Catalog.ListSnapshots(ctx, job.ID); len(left) != 1 {
		t.Fatal("a refused request deleted a snapshot")
	}
	if _, err := c.Catalog.Repository(ctx, job.RepositoryID); err != nil {
		t.Fatalf("a refused request removed the repository: %v", err)
	}
	// With the right ID the snapshot goes.
	if n := post("/jobs/"+job.ID+"/snapshots/"+snaps[0].ID+"/delete", url.Values{"confirm": {shortID(snaps[0].ID)}}); !strings.Contains(n, "Deleted snapshot") {
		t.Fatalf("snapshot delete with the ID: %q", n)
	}
}

// A second job on the same agent, repository and path sees the first job's
// snapshots, so it shows what the repository really holds after a delete.
func sameSourceJob(t *testing.T, c *Controller, job catalog.Job) catalog.Job {
	t.Helper()
	other, err := c.AddJob(context.Background(), JobInput{
		Name: "docs-twin", AgentID: job.AgentID, RepositoryID: job.RepositoryID,
		Paths: job.Paths, Keep: catalog.DefaultRetention,
	})
	if err != nil {
		t.Fatal(err)
	}
	return other
}

func TestDeleteJobSnapshotsAreOptional(t *testing.T) {
	ctx := context.Background()
	for _, withSnapshots := range []bool{false, true} {
		c, job, _ := setupJob(t)
		twin := sameSourceJob(t, c, job)
		for i := 0; i < 2; i++ {
			if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil {
				t.Fatalf("backup %d: %+v %v", i, run, err)
			}
			time.Sleep(1100 * time.Millisecond)
		}
		n, err := c.DeleteJob(ctx, job.ID, withSnapshots)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Catalog.Job(ctx, job.ID); !errors.Is(err, catalog.ErrNotFound) {
			t.Fatalf("withSnapshots=%v: the job is still there: %v", withSnapshots, err)
		}
		if err := c.RefreshSnapshots(ctx, twin.ID); err != nil {
			t.Fatal(err)
		}
		listed, err := c.Snapshots(ctx, twin.ID)
		if err != nil {
			t.Fatal(err)
		}
		want, wantN := 2, 0
		if withSnapshots {
			want, wantN = 0, 2
		}
		if len(listed) != want || n != wantN {
			t.Errorf("withSnapshots=%v: forgot %d, repository lists %d; want %d and %d", withSnapshots, n, len(listed), wantN, want)
		}
	}
}

func TestDeleteJobRefusedWhileRunning(t *testing.T) {
	c, job, _ := setupJob(t)
	if !c.inflight.start(job.ID) {
		t.Fatal("could not mark the job running")
	}
	defer c.inflight.done(job.ID)
	if _, err := c.DeleteJob(context.Background(), job.ID, true); !errors.Is(err, ErrRunning) {
		t.Fatalf("got %v, want ErrRunning", err)
	}
	if _, err := c.Catalog.Job(context.Background(), job.ID); err != nil {
		t.Fatalf("a refused delete removed the job: %v", err)
	}
}

func TestDefaultRepositoryAndDeleteFormsOnThePages(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	get := func(path string) string {
		t.Helper()
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	post := func(path string, form url.Values) string {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.Request.URL.Query().Get("notice")
	}

	// The add-job form carries each agent's default, and makes the operator pick when there is none.
	if body := get("/jobs"); !strings.Contains(body, "data-defaults=") || !strings.Contains(body, job.RepositoryID) || !strings.Contains(body, "Choose a repository") {
		t.Errorf("the add-job form doesn't carry the defaults:\n%s", body)
	}
	// The Repositories page offers to change it, and the change is refused for an agent that isn't assigned.
	if body := get("/repositories"); !strings.Contains(body, ">default<") {
		t.Errorf("the Repositories page doesn't show the default")
	}
	if n := post("/repositories/"+job.RepositoryID+"/agents/nobody/default", nil); !strings.Contains(n, "Not changed") {
		t.Errorf("default for an unassigned agent: %q", n)
	}
	if n := post("/repositories/"+job.RepositoryID+"/agents/"+job.AgentID+"/default", nil); !strings.Contains(n, "now what new jobs") {
		t.Errorf("default for an assigned agent: %q", n)
	}

	// The job page offers the snapshots checkbox, and it is off unless ticked.
	if body := get("/jobs/" + job.ID); !strings.Contains(body, `name="snapshots"`) || strings.Contains(body, `name="snapshots" value="1" checked`) {
		t.Errorf("the delete form lacks an unticked snapshots checkbox")
	}
	if _, err := c.RunJob(ctx, job.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	if n := post("/jobs/"+job.ID+"/delete", url.Values{"confirm": {job.Name}, "snapshots": {"1"}}); !strings.Contains(n, "and 1 snapshot(s)") {
		t.Errorf("delete with snapshots: %q", n)
	}
}

// The job's snapshot list can be ahead of the repository (a snapshot removed
// outside the server, or by retention): deleting the job with its snapshots
// must not fail on the one that is already gone.
func TestDeleteJobWithAStaleSnapshotRecord(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil {
		t.Fatalf("backup: %+v %v", run, err)
	}
	ghost := catalog.Snapshot{ID: "d765d10f8345b3ebb04f94552800b745", Path: job.Paths[0], Start: time.Now(), End: time.Now()}
	if err := c.Catalog.UpsertSnapshots(ctx, job.ID, []catalog.Snapshot{ghost}, nil); err != nil {
		t.Fatal(err)
	}
	n, err := c.DeleteJob(ctx, job.ID, true)
	if err != nil {
		t.Fatalf("a stale record blocked the delete: %v", err)
	}
	if n != 2 {
		t.Errorf("forgot %d, want 2 (the real one and the record of the missing one)", n)
	}
	if _, err := c.Catalog.Job(ctx, job.ID); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("the job is still there: %v", err)
	}
}
