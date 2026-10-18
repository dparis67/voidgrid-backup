package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/engine"
)

func TestTakeOverMaintenanceWhenTheOwnerIsGone(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t) // the first agent created the repository: it owns maintenance
	b, addrB := startAgent(t, t.TempDir())
	agB, err := enrollTestAgent(t, c, b, "box2", addrB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AssignAgent(ctx, job.RepositoryID, agB.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddJob(ctx, JobInput{Name: "onb", AgentID: agB.ID, RepositoryID: job.RepositoryID,
		Paths: []string{t.TempDir()}, Keep: catalog.DefaultRetention}); err != nil {
		t.Fatal(err)
	}
	// The owner's host is replaced or removed behind the server's back.
	if err := c.Catalog.DeleteJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.Catalog.DeleteAgent(ctx, job.AgentID); err != nil {
		t.Fatal(err)
	}

	var gone OwnerGoneError
	if _, err := c.MaintainRepository(ctx, job.RepositoryID); !errors.As(err, &gone) || gone.Owner != engine.OwnerOf(job.AgentID) {
		t.Fatalf("maintenance with an owner that is gone: %v", err)
	}
	m, _ := c.RepoMaintenance(ctx, job.RepositoryID)
	if !m.OwnerGone || m.Owner != engine.OwnerOf(job.AgentID) || m.Status != "failed" {
		t.Fatalf("the failure was not recorded as an owner that is gone: %+v", m)
	}

	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	page := get(t, client, srv.URL+"/repositories", "Take over maintenance")
	if !strings.Contains(page, `<option value="`+agB.ID+`">box2</option>`) {
		t.Fatalf("the take-over form does not offer the assigned agent:\n%s", page)
	}

	// Nothing chosen changes nothing; choosing the other agent fixes it.
	post(t, client, srv.URL+"/repositories/"+job.RepositoryID+"/takeover", url.Values{})
	if m, _ := c.RepoMaintenance(ctx, job.RepositoryID); !m.OwnerGone {
		t.Fatal("a take-over with nothing chosen cleared the problem")
	}
	post(t, client, srv.URL+"/repositories/"+job.RepositoryID+"/takeover", url.Values{"to": {agB.ID}})
	m, _ = c.RepoMaintenance(ctx, job.RepositoryID)
	if m.OwnerGone || m.Error != "" || time.Until(m.NextDue) > time.Minute {
		t.Fatalf("the take-over did not clear the state: %+v", m)
	}
	if _, err := c.MaintainRepository(ctx, job.RepositoryID); err != nil {
		t.Fatalf("maintenance after the take-over: %v", err)
	}
	if page := get(t, client, srv.URL+"/repositories", "Repositories"); strings.Contains(page, "Take over maintenance") {
		t.Fatalf("the take-over form is still shown:\n%s", page)
	}
}
