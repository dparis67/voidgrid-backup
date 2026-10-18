//go:build s3live

package server

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

// liveS3Config is the connection scripts/s3-live-test.sh passes in, under a
// sub-prefix of the throwaway prefix.
func liveS3Config(t *testing.T, sub string) repocfg.Config {
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
	return repocfg.Config{Kind: repocfg.KindS3, S3: &repocfg.S3{
		Endpoint: endpoint, Bucket: bucket, Prefix: prefix + sub, Region: os.Getenv("VB_LIVE_S3_REGION"),
		AccessKeyID: os.Getenv("VB_LIVE_S3_ACCESS_KEY"), SecretAccessKey: os.Getenv("VB_LIVE_S3_SECRET_KEY"),
		Insecure: insecure,
	}}
}

// waitGone waits for a repository's record to disappear (a wipe removes it
// when the data is gone).
func waitGone(t *testing.T, c *Controller, repoID string) {
	t.Helper()
	for i := 0; ; i++ {
		if _, err := c.Catalog.Repository(context.Background(), repoID); errors.Is(err, catalog.ErrNotFound) {
			return
		}
		if i > 1200 {
			t.Fatalf("the repository was not wiped and removed (wipe error: %q)", c.WipeError(context.Background(), repoID))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestLiveS3Server runs the server's own S3 paths against a real bucket, with a
// real in-process agent and the server's engine, in two small repositories (one
// added through the agent, one created by the server). The data is a few
// kilobytes, so it fits a free account's daily allowance. It checks: adding
// through an agent, who can open it, the card's note, a job backed up twice,
// measuring, maintenance by the agent, a credentials edit that is refused when
// the new secret does not work and leaves the saved settings alone, deleting a
// snapshot through the agent and then through the server, wiping through the
// agent, and creating a repository from the server, maintaining it and wiping
// it through the server.
func TestLiveS3Server(t *testing.T) {
	ctx := context.Background()
	c, seed, _ := setupJob(t) // a controller with an enrolled in-process agent
	withServerEngine(t, c)
	agentID := seed.AgentID
	if err := c.Catalog.DeleteJob(ctx, seed.ID); err != nil {
		t.Fatal(err)
	}
	agentCfg, serverCfg := liveS3Config(t, "agent/"), liveS3Config(t, "server/")

	// Leave nothing behind if the test fails partway: wipe whatever repositories
	// are still recorded, through the server's own engine.
	var recorded []string
	defer func() {
		for _, id := range recorded {
			repo, err := c.Catalog.Repository(ctx, id)
			if err != nil {
				continue
			}
			if er, err := serverRepo(repo); err == nil {
				if _, _, err := c.Engine.Wipe(ctx, er); err != nil {
					t.Logf("cleanup: wipe %s: %v", repo.Name, err)
				}
			}
		}
	}()

	// Add it through the agent.
	repo, _, err := c.AddRepository(ctx, "s3agent", agentCfg, "", agentID)
	if err != nil {
		t.Fatalf("add through the agent: %v", err)
	}
	recorded = append(recorded, repo.ID)
	if ids, _ := c.Catalog.RepositoryAgents(ctx, repo.ID); len(ids) != 1 || ids[0] != agentID {
		t.Fatalf("assigned agents: %v", ids)
	}

	// Who can open it: the agent and the server, and the card says so.
	results, err := c.CheckReach(ctx, repo.ID)
	if err != nil || len(results) != 2 {
		t.Fatalf("reach: %+v %v", results, err)
	}
	for _, r := range results {
		if !r.OK {
			t.Fatalf("%s cannot open the S3 repository: %s", r.Who, r.Detail)
		}
	}
	if text, warn := c.ServerReachNote(ctx, repo); warn || !strings.Contains(text, "The server can open this repository") {
		t.Fatalf("the card's note: %q warn=%v", text, warn)
	}

	// A job, backed up twice: the second run reuses the first's files.
	src := t.TempDir()
	for name, content := range map[string]string{"one.txt": "first", "two.txt": "second"} {
		if err := os.WriteFile(src+"/"+name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	job, err := c.AddJob(ctx, JobInput{Name: "s3job", AgentID: agentID, RepositoryID: repo.ID, Paths: []string{src},
		Schedule: "0 3 * * *", Keep: catalog.DefaultRetention, Enabled: true})
	if err != nil {
		t.Fatalf("add job: %v", err)
	}
	for i := 1; i <= 2; i++ {
		if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil || run.Status == catalog.RunFailed {
			t.Fatalf("backup %d: %+v %v", i, run, err)
		}
	}
	// The first backup of a job starts a refresh of its list in the background;
	// let it finish before looking at the list.
	for i := 0; ; i++ {
		if _, ok := c.SnapshotsSynced(ctx, job.ID); ok {
			break
		}
		if i > 400 {
			t.Fatal("the background snapshot refresh never finished")
		}
		time.Sleep(50 * time.Millisecond)
	}
	snaps, err := c.Snapshots(ctx, job.ID)
	if err != nil || len(snaps) != 2 {
		t.Fatalf("snapshots: %d %v", len(snaps), err)
	}

	// Measure it, and run maintenance as its owner (the agent that created it).
	st, err := c.RefreshRepoStats(ctx, repo.ID)
	if err != nil || st.Blobs == 0 || st.Via != "" {
		t.Fatalf("stats: %+v %v", st, err)
	}
	if _, err := c.MaintainRepository(ctx, repo.ID); err != nil {
		t.Fatalf("maintenance: %v", err)
	}

	// Credentials: an edit that changes nothing is refused; a secret that does
	// not work is refused and the saved settings are untouched.
	if _, err := c.EditRepository(ctx, repo.ID, RepoEdit{S3AccessKey: agentCfg.S3.AccessKeyID}); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("an edit that changes nothing: %v", err)
	}
	if _, err := c.EditRepository(ctx, repo.ID, RepoEdit{S3Secret: "not-the-secret"}); err == nil || !strings.Contains(err.Error(), "new settings") {
		t.Fatalf("a secret that does not work: %v", err)
	}
	if got, _ := c.Catalog.Repository(ctx, repo.ID); got.Config.S3.SecretAccessKey != agentCfg.S3.SecretAccessKey {
		t.Fatal("a refused edit changed the saved secret")
	}

	// Delete one snapshot through the agent, the other through the server.
	if err := c.DeleteSnapshot(ctx, job.ID, snaps[0].ID); err != nil {
		t.Fatalf("delete through the agent: %v", err)
	}
	if err := c.DeleteSnapshotOn(ctx, job.ID, snaps[1].ID, ServerHost); err != nil {
		t.Fatalf("delete through the server: %v", err)
	}
	if left, _ := c.Snapshots(ctx, job.ID); len(left) != 0 {
		t.Fatalf("snapshots left: %d", len(left))
	}

	// Wipe it through the agent.
	if err := c.Catalog.DeleteJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.StartWipe(ctx, repo.ID, agentID); err != nil {
		t.Fatalf("wipe through the agent: %v", err)
	}
	waitGone(t, c, repo.ID)

	// Create one from the server: it maintains it, and wipes it too.
	own, _, err := c.AddRepository(ctx, "s3server", serverCfg, "", ServerHost)
	if err != nil {
		t.Fatalf("create from the server: %v", err)
	}
	recorded = append(recorded, own.ID)
	if got := c.MaintainedBy(ctx, own.ID); got != MaintByServer {
		t.Fatalf("maintained by %q", got)
	}
	if ids, _ := c.Catalog.RepositoryAgents(ctx, own.ID); len(ids) != 0 {
		t.Fatalf("agents assigned to a repository only the server opened: %v", ids)
	}
	if _, err := c.MaintainRepository(ctx, own.ID); err != nil {
		t.Fatalf("maintenance by the server: %v", err)
	}
	if err := c.StartWipe(ctx, own.ID, ServerHost); err != nil {
		t.Fatalf("wipe through the server: %v", err)
	}
	waitGone(t, c, own.ID)
}
