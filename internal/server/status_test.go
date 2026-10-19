package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

func TestStatusPage(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t) // one agent, one repository, one scheduled job that has never run
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)

	// Home is the status page, and every line links to where to look closer.
	page := get(t, client, srv.URL+"/", "Status")
	for _, want := range []string{`href="/agents"`, `href="/jobs"`, `href="/repositories"`, `href="/schedule"`,
		"1 of 1 reachable", "1 job", "1 never run", "1 repository", "maintenance ok", "docs"} {
		if !strings.Contains(page, want) {
			t.Errorf("the status page is missing %q:\n%s", want, page)
		}
	}
	// The navigation starts with Status, and Agents has its own address.
	if !strings.Contains(page, `<a href="/" class="on">Status</a>`) || !strings.Contains(page, `<a href="/agents" >Agents</a>`) {
		t.Errorf("navigation: %s", page)
	}
	get(t, client, srv.URL+"/agents", "Waiting for approval")

	// A good backup: all good.
	if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil || run.Status != catalog.RunSuccess {
		t.Fatalf("backup: %+v %v", run, err)
	}
	page = get(t, client, srv.URL+"/", "all good")
	if !strings.Contains(page, "last backups all ok") {
		t.Errorf("a good backup is not reported as ok:\n%s", page)
	}

	// An agent that cannot be reached, and one waiting for approval.
	if err := c.Catalog.RecordFailure(ctx, job.AgentID, "connection refused"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Catalog.RegisterAgent(ctx, catalog.Registration{ID: "reg1", Fingerprint: "fp1", AgentCert: []byte{1},
		Hostname: "newbox", Address: "10.0.0.9:9443", Version: "v", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	page = get(t, client, srv.URL+"/", "unreachable")
	for _, want := range []string{"0 of 1 reachable", "box unreachable", "1 waiting for approval", "1 thing needs attention"} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q:\n%s", want, page)
		}
	}
	if err := c.Catalog.RecordSeen(ctx, job.AgentID, time.Now(), "h", "v", nil); err != nil {
		t.Fatal(err)
	}

	// A failed backup and a failed maintenance cycle name what failed.
	id, err := c.Catalog.StartRun(ctx, job.ID, "backup", "schedule", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Catalog.FinishRun(ctx, catalog.Run{ID: id, Status: catalog.RunFailed, Error: "boom", StartedAt: time.Now(), FinishedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	c.saveMaintenance(ctx, job.RepositoryID, RepoMaintenance{LastRun: time.Now(), Status: "failed", Error: "x"})
	page = get(t, client, srv.URL+"/", "failed")
	for _, want := range []string{"1 failed", "docs failed", "maintenance failed on local", "3 things need attention"} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q:\n%s", want, page)
		}
	}
}

func TestStatusPageWithNothingSetUp(t *testing.T) {
	c := newController(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	page := get(t, client, srv.URL+"/", "all good")
	if strings.Count(page, "none yet") != 3 || !strings.Contains(page, "nothing scheduled") {
		t.Errorf("an empty system:\n%s", page)
	}
}
