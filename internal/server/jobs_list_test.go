package server

import (
	"context"
	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/tidy"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTook(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		start, end time.Time
		want       string
	}{
		{t0, t0.Add(42 * time.Second), "42s"},
		{t0, t0.Add(83 * time.Second), "1m 23s"},
		{t0, t0.Add(2*time.Hour + 5*time.Minute + 30*time.Second), "2h 05m"},
		{t0, time.Time{}, "-"},
		{time.Time{}, t0, "-"},
		{t0, t0.Add(-time.Second), "-"},
	} {
		if got := took(c.start, c.end); got != c.want {
			t.Errorf("took(%v, %v) = %q, want %q", c.start, c.end, got, c.want)
		}
	}
}

func TestSortJobs(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC)
	run := func(secs int) *catalog.Run {
		return &catalog.Run{StartedAt: t0, FinishedAt: t0.Add(time.Duration(secs) * time.Second)}
	}
	rows := func() []jobRow {
		return []jobRow{
			{Job: catalog.Job{Name: "b"}, Last: run(30)},
			{Job: catalog.Job{Name: "A"}},
			{Job: catalog.Job{Name: "c"}, Last: run(90)},
		}
	}
	names := func(r []jobRow) string {
		s := ""
		for _, x := range r {
			s += x.Name
		}
		return s
	}
	for _, c := range []struct {
		key  string
		desc bool
		want string
	}{
		{"name", false, "Abc"}, {"name", true, "cbA"},
		{"took", false, "bcA"}, {"took", true, "cbA"}, // no value goes last either way
		{"bogus", false, "bAc"},
	} {
		r := rows()
		sortJobs(r, c.key, c.desc)
		if got := names(r); got != c.want {
			t.Errorf("sort %s desc=%v = %s, want %s", c.key, c.desc, got, c.want)
		}
	}
}

func TestRunFromListKeepsSort(t *testing.T) {
	c, job, _ := setupJob(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	for _, f := range []struct {
		form url.Values
		want string
	}{
		{url.Values{"from": {"list"}, "sort": {"took"}, "dir": {"desc"}}, "/jobs?dir=desc&sort=took&notice="},
		{url.Values{"from": {"list"}, "sort": {"evil"}}, "/jobs?notice="},
		{url.Values{}, "/jobs/" + job.ID + "?notice="},
	} {
		req, _ := http.NewRequest("POST", srv.URL+"/jobs/"+job.ID+"/run", strings.NewReader(f.form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		tidy.Close(resp.Body)
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, f.want) {
			t.Errorf("%v redirected to %q, want prefix %q", f.form, loc, f.want)
		}
	}
	// let the started backup finish before the test's cleanup
	for i := 0; i < 100 && c.IsRunning(job.ID); i++ {
		time.Sleep(100 * time.Millisecond)
	}
}

func TestFindClashes(t *testing.T) {
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	row := func(id, agent, repo, sched string, on bool) jobRow {
		return jobRow{Job: catalog.Job{ID: id, Name: id, AgentID: agent, RepositoryID: repo, Schedule: sched, Enabled: on}}
	}
	rows := []jobRow{
		row("a", "ag1", "r1", "0 3 * * *", true),
		row("b", "ag1", "r2", "5 3 * * *", true),  // same agent, 5 min later: inside a's assumed 15 min
		row("c", "ag2", "r1", "10 3 * * *", true), // same repository as a
		row("d", "ag2", "r3", "0 3 * * *", true),  // other agent and repo than a, but same agent as c
		row("e", "ag1", "r1", "30 4 * * *", true), // an hour and a half clear of the rest
		row("f", "ag1", "r1", "0 3 * * *", false), // disabled
		row("g", "ag1", "r1", "", true),           // manual
	}
	got := findClashes(rows, now)
	for id, want := range map[string]int{"a": 2, "b": 1, "c": 2, "d": 1, "e": 0, "f": 0, "g": 0} {
		if len(got[id]) != want {
			t.Errorf("%s: %v, want %d clashes", id, got[id], want)
		}
	}
}

func TestSetScheduleFromList(t *testing.T) {
	c, job, _ := setupJob(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	post := func(f url.Values) string {
		req, _ := http.NewRequest("POST", srv.URL+"/jobs/"+job.ID+"/schedule", strings.NewReader(f.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		tidy.Close(resp.Body)
		return resp.Header.Get("Location")
	}
	if loc := post(url.Values{"schedule": {"nonsense"}, "from": {"list"}, "sort": {"next"}, "dir": {"asc"}}); !strings.HasPrefix(loc, "/jobs?dir=asc&sort=next&notice=Schedule+not+saved") {
		t.Errorf("bad schedule redirected to %q", loc)
	}
	if got, _ := c.Catalog.Job(context.Background(), job.ID); got.Schedule == "nonsense" {
		t.Fatal("an invalid schedule was stored")
	}
	post(url.Values{"schedule": {" 15 2 * * * "}, "from": {"list"}})
	if got, _ := c.Catalog.Job(context.Background(), job.ID); got.Schedule != "15 2 * * *" {
		t.Errorf("schedule = %q", got.Schedule)
	}
	post(url.Values{"schedule": {""}})
	if got, _ := c.Catalog.Job(context.Background(), job.ID); got.Schedule != "" {
		t.Errorf("blank schedule = %q, want manual", got.Schedule)
	}
}

// A run's length leaves out the time it waited for its agent, everywhere it is
// shown or used to lay out the schedule.
func TestRunLengthExcludesQueueWait(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC)
	r := catalog.Run{Kind: "backup", Status: catalog.RunSuccess, StartedAt: t0,
		FinishedAt: t0.Add(2 * time.Hour), Queued: 110 * time.Minute}
	if got := r.Took(); got != 10*time.Minute {
		t.Errorf("Took = %v, want 10m", got)
	}
	if got := tookRun(r); got != "10m 00s" {
		t.Errorf("tookRun = %q, want 10m 00s", got)
	}
	if got := rowDur(jobRow{Last: &r}); got != 10*time.Minute {
		t.Errorf("rowDur = %v, want 10m (the schedule block length)", got)
	}
	old := r
	old.Queued = 0 // a run recorded before the wait was kept, or by an old agent
	if got := old.Took(); got != 2*time.Hour {
		t.Errorf("Took without a recorded wait = %v, want 2h", got)
	}
	if got := tookRun(catalog.Run{StartedAt: t0}); got != "-" {
		t.Errorf("tookRun of an unfinished run = %q, want -", got)
	}
}
