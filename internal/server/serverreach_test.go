package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/engine"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
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

func TestDeleteAJobWithItsSnapshotsWhenItsAgentIsDown(t *testing.T) {
	ctx := context.Background()
	c, job, repo, _ := serverOnlyRepo(t)

	// Deleting the job alone needs no agent at all, so it is not tested here;
	// with the snapshots it needs somewhere that can open the repository.
	if _, err := c.DeleteJobOn(ctx, job.ID, true, ""); err == nil {
		t.Fatal("the job and its snapshots were deleted through an agent that cannot open the repository")
	}
	if _, err := c.DeleteJobOn(ctx, job.ID, true, "bogus"); err == nil {
		t.Fatal("an unknown place to delete from was accepted")
	}
	if _, err := c.Catalog.Job(ctx, job.ID); err != nil {
		t.Fatalf("a refused delete removed the job: %v", err)
	}
	if snaps, _ := c.Snapshots(ctx, job.ID); len(snaps) != 1 {
		t.Fatalf("a refused delete changed the snapshot record: %d", len(snaps))
	}

	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	if err := c.Catalog.RecordFailure(ctx, job.AgentID, "connection refused"); err != nil {
		t.Fatal(err)
	}
	page := get(t, client, srv.URL+"/jobs/"+job.ID, "Delete the snapshots through")
	if !strings.Contains(page, `<option value="server">the server</option>`) {
		t.Fatalf("the job's delete form does not offer the server while its agent is down:\n%s", page)
	}

	// Through the form, naming the server.
	post(t, client, srv.URL+"/jobs/"+job.ID+"/delete", url.Values{"confirm": {job.Name}, "snapshots": {"1"}, "on": {"server"}})
	if _, err := c.Catalog.Job(ctx, job.ID); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("the job is still there: %v", err)
	}
	er, _ := serverRepo(repo)
	if left, err := c.Engine.ListSnapshotsOf(ctx, er, job.AgentID, nil); err != nil || len(left) != 0 {
		t.Fatalf("the snapshots are still in the repository: %d %v", len(left), err)
	}
}

func TestServerReachNoteOnTheCard(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	repo, _ := c.Catalog.Repository(ctx, job.RepositoryID)

	// A server with no engine says nothing at all.
	if text, _ := c.ServerReachNote(ctx, repo); text != "" {
		t.Fatalf("a note without an engine: %q", text)
	}
	withServerEngine(t, c)

	// A filesystem directory with no path on the server is a warning at once.
	text, warn := c.ServerReachNote(ctx, repo)
	if !warn || !strings.Contains(text, "The server cannot open this repository") {
		t.Fatalf("an unmapped directory: %q warn=%v", text, warn)
	}

	// Mapped but never checked: a plain line pointing at Check access.
	cfg := repo.Config
	f := *cfg.Filesystem
	f.ServerPath = f.Path
	cfg.Filesystem = &f
	if err := c.Catalog.UpdateRepositoryConfig(ctx, repo.ID, cfg); err != nil {
		t.Fatal(err)
	}
	repo, _ = c.Catalog.Repository(ctx, repo.ID)
	if text, warn = c.ServerReachNote(ctx, repo); warn || !strings.Contains(text, "not been checked") {
		t.Fatalf("not checked yet: %q warn=%v", text, warn)
	}

	// Checked and fine.
	if _, err := c.CheckReach(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if text, warn = c.ServerReachNote(ctx, repo); warn || !strings.Contains(text, "The server can open this repository (checked") {
		t.Fatalf("checked and reachable: %q warn=%v", text, warn)
	}

	// Checked and failing (the mapping now points nowhere): a warning with why.
	f.ServerPath = filepath.Join(t.TempDir(), "gone")
	cfg.Filesystem = &f
	if err := c.Catalog.UpdateRepositoryConfig(ctx, repo.ID, cfg); err != nil {
		t.Fatal(err)
	}
	repo, _ = c.Catalog.Repository(ctx, repo.ID)
	if _, err := c.CheckReach(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if text, warn = c.ServerReachNote(ctx, repo); !warn || !strings.Contains(text, "could not open this repository when last checked") {
		t.Fatalf("checked and unreachable: %q warn=%v", text, warn)
	}

	// The card shows it.
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	get(t, client, srv.URL+"/repositories", "could not open this repository when last checked")
}

func TestAddingARepositoryChecksTheServerInTheBackground(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	withServerEngine(t, c)
	dir := filepath.Join(t.TempDir(), "viaagent")
	cfg := repocfg.Config{Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: dir, ServerPath: dir}}
	repo, _, err := c.AddRepository(ctx, "viaagent", cfg, "", job.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		if text, _ := c.ServerReachNote(ctx, repo); strings.Contains(text, "The server can open this repository (checked") {
			return
		}
		if i > 400 {
			t.Fatal("the server was never checked after the repository was added")
		}
		time.Sleep(25 * time.Millisecond)
	}
}
