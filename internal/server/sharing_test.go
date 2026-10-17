package server

import (
	"context"
	"errors"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

func TestSharingARepository(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	b, addrB := startAgent(t, t.TempDir())
	agB, err := enrollTestAgent(t, c, b, "box2", addrB)
	if err != nil {
		t.Fatal(err)
	}
	newJob := func(name string) (catalog.Job, error) {
		return c.AddJob(ctx, JobInput{Name: name, AgentID: agB.ID, RepositoryID: job.RepositoryID,
			Paths: []string{t.TempDir()}, Keep: catalog.DefaultRetention})
	}

	// The creating agent was assigned; the other one is not, and is not offered it.
	if ids, _ := c.Catalog.RepositoryAgents(ctx, job.RepositoryID); len(ids) != 1 || ids[0] != job.AgentID {
		t.Fatalf("assigned after creation: %v", ids)
	}
	if _, err := newJob("b1"); !errors.Is(err, ErrNotAssigned) {
		t.Fatalf("job for an unassigned agent: %v", err)
	}
	if repos, _ := c.ReposFor(ctx, agB.ID); len(repos) != 0 {
		t.Fatalf("an unassigned agent is offered %d repositories", len(repos))
	}

	// A key file only makes sense for SFTP.
	if _, err := c.AssignAgent(ctx, job.RepositoryID, agB.ID, "/keys/b"); err == nil {
		t.Fatal("a key file was accepted for a filesystem repository")
	}
	if ids, _ := c.Catalog.RepositoryAgents(ctx, job.RepositoryID); len(ids) != 1 {
		t.Fatalf("a failed assignment left %v", ids)
	}

	if _, err := c.AssignAgent(ctx, job.RepositoryID, agB.ID, ""); err != nil {
		t.Fatal(err)
	}
	if repos, _ := c.ReposFor(ctx, agB.ID); len(repos) != 1 {
		t.Fatalf("an assigned agent is offered %d repositories", len(repos))
	}
	bj, err := newJob("b1")
	if err != nil {
		t.Fatal(err)
	}

	// Two agents back up into the one repository.
	for _, id := range []string{job.ID, bj.ID} {
		if run, err := c.RunJob(ctx, id, "manual"); err != nil || run.Status == catalog.RunFailed {
			t.Fatalf("backup of %s: %+v %v", id, run, err)
		}
	}

	if err := c.UnassignAgent(ctx, job.RepositoryID, agB.ID); err == nil {
		t.Fatal("unassigned an agent that still has a job")
	}
	if err := c.Catalog.DeleteJob(ctx, bj.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.UnassignAgent(ctx, job.RepositoryID, agB.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := newJob("b2"); !errors.Is(err, ErrNotAssigned) {
		t.Fatalf("job after unassigning: %v", err)
	}

	// A repository nobody is assigned to (from before assignments) lets the
	// first agent through and assigns it.
	if err := c.Catalog.UnassignAgent(ctx, job.RepositoryID, job.AgentID); err != nil {
		t.Fatal(err)
	}
	if err := c.Catalog.DeleteJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := newJob("b3"); err != nil {
		t.Fatalf("legacy repository without assignments: %v", err)
	}
	if ids, _ := c.Catalog.RepositoryAgents(ctx, job.RepositoryID); len(ids) != 1 || ids[0] != agB.ID {
		t.Fatalf("legacy assignment: %v", ids)
	}
}

func TestDefaultRepoFor(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	repos, err := c.ReposFor(ctx, job.AgentID)
	if err != nil || len(repos) != 1 {
		t.Fatalf("repositories: %+v %v", repos, err)
	}
	if got := c.DefaultRepoFor(ctx, job.AgentID, repos); got != job.RepositoryID {
		t.Fatalf("an agent with one repository: default %q, want %q", got, job.RepositoryID)
	}
	// A second repository the agent can use, with no choice made: the form must ask.
	two := append(repos, catalog.Repository{ID: "other"})
	if err := c.Catalog.SetDefaultRepository(ctx, job.AgentID, "other"); err == nil {
		t.Fatal("a default the agent isn't assigned to must be refused")
	}
	// The assigned repository stays the default while another one is offered.
	if got := c.DefaultRepoFor(ctx, job.AgentID, two); got != job.RepositoryID {
		t.Fatalf("with a stored default: %q, want %q", got, job.RepositoryID)
	}
	// A stored default that isn't on offer (unassigned) falls back to asking.
	if got := c.DefaultRepoFor(ctx, job.AgentID, two[1:]); got != "other" {
		t.Fatalf("one offered repository is the default: %q", got)
	}
	if got := c.DefaultRepoFor(ctx, job.AgentID, append(two, catalog.Repository{ID: "third"})[1:]); got != "" {
		t.Fatalf("several repositories and none stored: %q, want blank", got)
	}
}
