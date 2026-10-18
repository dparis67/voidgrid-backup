package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/engine"
)

// serverOnlyRepo sets a job up whose repository the server can open (through
// its mapped path) and its agent cannot (the path the agent is given leads
// nowhere), after one real backup.
func serverOnlyRepo(t *testing.T) (*Controller, catalog.Job, catalog.Repository, string) {
	t.Helper()
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil || run.Status == catalog.RunFailed {
		t.Fatalf("backup: %+v %v", run, err)
	}
	// The first backup of a job whose list was never checked starts a refresh
	// in the background. Let it finish before the agent's path is broken, or
	// it would run through the server and race the end of the test.
	for i := 0; ; i++ {
		if _, ok := c.SnapshotsSynced(ctx, job.ID); ok {
			break
		}
		if i > 400 {
			t.Fatal("the background snapshot refresh never finished")
		}
		time.Sleep(25 * time.Millisecond)
	}
	eng, err := engine.New(t.TempDir(), EngineHost)
	if err != nil {
		t.Fatal(err)
	}
	c.Engine = eng
	repo, _ := c.Catalog.Repository(ctx, job.RepositoryID)
	real := repo.Config.Filesystem.Path
	cfg := repo.Config
	f := *cfg.Filesystem
	f.ServerPath, f.Path = real, filepath.Join(t.TempDir(), "not", "here")
	cfg.Filesystem = &f
	if err := c.Catalog.UpdateRepositoryConfig(ctx, repo.ID, cfg); err != nil {
		t.Fatal(err)
	}
	repo, _ = c.Catalog.Repository(ctx, job.RepositoryID)
	snaps, err := c.Snapshots(ctx, job.ID)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots before: %d %v", len(snaps), err)
	}
	return c, job, repo, snaps[0].ID
}

func TestServerStandsInForReadsWhenTheAgentCannotReachTheRepository(t *testing.T) {
	ctx := context.Background()
	c, job, repo, _ := serverOnlyRepo(t)

	// The agent really cannot open it.
	if _, err := c.measureOnAgent(ctx, repo); err == nil {
		t.Fatal("the agent opened a repository at a path that leads nowhere")
	}
	st, err := c.RefreshRepoStats(ctx, repo.ID)
	if err != nil || st.Via != "the server" || st.StoredBytes == 0 || st.Blobs == 0 {
		t.Fatalf("stats through the server: %+v %v", st, err)
	}
	if saved, ok, _ := c.RepoStats(ctx, repo.ID); !ok || saved.Via != "the server" {
		t.Fatalf("the saved measurement does not say who made it: %+v", saved)
	}

	if err := c.RefreshSnapshots(ctx, job.ID); err != nil {
		t.Fatalf("refresh through the server: %v", err)
	}
	if got := c.SnapshotsVia(ctx, job.ID); got != "the server" {
		t.Fatalf("SnapshotsVia = %q", got)
	}
	if snaps, _ := c.Snapshots(ctx, job.ID); len(snaps) != 1 {
		t.Fatalf("snapshots after the refresh: %d", len(snaps))
	}

	// With no server engine it is an error, not a silent success.
	c.Engine = nil
	if _, err := c.RefreshRepoStats(ctx, repo.ID); err == nil {
		t.Fatal("stats succeeded with neither an agent nor the server able to open the repository")
	}
}

func TestDeleteAndWipeThroughTheServerAreExplicit(t *testing.T) {
	ctx := context.Background()
	c, job, repo, sid := serverOnlyRepo(t)

	// A delete never falls back by itself.
	if err := c.DeleteSnapshot(ctx, job.ID, sid); err == nil {
		t.Fatal("the agent deleted a snapshot of a repository it cannot open")
	}
	if err := c.DeleteSnapshotOn(ctx, job.ID, sid, "bogus"); err == nil {
		t.Fatal("an unknown place to delete from was accepted")
	}
	if snaps, _ := c.Snapshots(ctx, job.ID); len(snaps) != 1 {
		t.Fatalf("a refused delete changed the record: %d", len(snaps))
	}
	if err := c.DeleteSnapshotOn(ctx, job.ID, sid, ServerHost); err != nil {
		t.Fatalf("delete through the server: %v", err)
	}
	if snaps, _ := c.Snapshots(ctx, job.ID); len(snaps) != 0 {
		t.Fatalf("the snapshot is still in the record: %d", len(snaps))
	}
	er, _ := serverRepo(repo)
	if left, err := c.Engine.ListSnapshotsOf(ctx, er, job.AgentID, nil); err != nil || len(left) != 0 {
		t.Fatalf("the snapshot is still in the repository: %d %v", len(left), err)
	}
	// The server acts for the job's agent only: another host's snapshot is refused.
	if err := c.Engine.DeleteSnapshotOf(ctx, er, "someone-else", "does-not-matter"); err == nil {
		t.Fatal("deleting as another host was accepted")
	}

	// Wipe through the server: refused while a job uses the repository, and
	// when the server cannot open it; allowed once it can and none does.
	if err := c.StartWipe(ctx, repo.ID, ServerHost); err == nil {
		t.Fatal("a wipe started while a job still uses the repository")
	}
	if err := c.Catalog.DeleteJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	engineWas := c.Engine
	c.Engine = nil
	if err := c.StartWipe(ctx, repo.ID, ServerHost); err == nil {
		t.Fatal("a wipe through the server started with no server engine")
	}
	c.Engine = engineWas
	if err := c.StartWipe(ctx, repo.ID, ServerHost); err != nil {
		t.Fatalf("wipe through the server: %v", err)
	}
	for i := 0; ; i++ {
		if _, err := c.Catalog.Repository(ctx, repo.ID); errors.Is(err, catalog.ErrNotFound) {
			break
		}
		if i > 400 {
			t.Fatalf("the repository was not wiped and removed (wipe error: %q)", c.WipeError(ctx, repo.ID))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestCheckAccessAndThePagesOfferTheServer(t *testing.T) {
	ctx := context.Background()
	c, job, repo, _ := serverOnlyRepo(t)

	results, err := c.CheckReach(ctx, repo.ID)
	if err != nil || len(results) != 2 {
		t.Fatalf("reach: %+v %v", results, err)
	}
	var agentOK, serverOK bool
	for _, r := range results {
		if r.Who == "the server" {
			serverOK = r.OK
		} else {
			agentOK = r.OK
		}
	}
	if agentOK || !serverOK {
		t.Fatalf("expected the server only: %+v", results)
	}
	if text := reachText(repo.Name, results); !strings.Contains(text, "cannot open it") || !strings.Contains(text, "the server: can open it") {
		t.Fatalf("notice: %s", text)
	}

	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	get(t, client, srv.URL+"/repositories", "Check access")

	// The delete form offers the server only once the agent is known to be down.
	if err := c.Catalog.RecordFailure(ctx, job.AgentID, "connection refused"); err != nil {
		t.Fatal(err)
	}
	jobPage := get(t, client, srv.URL+"/jobs/"+job.ID, "Delete")
	if !strings.Contains(jobPage, `<option value="server">through the server</option>`) {
		t.Fatalf("an unreachable agent's snapshot delete does not offer the server:\n%s", jobPage)
	}
	if err := c.Catalog.RecordSeen(ctx, job.AgentID, time.Now(), "h", "v", nil); err != nil {
		t.Fatal(err)
	}
	if jobPage = get(t, client, srv.URL+"/jobs/"+job.ID, "Delete"); strings.Contains(jobPage, `<option value="server">through the server</option>`) {
		t.Fatalf("a reachable agent still offers the server for deletes:\n%s", jobPage)
	}

	// The wipe form is there once no job uses the repository, and offers the server.
	if err := c.Catalog.DeleteJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	page := get(t, client, srv.URL+"/repositories", "Wipe all its data")
	if !strings.Contains(page, `<option value="server">the server</option>`) {
		t.Fatalf("the wipe form does not offer the server:\n%s", page)
	}
}
