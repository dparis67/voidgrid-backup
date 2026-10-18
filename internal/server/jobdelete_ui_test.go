package server

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestDeleteJobControlIsInTheHeaderAndLinkedFromTheList(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)

	page := get(t, client, srv.URL+"/jobs/"+job.ID, `id="delete"`)
	head, body, ok := strings.Cut(page, "<h2>Details</h2>")
	if !ok || !strings.Contains(head, "Also delete its snapshots") {
		t.Fatalf("the delete form is not in the job header:\n%s", page)
	}
	if strings.Count(page, `action="/jobs/`+job.ID+`/delete"`) != 1 || strings.Contains(body, "<h2>Delete job</h2>") {
		t.Fatalf("the delete form is on the page more than once:\n%s", page)
	}

	list := get(t, client, srv.URL+"/jobs", "/jobs/"+job.ID+"#delete")
	if !strings.Contains(list, "Delete…") {
		t.Fatalf("the Jobs list has no delete link:\n%s", list)
	}

	// A running job shows neither.
	if !c.inflight.start(job.ID) {
		t.Fatal("could not mark the job running")
	}
	running := get(t, client, srv.URL+"/jobs/"+job.ID, "running")
	c.inflight.done(job.ID)
	if strings.Contains(running, `id="delete"`) {
		t.Fatalf("a running job offers to be deleted:\n%s", running)
	}

	// The form's fields still work: the typed name is required.
	post(t, client, srv.URL+"/jobs/"+job.ID+"/delete", url.Values{"confirm": {"wrong"}})
	if _, err := c.Catalog.Job(ctx, job.ID); err != nil {
		t.Fatalf("a wrong name deleted the job: %v", err)
	}
	post(t, client, srv.URL+"/jobs/"+job.ID+"/delete", url.Values{"confirm": {job.Name}})
	if _, err := c.Catalog.Job(ctx, job.ID); err == nil {
		t.Fatal("the job was not deleted")
	}
}
