package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

func TestEditRepositoryMovesTheData(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil || run.Status == catalog.RunFailed {
		t.Fatalf("first backup: %+v %v", run, err)
	}
	repo, err := c.Catalog.Repository(ctx, job.RepositoryID)
	if err != nil {
		t.Fatal(err)
	}
	oldPath := repo.Config.Filesystem.Path
	newPath := filepath.Join(filepath.Dir(oldPath), "moved", "repo")

	if _, err := c.EditRepository(ctx, repo.ID, RepoEdit{}); err == nil {
		t.Fatal("an edit that changes nothing was accepted")
	}

	// Something running on the repository refuses the edit and changes nothing.
	if !c.inflight.start(job.ID) {
		t.Fatal("could not mark the job running")
	}
	if _, err := c.EditRepository(ctx, repo.ID, RepoEdit{FSPath: newPath}); !errors.Is(err, ErrRunning) {
		t.Fatalf("edit while a job runs: %v", err)
	}
	c.inflight.done(job.ID)
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("a refused edit moved the data: %v", err)
	}
	if c.IsRunning(job.ID) || c.IsRunning(repoMaintRunKey(repo.ID)) {
		t.Fatal("an edit left an in-flight key held")
	}

	// A destination that exists is refused, and nothing moves or is saved.
	if err := os.MkdirAll(newPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := c.EditRepository(ctx, repo.ID, RepoEdit{FSPath: newPath}); err == nil {
		t.Fatal("a move onto an existing directory was accepted")
	}
	if got, _ := c.Catalog.Repository(ctx, repo.ID); got.Config.Filesystem.Path != oldPath {
		t.Fatalf("a refused move changed the saved path to %s", got.Config.Filesystem.Path)
	}
	if err := os.Remove(newPath); err != nil {
		t.Fatal(err)
	}

	if _, err := c.EditRepository(ctx, repo.ID, RepoEdit{FSPath: newPath}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	got, err := c.Catalog.Repository(ctx, repo.ID)
	if err != nil || got.Config.Filesystem.Path != newPath {
		t.Fatalf("saved path: %+v %v", got.Config.Filesystem, err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("the old directory is still there: %v", err)
	}
	if c.IsRunning(job.ID) {
		t.Fatal("the edit left the job held")
	}

	// The job keeps working from the new place, with the same snapshots.
	if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil || run.Status == catalog.RunFailed {
		t.Fatalf("backup after the move: %+v %v", run, err)
	}
	snaps, err := c.Snapshots(ctx, job.ID)
	if err != nil || len(snaps) != 2 {
		t.Fatalf("snapshots after the move: %d %v", len(snaps), err)
	}
}

func TestEditRepositoryPages(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil || run.Status == catalog.RunFailed {
		t.Fatalf("backup: %+v %v", run, err)
	}
	repo, _ := c.Catalog.Repository(ctx, job.RepositoryID)
	oldPath := repo.Config.Filesystem.Path
	newPath := filepath.Join(filepath.Dir(oldPath), "viaform")

	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)

	cards := get(t, client, srv.URL+"/repositories", "repo-head")
	if !strings.Contains(cards, "/repositories/"+repo.ID+"/edit") || !strings.Contains(cards, oldPath) {
		t.Fatalf("the repository card has no edit link or location:\n%s", cards)
	}
	page := get(t, client, srv.URL+"/repositories/"+repo.ID+"/edit", "moves the repository")
	if !strings.Contains(page, oldPath) {
		t.Fatalf("the edit page does not show the current path:\n%s", page)
	}

	post(t, client, srv.URL+"/repositories/"+repo.ID+"/edit", url.Values{"fs_path": {newPath}})
	got, err := c.Catalog.Repository(ctx, repo.ID)
	if err != nil || got.Config.Filesystem.Path != newPath {
		t.Fatalf("the form did not save the new path: %+v %v", got.Config.Filesystem, err)
	}
	if entries, err := os.ReadDir(newPath); err != nil || len(entries) == 0 {
		t.Fatalf("the data is not at the new path: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("the old directory is still there: %v", err)
	}
	get(t, client, srv.URL+"/repositories", newPath)
}
