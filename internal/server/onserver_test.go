package server

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/engine"
)

// mapRepoToServer gives the controller an engine and tells it where the
// filesystem repository is mounted on the server (the same path in tests).
func mapRepoToServer(t *testing.T, c *Controller, repoID string) {
	t.Helper()
	eng, err := engine.New(t.TempDir(), EngineHost)
	if err != nil {
		t.Fatal(err)
	}
	c.Engine = eng
	repo, err := c.Catalog.Repository(context.Background(), repoID)
	if err != nil {
		t.Fatal(err)
	}
	f := *repo.Config.Filesystem
	f.ServerPath = f.Path
	repo.Config.Filesystem = &f
	if err := c.Catalog.UpdateRepositoryConfig(context.Background(), repoID, repo.Config); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreOnServer(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil || run.Status != catalog.RunSuccess {
		t.Fatalf("backup run: %+v %v", run, err)
	}
	snaps, err := c.Snapshots(ctx, job.ID)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots: %+v %v", snaps, err)
	}

	// Without a mapping the server cannot open a filesystem repository.
	repo, _ := c.Catalog.Repository(ctx, job.RepositoryID)
	if err := c.ServerCanOpen(repo); err == nil {
		t.Fatal("an unmapped filesystem repository was openable by the server")
	}
	if _, err := c.Browse(ctx, job.ID, ServerHost, snaps[0].ID, ""); err == nil {
		t.Fatal("browse on the server without an engine or mapping")
	}

	mapRepoToServer(t, c, job.RepositoryID)
	repo, _ = c.Catalog.Repository(ctx, job.RepositoryID)
	if err := c.ServerCanOpen(repo); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	page := srv.URL + "/jobs/" + job.ID + "/snapshots/" + snaps[0].ID
	if body := get(t, client, page, "The server"); strings.Contains(body, "(unavailable)") {
		t.Fatal("the server is offered as unavailable though it can open the repository")
	}
	body := get(t, client, page+"?on=server", "inside its own container")
	// Choosing the server says up front that it does not run as root, when it does not.
	if uid, gid, can := c.Engine.RestoreIdentity(); can {
		if strings.Contains(body, "does not run as root") {
			t.Fatal("a server that can restore ownership warns that it cannot")
		}
	} else if !strings.Contains(body, "The server does not run as root") ||
		!strings.Contains(body, fmt.Sprintf("uid:gid %d:%d", uid, gid)) {
		t.Fatalf("no warning that the server restores as its own user:\n%s", body)
	}
	root, err := c.Browse(ctx, job.ID, ServerHost, snaps[0].ID, "")
	if err != nil || len(root) != 1 || root[0].GetName() != "docs" {
		t.Fatalf("browse on the server: %+v %v", root, err)
	}
	target := filepath.Join(t.TempDir(), "restored")
	rr, err := c.Restore(ctx, job.ID, ServerHost, snaps[0].ID, "docs", target, false)
	if err != nil || !restoreOK(rr) || !strings.Contains(rr.Summary, " on the server") {
		t.Fatalf("restore on the server: %+v %v", rr, err)
	}
	if b, err := os.ReadFile(filepath.Join(target, "note.txt")); err != nil || string(b) != "hello" {
		t.Fatalf("restored note.txt: %q %v", b, err)
	}
	// A restore that could not set ownership is complete, not partial, and says whose files they are.
	if uid, gid, can := c.Engine.RestoreIdentity(); !can {
		want := fmt.Sprintf("complete, files belong to uid:gid %d:%d (the server does not run as root", uid, gid)
		if rr.Status != catalog.RunSuccess || rr.Error != "" || !strings.Contains(rr.Summary, want) {
			t.Fatalf("a restore as the server's own user: status=%s error=%q summary=%q", rr.Status, rr.Error, rr.Summary)
		}
	} else if strings.Contains(rr.Summary, "uid:gid") {
		t.Fatalf("ownership was restored but the summary says it was not: %q", rr.Summary)
	}
	// System directories are refused as on an agent.
	if _, err := c.Restore(ctx, job.ID, ServerHost, snaps[0].ID, "docs", "/", false); err == nil {
		t.Fatal("restore to / accepted")
	}
}
