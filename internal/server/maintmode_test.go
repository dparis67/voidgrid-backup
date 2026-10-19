package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/tidy"
)

func TestAfterJobsDue(t *testing.T) {
	now := time.Now()
	due := RepoMaintenance{NextDue: now.Add(-time.Minute), Status: "ok"}
	for name, tc := range map[string]struct {
		m       RepoMaintenance
		waiting int
		ran     bool
		want    bool
	}{
		"jobs have run":                     {due, 0, true, true},
		"a job is still to run":             {due, 1, true, false},
		"no job has run":                    {due, 0, false, false},
		"before the earliest time":          {RepoMaintenance{NextDue: now.Add(time.Hour)}, 0, true, false},
		"a failed cycle is retried":         {RepoMaintenance{NextDue: now.Add(-time.Minute), Status: "failed"}, 3, false, true},
		"a failed retry still waits":        {RepoMaintenance{NextDue: now.Add(time.Minute), Status: "failed"}, 0, true, false},
		"overdue runs anyway":               {RepoMaintenance{NextDue: now.Add(-maintOverdue), Status: "ok"}, 2, false, true},
		"just short of overdue still waits": {RepoMaintenance{NextDue: now.Add(-maintOverdue + time.Minute), Status: "ok"}, 2, false, false},
	} {
		if got := afterJobsDue(now, tc.m, tc.waiting, tc.ran); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}

func TestSetMaintScheduleValidatesAndMovesTheNextCycle(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	id := job.RepositoryID
	if s := c.MaintSchedule(ctx, id); s.Mode != MaintAfterJobs {
		t.Fatalf("default: %+v, want after its jobs", s)
	}
	for _, bad := range [][2]string{{"at", "not cron"}, {"at", ""}, {"sometimes", ""}} {
		if err := c.SetMaintSchedule(ctx, id, bad[0], bad[1]); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	if s := c.MaintSchedule(ctx, id); s.Mode != MaintAfterJobs {
		t.Fatalf("a refused change was saved: %+v", s)
	}
	c.saveMaintenance(ctx, id, RepoMaintenance{NextDue: time.Now().Add(-time.Hour)})
	if err := c.SetMaintSchedule(ctx, id, MaintAtTime, " 30 1 * * * "); err != nil {
		t.Fatal(err)
	}
	if s := c.MaintSchedule(ctx, id); s.Mode != MaintAtTime || s.Cron != "30 1 * * *" {
		t.Fatalf("saved: %+v", s)
	}
	m, _ := c.RepoMaintenance(ctx, id)
	if want := nextAt(MaintSchedule{Cron: "30 1 * * *"}, time.Now()); !m.NextDue.Equal(want) {
		t.Fatalf("next due %v, want the next 01:30 (%v)", m.NextDue, want)
	}
	if err := c.SetMaintSchedule(ctx, id, MaintAfterJobs, ""); err != nil {
		t.Fatal(err)
	}
	if s := c.MaintSchedule(ctx, id); s.Mode != MaintAfterJobs || s.Cron != "" {
		t.Fatalf("back to default: %+v", s)
	}
}

// At a fixed time the cycle runs when the time comes, whether or not a job has
// run, and the next one is the following cron time.
func TestStartMaintenanceAtAFixedTime(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	id := job.RepositoryID
	if err := c.SetMaintSchedule(ctx, id, MaintAtTime, "0 4 * * *"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c.startMaintenance(ctx, now) // first sight
	m, ok := c.RepoMaintenance(ctx, id)
	if want := nextAt(MaintSchedule{Cron: "0 4 * * *"}, now); !ok || !m.NextDue.Equal(want) {
		t.Fatalf("first sight: %+v ok=%v, want next due %v", m, ok, want)
	}
	c.startMaintenance(ctx, now)
	waitIdle(t, c, repoMaintRunKey(id))
	if m, _ := c.RepoMaintenance(ctx, id); !m.LastRun.IsZero() {
		t.Fatal("ran before its time")
	}
	c.startMaintenance(ctx, m.NextDue.Add(time.Minute))
	waitFor(t, "the cycle at its time", func() bool { m, _ := c.RepoMaintenance(ctx, id); return !m.LastRun.IsZero() })
	waitIdle(t, c, repoMaintRunKey(id))
	m2, _ := c.RepoMaintenance(ctx, id)
	if want := nextAt(MaintSchedule{Cron: "0 4 * * *"}, time.Now()); m2.Status != "ok" || !m2.NextDue.Equal(want) {
		t.Fatalf("after the run: %+v, want next due %v", m2, want)
	}
}

// After its jobs: a scheduled job that has not run since the last cycle, and is
// due soon, holds the cycle back; once it has run (even to fail) the cycle goes.
func TestStartMaintenanceWaitsForScheduledJobs(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	id := job.RepositoryID
	if err := c.SetSchedule(ctx, job.ID, "* * * * *"); err != nil {
		t.Fatal(err)
	}
	c.saveMaintenance(ctx, id, RepoMaintenance{Status: "ok", NextDue: time.Now().Add(-time.Minute)})

	c.startMaintenance(ctx, time.Now())
	waitIdle(t, c, repoMaintRunKey(id))
	if m, _ := c.RepoMaintenance(ctx, id); !m.LastRun.IsZero() {
		t.Fatal("ran while its scheduled job was still to run")
	}
	if _, err := c.RunJob(ctx, job.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	c.startMaintenance(ctx, time.Now())
	waitFor(t, "the cycle after the job", func() bool { m, _ := c.RepoMaintenance(ctx, id); return !m.LastRun.IsZero() })
	waitIdle(t, c, repoMaintRunKey(id))
}

func TestMaintScheduleForm(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	post := func(form url.Values) {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/repositories/"+job.RepositoryID+"/maint-schedule", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		tidy.Close(resp.Body)
	}
	post(url.Values{"mode": {"at"}, "cron": {"oops"}})
	if s := c.MaintSchedule(ctx, job.RepositoryID); s.Mode != MaintAfterJobs {
		t.Fatalf("a bad cron was saved: %+v", s)
	}
	post(url.Values{"mode": {"at"}, "cron": {"0 1 * * *"}})
	if s := c.MaintSchedule(ctx, job.RepositoryID); s.Mode != MaintAtTime || s.Cron != "0 1 * * *" {
		t.Fatalf("saved: %+v", s)
	}
	resp, err := client.Get(srv.URL + "/repositories")
	if err != nil {
		t.Fatal(err)
	}
	defer tidy.Close(resp.Body)
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if body := sb.String(); !strings.Contains(body, "When maintenance runs") || !strings.Contains(body, "0 1 * * *") {
		t.Fatal("the Repositories page does not show the schedule form and the cron")
	}
}
