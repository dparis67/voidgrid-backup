package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

func TestBuildTimeline(t *testing.T) {
	now := time.Date(2026, 10, 14, 2, 30, 0, 0, time.UTC)
	row := func(name, agent, repo, sched string, on bool, dur time.Duration) jobRow {
		return jobRow{Job: catalog.Job{ID: name, Name: name, Schedule: sched, Enabled: on}, Agent: agent, Repo: repo, Dur: dur}
	}
	rows := []jobRow{
		row("a", "box1", "r1", "0 3 * * *", true, 40*time.Minute),
		row("b", "box1", "r2", "10 3 * * *", true, 20*time.Minute), // overlaps a on the same agent
		row("c", "box2", "r1", "0 3 * * *", true, 30*time.Minute),  // other agent, same repository as a
		row("d", "box2", "r2", "", true, time.Hour),                // manual
		row("e", "box2", "r2", "0 5 * * *", false, time.Hour),      // off
		row("f", "box2", "r2", "0 22 * * *", true, 2*time.Hour),    // late in the window
	}
	lanes, ticks, nowPct, unscheduled := buildTimeline(rows, "agent", now)
	if len(ticks) != 24 || ticks[0].Label != "02:00" || ticks[1].Label != "" {
		t.Fatalf("ticks: %+v", ticks[:3])
	}
	// The window starts at 02:00, so 02:30 is 2.0833% in.
	if nowPct < 2.08 || nowPct > 2.09 {
		t.Fatalf("now at %v%%", nowPct)
	}
	if len(lanes) != 2 || lanes[0].Name != "box1" || lanes[1].Name != "box2" {
		t.Fatalf("lanes: %+v", lanes)
	}
	if got := strings.Join(unscheduled, ","); got != "d,e" {
		t.Fatalf("unscheduled = %q", got)
	}
	box1 := lanes[0]
	if len(box1.Bars) != 2 || box1.Rows != 2 || !box1.Bars[0].Clash || !box1.Bars[1].Clash || box1.Bars[0].Row != 0 || box1.Bars[1].Row != 1 {
		t.Fatalf("box1 should stack two clashing bars: %+v", box1)
	}
	if b := box1.Bars[0]; b.Start != "03:00" || b.End != "03:40" || b.Left < 4.16 || b.Left > 4.17 {
		t.Fatalf("bar a: %+v", b)
	}
	box2 := lanes[1]
	if box2.Rows != 1 || box2.Bars[0].Clash || box2.Bars[1].Clash {
		t.Fatalf("box2 has no overlap on its own agent: %+v", box2)
	}
	if last := box2.Bars[1]; last.Name != "f" || last.Left+last.Width > 100.0001 {
		t.Fatalf("a bar must stay inside the span: %+v", last)
	}

	// By repository, a and c share r1 and overlap there instead.
	lanes, _, _, _ = buildTimeline(rows, "repo", now)
	if len(lanes) != 2 || lanes[0].Name != "r1" || !lanes[0].Bars[0].Clash || !lanes[0].Bars[1].Clash {
		t.Fatalf("repository lanes: %+v", lanes)
	}
}

func TestJobDurationAndPage(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if d, guess := c.jobDuration(ctx, job.ID); d != overlapDefault || !guess {
		t.Fatalf("before any backup: %v guess=%v", d, guess)
	}
	if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil || run.Status != catalog.RunSuccess {
		t.Fatalf("backup: %+v %v", run, err)
	}
	if d, guess := c.jobDuration(ctx, job.ID); guess || d < overlapMin {
		t.Fatalf("after a backup: %v guess=%v", d, guess)
	}

	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	body := get(t, client, srv.URL+"/schedule", `class="tl-bar`)
	if !strings.Contains(body, ">docs</a>") {
		t.Fatalf("the job is not on the timeline:\n%s", body)
	}
	get(t, client, srv.URL+"/schedule?by=repo", "local") // the repository's name labels the lane
	get(t, client, srv.URL+"/schedule?by=nonsense", "By agent")
}

func TestShiftSchedule(t *testing.T) {
	cases := []struct {
		spec    string
		minutes int
		want    string
	}{
		{"0 20 * * *", 75, "15 21 * * *"},
		{"0 20 * * *", -20, "40 19 * * *"},
		{"30 23 * * *", 45, "15 0 * * *"},  // across midnight, every day
		{"15 0 * * *", -30, "45 23 * * *"}, // and back
		{"0 20 * * 1", 300, "0 1 * * 2"},   // Monday 20:00 + 5h is Tuesday
		{"0 1 * * 0", -120, "0 23 * * 6"},  // Sunday 01:00 - 2h is Saturday
		{"0 1 * * 7", -120, "0 23 * * 6"},  // 7 is Sunday too
		{"0 3 * * 1,3,5", 1440, "0 3 * * 2,4,6"},
		{"0 22 * * 6,0", 180, "0 1 * * 0,1"}, // the list wraps and is sorted
		{"5 4 ? * *", 0, "5 4 * * *"},
	}
	for _, c := range cases {
		got, err := shiftSchedule(c.spec, c.minutes)
		if err != nil || got != c.want {
			t.Errorf("shiftSchedule(%q, %d) = %q, %v; want %q", c.spec, c.minutes, got, err, c.want)
		}
	}
	for _, spec := range []string{"", "@daily", "*/15 * * * *", "0 */6 * * *", "0 2,14 * * *", "0 3 1 * *", "0 3 * 6 *",
		"0 3 * * 1-5", "0 3 * * mon", "60 3 * * *", "0 24 * * *", "CRON_TZ=UTC 0 3 * * *", "0 3 * *"} {
		if movable(spec) {
			t.Errorf("%q should not be movable", spec)
		}
		if _, err := shiftSchedule(spec, 10); !errors.Is(err, ErrNotMovable) {
			t.Errorf("shiftSchedule(%q) error = %v, want ErrNotMovable", spec, err)
		}
	}
}

func TestMoveScheduleEndpoint(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if err := c.SetSchedule(ctx, job.ID, "0 20 * * *"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	move := func(was, minutes string) (int, string) {
		resp, err := client.PostForm(srv.URL+"/jobs/"+job.ID+"/schedule/move", url.Values{"was": {was}, "minutes": {minutes}})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}

	if code, body := move("0 20 * * *", "90"); code != 200 || body != "30 21 * * *" {
		t.Fatalf("move: %d %q", code, body)
	}
	if j, _ := c.Catalog.Job(ctx, job.ID); j.Schedule != "30 21 * * *" {
		t.Fatalf("stored schedule %q", j.Schedule)
	}
	// A page drawn before that move must not overwrite it.
	if code, _ := move("0 20 * * *", "30"); code != http.StatusConflict {
		t.Fatalf("stale move: %d", code)
	}
	if code, _ := move("30 21 * * *", "banana"); code != http.StatusBadRequest {
		t.Fatalf("bad minutes: %d", code)
	}
	if err := c.SetSchedule(ctx, job.ID, "0 */6 * * *"); err != nil {
		t.Fatal(err)
	}
	if code, _ := move("0 */6 * * *", "30"); code != http.StatusConflict {
		t.Fatalf("complex schedule: %d", code)
	}
	if j, _ := c.Catalog.Job(ctx, job.ID); j.Schedule != "0 */6 * * *" {
		t.Fatalf("a refused move changed the schedule to %q", j.Schedule)
	}
	// The page marks simple schedules as draggable and leaves the others alone.
	if err := c.SetSchedule(ctx, job.ID, "0 20 * * *"); err != nil {
		t.Fatal(err)
	}
	get(t, client, srv.URL+"/schedule", `class="tl-bar`)
	body := get(t, client, srv.URL+"/schedule", "tl-bar")
	if !strings.Contains(body, ` movable"`) || !strings.Contains(body, `data-was="0 20 * * *"`) {
		t.Fatalf("simple schedule is not draggable:\n%s", body)
	}
}
