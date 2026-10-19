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
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
	"github.com/voidgrid/voidgrid-backup/internal/tidy"
)

// day is the default window: 24 hours from the start of the current hour.
func day(now time.Time) tlView { return tlView{From: now.Truncate(time.Hour), Span: 24 * time.Hour} }

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
	lanes, ticks, nowPct, unscheduled := buildTimeline(rows, nil, "agent", now, day(now))
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
	if box2.Rows != 2 || box2.Bars[0].Clash || box2.Bars[1].Clash {
		t.Fatalf("box2 has no overlap on its own agent: %+v", box2)
	}
	if last := box2.Bars[1]; last.Name != "f" || last.Left+last.Width > 100.0001 {
		t.Fatalf("a bar must stay inside the span: %+v", last)
	}

	// By repository, a and c share r1 and overlap there instead.
	lanes, _, _, _ = buildTimeline(rows, nil, "repo", now, day(now))
	if len(lanes) != 2 || lanes[0].Name != "r1" || !lanes[0].Bars[0].Clash || !lanes[0].Bars[1].Clash {
		t.Fatalf("repository lanes: %+v", lanes)
	}
}

func TestJobDurationAndPage(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if d, _, guess := c.jobDuration(ctx, job.ID); d != overlapDefault || !guess {
		t.Fatalf("before any backup: %v guess=%v", d, guess)
	}
	if run, err := c.RunJob(ctx, job.ID, "manual"); err != nil || run.Status != catalog.RunSuccess {
		t.Fatalf("backup: %+v %v", run, err)
	}
	if d, _, guess := c.jobDuration(ctx, job.ID); guess || d < overlapMin {
		t.Fatalf("after a backup: %v guess=%v", d, guess)
	}

	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	body := get(t, client, srv.URL+"/schedule", `class="tl-bar`)
	if !strings.Contains(body, `data-name="docs"`) && !strings.Contains(body, ">docs</") {
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
	move := func(was, minutes string) (int, string) { // status and body
		resp, err := client.PostForm(srv.URL+"/jobs/"+job.ID+"/schedule/move", url.Values{"was": {was}, "minutes": {minutes}})
		if err != nil {
			t.Fatal(err)
		}
		defer tidy.Close(resp.Body)
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}

	if code, body := move("0 20 * * *", "90"); code != http.StatusNoContent || body != "" {
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
	if !strings.Contains(body, ` movable`) || !strings.Contains(body, `data-was="0 20 * * *"`) {
		t.Fatalf("simple schedule is not draggable:\n%s", body)
	}
}

func TestLocalPath(t *testing.T) {
	for in, want := range map[string]string{
		"/jobs/abc":                        "/jobs/abc",
		"/jobs/abc/snapshots/s?path=a%2Fb": "/jobs/abc/snapshots/s?path=a%2Fb",
		"/jobs?sort=name&dir=desc":         "/jobs?dir=desc&sort=name",
		"/jobs?on=x&on=y":                  "/jobs?on=x&on=y",
		"/x?notice=a+b%26c":                "/x?notice=a+b%26c",
		"/":                                "/",
		"":                                 "/",
		"https://evil.example/x":           "/",
		"//evil.example/x":                 "/",
		"///evil.example":                  "/",
		`/\evil.example`:                   "/%5Cevil.example", // escaped: a path segment, not a host
		"evil.example/x":                   "/",
		"javascript:alert(1)":              "/",
		"/ok\n/x":                          "/",
	} {
		if got := localPath(in); got != want {
			t.Errorf("localPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestZoneLabel(t *testing.T) {
	cdt := time.Date(2026, 10, 16, 22, 0, 0, 0, time.FixedZone("CDT", -5*3600))
	t.Setenv("TZ", "")
	// zoneLabel formats in the process's local zone; pin that to CDT.
	old := time.Local
	time.Local = time.FixedZone("CDT", -5*3600)
	defer func() { time.Local = old }()
	if got := zoneLabel(cdt); got != "CDT (UTC-05:00)" {
		t.Fatalf("zoneLabel = %q", got)
	}
	t.Setenv("TZ", "America/Chicago")
	if got := zoneLabel(cdt); got != "America/Chicago · CDT (UTC-05:00)" {
		t.Fatalf("with TZ set: %q", got)
	}
	time.Local = time.FixedZone("IST", 5*3600+1800)
	t.Setenv("TZ", "")
	if got := zoneLabel(cdt); got != "IST (UTC+05:30)" {
		t.Fatalf("half-hour zone: %q", got)
	}
	time.Local = time.UTC
	if got := zoneLabel(cdt); got != "UTC" {
		t.Fatalf("utc: %q", got)
	}
}

func TestPagesNameTheirZone(t *testing.T) {
	c, job, _ := setupJob(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	want := zoneLabel(time.Now())
	for _, page := range []string{"/", "/jobs", "/schedule", "/logs", "/jobs/" + job.ID, "/jobs/" + job.ID + "/edit"} {
		body := get(t, client, srv.URL+page, "voidgrid-backup")
		if !strings.Contains(body, want+" · ") {
			t.Errorf("%s does not name the zone %q in its header", page, want)
		}
	}
	for _, page := range []string{"/jobs", "/schedule", "/jobs/" + job.ID + "/edit"} {
		if body := get(t, client, srv.URL+page, "chedule"); !strings.Contains(body, "in "+want) && !strings.Contains(body, "cron in "+want) {
			t.Errorf("%s does not say which zone its schedules are in", page)
		}
	}
}

// A bar is a link and a browser drags a link natively, which cancels the
// pointer drag after the first one; the page must switch that off. And the
// Jobs list says which zone its cron column is in.
func TestScheduleBarsAreNotNativelyDraggable(t *testing.T) {
	c, job, _ := setupJob(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	body := get(t, client, srv.URL+"/schedule", "tl-bar")
	if !strings.Contains(body, `href="/jobs/`+job.ID+`" draggable="false"`) {
		t.Errorf("a movable bar is still a natively draggable link:\n%s", body)
	}
	if !strings.Contains(body, `addEventListener("dragstart"`) {
		t.Error("the page does not cancel dragstart on bars")
	}
	jobs := get(t, client, srv.URL+"/jobs", "Schedule")
	if !strings.Contains(jobs, "Cron expressions are read in the server's time zone") && !strings.Contains(jobs, "Cron expressions are read in the server&#39;s time zone") {
		t.Errorf("the Jobs list does not name the zone of its Schedule column")
	}
}

func TestUTCToLocal(t *testing.T) {
	old := time.Local
	defer func() { time.Local = old }()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

	time.Local = time.FixedZone("CDT", -5*3600)
	for in, want := range map[string]string{
		"0 3 * * *":    "0 22 * * *", // 03:00 UTC was 22:00 the evening before
		"0 2 * * *":    "0 21 * * *",
		"5 23 * * *":   "5 18 * * *",
		"30 5 * * *":   "30 0 * * *",
		"0 5 * * *":    "0 0 * * *",
		"0 3 * * 1":    "0 22 * * 0", // crossing midnight backwards moves the weekday too
		"0 12 * * 1,3": "0 7 * * 1,3",
	} {
		if got, err := utcToLocal(in, now); err != nil || got != want {
			t.Errorf("CDT: utcToLocal(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"*/15 * * * *", "0 2,14 * * *", "@daily", "0 3 1 * *"} {
		if _, err := utcToLocal(in, now); !errors.Is(err, ErrNotMovable) {
			t.Errorf("utcToLocal(%q) error = %v, want ErrNotMovable", in, err)
		}
	}

	time.Local = time.FixedZone("IST", 5*3600+1800)
	if got, _ := utcToLocal("0 20 * * 1", now); got != "30 1 * * 2" {
		t.Errorf("IST: got %q, want %q", got, "30 1 * * 2")
	}
	time.Local = time.UTC
	if got, _ := utcToLocal("0 3 * * *", now); got != "0 3 * * *" {
		t.Errorf("UTC server: got %q, want it unchanged", got)
	}
}

func TestConvertSchedulesOnce(t *testing.T) {
	ctx := context.Background()
	old := time.Local
	time.Local = time.FixedZone("CDT", -5*3600)
	defer func() { time.Local = old }()

	c, job, _ := setupJob(t) // "docs", 0 3 * * *
	add := func(name, sched string) catalog.Job {
		j, err := c.AddJob(ctx, JobInput{Name: name, AgentID: job.AgentID, RepositoryID: job.RepositoryID,
			Paths: job.Paths, Schedule: sched, Keep: catalog.DefaultRetention, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	keep := add("already-local", "0 22 * * *")
	odd := add("every-quarter", "*/15 * * * *")
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	sched := func(id string) string {
		t.Helper()
		j, err := c.Catalog.Job(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return j.Schedule
	}

	// The Jobs page offers it, and the preview shows each result.
	if body := get(t, client, srv.URL+"/jobs", "Jobs"); !strings.Contains(body, "/jobs/convert-utc") {
		t.Fatal("the Jobs page does not offer the conversion")
	}
	preview := get(t, client, srv.URL+"/jobs/convert-utc", "Convert schedules from UTC")
	for _, want := range []string{"0 3 * * *", "0 22 * * *", "edit it by hand"} {
		if !strings.Contains(preview, want) {
			t.Errorf("the preview does not show %q", want)
		}
	}

	// Convert docs only: the ticked one changes, the others stay.
	post(t, client, srv.URL+"/jobs/convert-utc", url.Values{"action": {"convert"}, "job": {job.ID}})
	if got := sched(job.ID); got != "0 22 * * *" {
		t.Fatalf("docs = %q, want 0 22 * * *", got)
	}
	if sched(keep.ID) != "0 22 * * *" || sched(odd.ID) != "*/15 * * * *" {
		t.Fatalf("unticked jobs changed: %q %q", sched(keep.ID), sched(odd.ID))
	}

	// It is recorded: the offer is gone, and a second run changes nothing.
	if body := get(t, client, srv.URL+"/jobs", "Jobs"); strings.Contains(body, "/jobs/convert-utc") {
		t.Error("the offer is still on the Jobs page after the conversion")
	}
	post(t, client, srv.URL+"/jobs/convert-utc", url.Values{"action": {"convert"}, "job": {job.ID, keep.ID}})
	if sched(job.ID) != "0 22 * * *" || sched(keep.ID) != "0 22 * * *" {
		t.Fatalf("a second conversion shifted schedules again: %q %q", sched(job.ID), sched(keep.ID))
	}
	if _, _, err := c.ConvertSchedules(ctx, []string{job.ID}, time.Now()); !errors.Is(err, ErrAlreadyConverted) {
		t.Fatalf("second ConvertSchedules error = %v, want ErrAlreadyConverted", err)
	}
}

func TestDismissConversionAndUTCServer(t *testing.T) {
	ctx := context.Background()
	old := time.Local
	defer func() { time.Local = old }()

	c, _, _ := setupJob(t)
	// A server in UTC has nothing to convert, so nothing is offered.
	time.Local = time.UTC
	if rows, err := c.ConversionPreview(ctx, time.Now()); err != nil || len(rows) != 0 {
		t.Fatalf("UTC preview: %+v %v", rows, err)
	}
	time.Local = time.FixedZone("CDT", -5*3600)
	if rows, _ := c.ConversionPreview(ctx, time.Now()); len(rows) == 0 {
		t.Fatal("no preview rows for a zone with an offset")
	}
	if err := c.DismissConversion(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if done, _ := c.SchedulesConverted(ctx); !done {
		t.Fatal("dismissing did not record it")
	}
	if _, _, err := c.ConvertSchedules(ctx, nil, time.Now()); !errors.Is(err, ErrAlreadyConverted) {
		t.Fatalf("convert after dismiss: %v", err)
	}
}

func TestPhaseSummary(t *testing.T) {
	ms := func(d time.Duration) int64 { return d.Milliseconds() }
	got := phaseSummary(&agentpb.PhaseTimings{
		OpenMs: ms(19 * time.Second), UploadMs: ms(40 * time.Second), SaveMs: ms(2 * time.Second),
		FlushMs: ms(3 * time.Minute), CloseMs: 200, MaintenanceMs: ms(8 * time.Second),
		CommitMs: ms(3*time.Minute + 2*time.Second), // the sum an agent sends alongside the parts
	})
	if want := " (open 19s, upload 40s, save 2s, flush 3m0s, maintenance 8s)"; got != want {
		t.Errorf("phaseSummary = %q, want %q", got, want)
	}
	// An agent from before the split sends only the commit total.
	old := phaseSummary(&agentpb.PhaseTimings{OpenMs: ms(11 * time.Second), CommitMs: ms(105 * time.Second)})
	if want := " (open 11s, commit 1m45s)"; old != want {
		t.Errorf("old-agent summary = %q, want %q", old, want)
	}
	if got := phaseSummary(&agentpb.PhaseTimings{OpenMs: 400}); got != "" {
		t.Errorf("a fast run should add nothing, got %q", got)
	}
}

// Maintenance is drawn as a bar on the lane of the agent that runs it, from its
// due time for as long as its recent cycles took, and holds the agent like a job.
func TestTimelineShowsMaintenance(t *testing.T) {
	now := time.Date(2026, 10, 14, 2, 30, 0, 0, time.UTC)
	rows := []jobRow{{Job: catalog.Job{ID: "a", Name: "a", Schedule: "0 3 * * *", Enabled: true}, Agent: "box1", Repo: "r1", Dur: 40 * time.Minute}}
	maints := []maintSlot{
		{Repo: "r1", Agent: "box1", Start: now.Add(45 * time.Minute), Dur: time.Hour},            // overlaps the job
		{Repo: "r2", Agent: "box2", Start: now.Add(30 * time.Hour), Dur: time.Hour, Guess: true}, // beyond the 24 hours
	}
	lanes, _, _, _ := buildTimeline(rows, maints, "agent", now, day(now))
	if len(lanes) != 1 || len(lanes[0].Bars) != 2 {
		t.Fatalf("want one lane with the job and one maintenance bar: %+v", lanes)
	}
	var m tlBar
	for _, b := range lanes[0].Bars {
		if b.Maint {
			m = b
		}
	}
	if m.Name != "maintenance: r1" || m.Start != "03:15" || m.End != "04:15" || m.Href != "/repositories" || !m.Clash {
		t.Fatalf("maintenance bar: %+v", m)
	}
	lanes, _, _, _ = buildTimeline(rows, maints, "repo", now, day(now))
	if len(lanes) != 1 || lanes[0].Name != "r1" {
		t.Fatalf("by repository the bar sits in its repository's lane: %+v", lanes)
	}
}

// A maintenance cycle's expected length is the longest successful one lately.
func TestMaintDuration(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 3, 0, 0, 0, time.UTC)
	run := func(status string, took, queued time.Duration) catalog.MaintenanceRun {
		return catalog.MaintenanceRun{Status: status, StartedAt: t0, FinishedAt: t0.Add(took + queued), Queued: queued}
	}
	if d, _, guess := maintDuration(nil); d != overlapDefault || !guess {
		t.Errorf("no history: %v %v, want the default as a guess", d, guess)
	}
	d, longest, guess := maintDuration([]catalog.MaintenanceRun{
		run("ok", 20*time.Minute, 2*time.Hour), // the wait is not the work
		run("failed", 5*time.Hour, 0),          // failures say nothing about a good run
		run("ok", 8*time.Minute, 0),
	})
	// Of the two good cycles the typical one is the shorter, and the longest is kept for the tooltip.
	if d != 8*time.Minute || longest != 20*time.Minute || guess {
		t.Errorf("maintDuration = %v, %v, %v, want 8m, 20m", d, longest, guess)
	}
}

func TestTypicalDuration(t *testing.T) {
	m := time.Minute
	for _, c := range []struct {
		name          string
		took          []time.Duration
		want, longest time.Duration
	}{
		{"one cold run is all there is", []time.Duration{2 * time.Hour}, 2 * time.Hour, 2 * time.Hour},
		{"a cold run and a warm one: the warm one", []time.Duration{2 * time.Hour, 7*m + 30*time.Second}, 7*m + 30*time.Second, 2 * time.Hour},
		{"cold run among warm ones is ignored", []time.Duration{8 * m, 2 * time.Hour, 6 * m, 9 * m, 8 * m}, 8 * m, 2 * time.Hour},
		{"short runs are raised to the minimum", []time.Duration{time.Second, 2 * time.Second, 3 * time.Second}, overlapMin, overlapMin},
	} {
		d, longest, guess := typicalDuration(c.took)
		if d != c.want || longest != c.longest || guess {
			t.Errorf("%s: %v, %v, %v, want %v, %v", c.name, d, longest, guess, c.want, c.longest)
		}
	}
	if d, _, guess := typicalDuration(nil); d != overlapDefault || !guess {
		t.Errorf("no runs: %v %v, want the default as a guess", d, guess)
	}
}

func TestParseView(t *testing.T) {
	now := time.Date(2026, 10, 14, 2, 37, 40, 0, time.Local)
	hour := now.Truncate(time.Hour)
	for _, c := range []struct {
		q        string
		minutes  int
		from     time.Time
		explicit bool
	}{
		{"", 1440, hour, false},
		{"span=180", 180, hour, false},
		{"span=7", 1440, hour, false}, // not one of the zooms
		{"span=3", 1440, hour, false}, // the old unit, hours, is not a zoom now
		{"span=junk&from=junk", 1440, hour, false},
		{"span=15", 15, time.Date(2026, 10, 14, 2, 30, 0, 0, time.Local), false}, // the quarter hour
		{"span=5", 5, time.Date(2026, 10, 14, 2, 35, 0, 0, time.Local), false},   // the 5 minutes
		{"span=60&from=2026-10-15T22:05", 60, time.Date(2026, 10, 15, 22, 5, 0, 0, time.Local), true},
	} {
		q, _ := url.ParseQuery(c.q)
		v, explicit := parseView(q, now)
		if v.minutes() != c.minutes || !v.From.Equal(c.from) || explicit != c.explicit {
			t.Errorf("parseView(%q) = %dm from %v explicit=%v; want %dm from %v explicit=%v", c.q, v.minutes(), v.From, explicit, c.minutes, c.from, c.explicit)
		}
	}
}

func TestTLHref(t *testing.T) {
	from := time.Date(2026, 10, 15, 22, 5, 0, 0, time.Local)
	for _, c := range []struct {
		by      string
		minutes int
		from    *time.Time
		want    string
	}{
		{"agent", 1440, nil, "/schedule"},
		{"repo", 180, nil, "/schedule?by=repo&span=180"},
		{"agent", 5, &from, "/schedule?from=2026-10-15T22%3A05&span=5"},
	} {
		if got := tlHref(c.by, c.minutes, c.from); got != c.want {
			t.Errorf("tlHref(%s, %d) = %q, want %q", c.by, c.minutes, got, c.want)
		}
	}
	for m, want := range map[int]string{5: "5 m", 15: "15 m", 60: "1 h", 1440: "24 h"} {
		if got := zoomLabel(m); got != want {
			t.Errorf("zoomLabel(%d) = %q, want %q", m, got, want)
		}
	}
}

// Each zoom divides the window into the ticks it promises, labelled as it says.
func TestTimelineTicksPerZoom(t *testing.T) {
	now := time.Date(2026, 10, 14, 2, 30, 0, 0, time.Local)
	for _, c := range []struct {
		minutes, ticks int
		labels         []string // of the first ticks ("" if unlabelled)
	}{
		{1440, 24, []string{"02:00", ""}},
		{720, 12, []string{"02:00", "03:00"}},
		{360, 12, []string{"02:00", "02:30"}},
		{180, 12, []string{"02:00", "02:15"}},
		{60, 12, []string{"02:00", ""}},
		{15, 15, []string{"02:30", "", "", "", "", "02:35"}}, // a tick a minute, a label every 5th
		{5, 10, []string{"02:30", "", "02:31", ""}},          // a tick every 30 seconds, a label a minute
	} {
		span := time.Duration(c.minutes) * time.Minute
		from := now.Truncate(time.Hour)
		if span < time.Hour {
			from = now.Truncate(span)
		}
		_, ticks, _, _ := buildTimeline(nil, nil, "agent", now, tlView{From: from, Span: span})
		if len(ticks) != c.ticks {
			t.Errorf("%dm: %d ticks, want %d", c.minutes, len(ticks), c.ticks)
			continue
		}
		for i, want := range c.labels {
			if ticks[i].Label != want {
				t.Errorf("%dm: tick %d is labelled %q, want %q", c.minutes, i, ticks[i].Label, want)
			}
		}
	}
	// A window set to a time that is not on the hour starts at its own time,
	// and labels fall on the 10-minute marks.
	view := tlView{From: time.Date(2026, 10, 14, 22, 5, 0, 0, time.Local), Span: time.Hour}
	_, ticks, _, _ := buildTimeline(nil, nil, "agent", now, view)
	if len(ticks) != 12 || ticks[0].Left != 0 || ticks[0].Label != "" || ticks[1].Label != "22:10" || ticks[2].Label != "" {
		t.Errorf("ticks of a window starting at 22:05: %+v", ticks)
	}
}

// A run that began before the window and is still going at its left edge is
// drawn, clipped to the window; one that ended before it is not.
func TestTimelineClipsAtTheWindowEdge(t *testing.T) {
	now := time.Date(2026, 10, 14, 2, 30, 0, 0, time.Local)
	row := func(name, sched string, dur time.Duration) jobRow {
		return jobRow{Job: catalog.Job{ID: name, Name: name, Schedule: sched, Enabled: true}, Agent: "box", Repo: "r", Dur: dur}
	}
	rows := []jobRow{
		row("long", "30 2 * * *", 90*time.Minute), // 02:30 to 04:00
		row("over", "0 1 * * *", 30*time.Minute),  // 01:00 to 01:30, before the window
		row("tiny", "40 3 * * *", 5*time.Minute),  // inside it
	}
	view := tlView{From: time.Date(2026, 10, 14, 3, 0, 0, 0, time.Local), Span: time.Hour}
	lanes, _, _, _ := buildTimeline(rows, nil, "agent", now, view)
	if len(lanes) != 1 {
		t.Fatalf("lanes: %+v", lanes)
	}
	got := map[string]tlBar{}
	for _, b := range lanes[0].Bars {
		got[b.Name] = b
	}
	if _, ok := got["over"]; ok || len(got) != 2 {
		t.Fatalf("want the long and tiny bars only: %+v", got)
	}
	if l := got["long"]; l.Left != 0 || l.Width < 99.99 || l.Width > 100.01 {
		t.Errorf("the long run is clipped to the first hour: left %v width %v", l.Left, l.Width)
	}
	if b := got["tiny"]; b.Left < 66.6 || b.Left > 66.7 || b.Width < 8.3 || b.Width > 8.4 {
		t.Errorf("the tiny run is 5 of 60 minutes wide, from minute 40: left %v width %v", b.Left, b.Width)
	}
}

// The page follows the span and start in its address, and the drag script gets
// the window's length and snap from the markup.
func TestScheduleZoomPage(t *testing.T) {
	c, job, _ := setupJob(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	tomorrow := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	body := get(t, client, srv.URL+"/schedule?span=60&from="+tomorrow+"T02%3A30", `data-span="60"`)
	for _, want := range []string{`data-step="1"`, `left:50.000%`, ">docs</a>", `value="` + tomorrow + `T02:30"`,
		`href="/schedule?from=` + tomorrow + `T03%3A30&amp;span=60"`, `name="span" value="60"`, `data-job="` + job.ID + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("zoomed page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `class="tl-now"`) {
		t.Error("the now line is drawn although now is outside the window")
	}
	wide := get(t, client, srv.URL+"/schedule", `data-span="1440"`)
	if !strings.Contains(wide, `data-step="5"`) || !strings.Contains(wide, `class="tl-now"`) {
		t.Errorf("the default window should be 24 hours with a now line:\n%s", wide)
	}
}

// A name that doesn't fit its bar is written beside it, on the side that is free.
func TestSpillNames(t *testing.T) {
	// 7 px a letter on an 800 px track: 1% is 8 px, so "activity-tracker" (16
	// letters, 112 px, 14%) needs a bar over about 15% to sit inside.
	bar := func(name string, left, width float64, row int) tlBar {
		return tlBar{Name: name, Left: left, Width: width, Row: row}
	}
	bars := []tlBar{
		bar("activity-tracker", 10, 40, 0),  // wide: the name fits
		bar("foundry", 60, 1, 0),            // narrow: beside it, on the right
		bar("immich", 97, 1, 0),             // narrow at the right edge: beside it, on the left
		bar("pocketid", 30, 0.5, 1),         // narrow in its own row
		bar("sandwiched", 60, 0.5, 2),       // narrow, with a neighbour on both sides
		bar("neighbour-right", 61.0, 20, 2), // the right side of the one before is taken
		bar("neighbour-left", 20, 39.5, 2),  // and so is its left side
	}
	spillNames(bars)
	want := map[string]string{"activity-tracker": "", "foundry": "right", "immich": "left", "pocketid": "right", "sandwiched": "", "neighbour-right": "", "neighbour-left": ""}
	for _, b := range bars {
		if b.Spill != want[b.Name] {
			t.Errorf("%s: Spill = %q, want %q", b.Name, b.Spill, want[b.Name])
		}
	}
	// Two narrow bars side by side don't both claim the space between them.
	pair := []tlBar{bar("alpha", 50, 0.5, 0), bar("bravo", 52, 0.5, 0)}
	spillNames(pair)
	if pair[0].Spill == "right" && pair[1].Spill == "left" {
		t.Errorf("both names were put in the space between the bars: %+v", pair)
	}
}

// Every job has its own row in a lane, whether or not it overlaps another, and
// all of a job's runs share it; maintenance comes after the jobs.
func TestTimelineRowPerJob(t *testing.T) {
	now := time.Date(2026, 10, 14, 2, 30, 0, 0, time.Local)
	row := func(name, sched string) jobRow {
		return jobRow{Job: catalog.Job{ID: name, Name: name, Schedule: sched, Enabled: true}, Agent: "box", Repo: "r", Dur: 5 * time.Minute}
	}
	rows := []jobRow{
		row("zeta", "0 4 * * *"),    // does not overlap anything
		row("alpha", "0 3 * * *"),   // does not overlap anything
		row("hourly", "15 * * * *"), // many runs, one row
	}
	maints := []maintSlot{{Repo: "r", Agent: "box", Start: now.Add(10 * time.Hour), Dur: time.Hour}}
	lanes, _, _, _ := buildTimeline(rows, maints, "agent", now, day(now))
	if len(lanes) != 1 || lanes[0].Rows != 4 {
		t.Fatalf("want one lane of 4 rows (three jobs and maintenance): %+v", lanes)
	}
	rowOf := map[string]int{}
	for _, b := range lanes[0].Bars {
		if prev, ok := rowOf[b.Name]; ok && prev != b.Row {
			t.Errorf("%s runs are on rows %d and %d, want one row", b.Name, prev, b.Row)
		}
		rowOf[b.Name] = b.Row
	}
	want := map[string]int{"alpha": 0, "hourly": 1, "zeta": 2, "maintenance: r": 3}
	for name, r := range want {
		if rowOf[name] != r {
			t.Errorf("%s is on row %d, want %d (name order, maintenance last)", name, rowOf[name], r)
		}
	}
}

// The "then run maintenance" box sits on the same line as Back up now, in the
// page's inline checkbox style: a label that is a grid would drop below the
// button and push it out of line with the other buttons.
func TestMaintainBoxIsInlineWithBackUpNow(t *testing.T) {
	c, job, _ := setupJob(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	body := get(t, client, srv.URL+"/jobs/"+job.ID, `name="maintain"`)
	i := strings.Index(body, `action="/jobs/`+job.ID+`/run"`)
	if i < 0 {
		t.Fatal("no run form on the job page")
	}
	form := body[i : i+strings.Index(body[i:], "</form>")]
	for _, want := range []string{`class="row"`, `flex-wrap:nowrap`, `<label class="check"`, `Back up now`} {
		if !strings.Contains(form, want) {
			t.Errorf("the run form lacks %q:\n%s", want, form)
		}
	}
}
