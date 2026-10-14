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
