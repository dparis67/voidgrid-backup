package server

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/tidy"
)

var planOpts = planOptions{WindowStart: 0, WindowEnd: 7 * 60, RepoCap: 2, Buffer: 10 * time.Minute}

func TestCircOverlap(t *testing.T) {
	cases := []struct {
		s1, d1, s2, d2 int
		want           bool
	}{
		{0, 30, 30, 30, false},    // back to back
		{0, 30, 29, 30, true},     // one minute shared
		{1430, 30, 0, 10, true},   // a job across midnight holds the start of the day
		{1430, 30, 20, 10, false}, // and stops again
		{100, 1440, 700, 5, true}, // a day-long job overlaps everything
	}
	for _, c := range cases {
		if got := circOverlap(c.s1, c.d1, c.s2, c.d2); got != c.want {
			t.Errorf("circOverlap(%d,%d,%d,%d) = %v, want %v", c.s1, c.d1, c.s2, c.d2, got, c.want)
		}
	}
}

func TestSuggestStart(t *testing.T) {
	if s := suggestStart(nil, "a", "r", 20*time.Minute, planOpts); s.Tod != 0 || s.Clash || s.Schedule != "0 0 * * *" {
		t.Errorf("empty day: %+v", s)
	}
	// Same agent: it waits for the other job and the buffer (30 + 10 minutes).
	b := []busy{{Agent: "a", Repo: "r1", Start: 0, Dur: 40}}
	if s := suggestStart(b, "a", "r2", 20*time.Minute, planOpts); s.Tod != 40 || s.Clash {
		t.Errorf("same agent: %+v", s)
	}
	// Another agent on another repository does not matter.
	if s := suggestStart(b, "other", "r2", 20*time.Minute, planOpts); s.Tod != 0 {
		t.Errorf("unrelated job: %+v", s)
	}
	// Two jobs already on the repository fill its cap of two.
	full := []busy{{Agent: "x", Repo: "r", Start: 0, Dur: 60}, {Agent: "y", Repo: "r", Start: 0, Dur: 60}}
	if s := suggestStart(full, "z", "r", 20*time.Minute, planOpts); s.Tod != 60 || s.Clash {
		t.Errorf("repository cap: %+v", s)
	}
	// Only one: a second may run alongside.
	if s := suggestStart(full[:1], "z", "r", 20*time.Minute, planOpts); s.Tod != 0 {
		t.Errorf("under the cap: %+v", s)
	}
	// The window is full: the least bad start, flagged.
	jam := []busy{{Agent: "a", Repo: "r", Start: 0, Dur: 7 * 60}}
	if s := suggestStart(jam, "a", "r", 20*time.Minute, planOpts); !s.Clash {
		t.Errorf("a full window must be flagged: %+v", s)
	}
	// Maintenance holds the agent and repository like a job.
	maint := []busy{{Agent: "a", Repo: "r", Start: 0, Dur: 90}}
	if s := suggestStart(maint, "a", "r", 20*time.Minute, planOpts); s.Tod != 90 {
		t.Errorf("around maintenance: %+v", s)
	}
}

func item(name, agent, repo string, dur time.Duration, runs, tod int) planItem {
	return planItem{ID: name, Name: name, Agent: agent, Repo: repo, Dur: dur, Runs: runs, Sched: cronAt(tod), Tod: tod}
}

func overlapsOnAgent(moves []planMove, o planOptions) bool {
	for i, a := range moves {
		for _, b := range moves[i+1:] {
			if a.Agent == b.Agent && circOverlap(a.NewTod, minutesOf(a.Dur+o.Buffer), b.NewTod, minutesOf(b.Dur+o.Buffer)) {
				return true
			}
		}
	}
	return false
}

func TestPlanDay(t *testing.T) {
	// Three jobs on one agent all start at 03:00: the longest keeps its time
	// and the others move into the window.
	items := []planItem{
		item("short", "a", "r", 10*time.Minute, 5, 180),
		item("long", "a", "r", 90*time.Minute, 5, 180),
		item("mid", "a", "r", 30*time.Minute, 5, 180),
	}
	moves := planDay(items, nil, planOpts)
	if overlapsOnAgent(moves, planOpts) {
		t.Fatalf("jobs on one agent overlap: %+v", moves)
	}
	by := map[string]planMove{}
	for _, m := range moves {
		by[m.Name] = m
	}
	if by["long"].Changed() || by["long"].NewTod != 180 {
		t.Errorf("the longest job should keep 03:00: %+v", by["long"])
	}
	if !by["short"].Changed() || !by["mid"].Changed() {
		t.Errorf("the others should move: %+v", moves)
	}
	for i := 1; i < len(moves); i++ {
		if moves[i].NewTod < moves[i-1].NewTod {
			t.Errorf("moves are not ordered by new start: %+v", moves)
		}
	}
	// A plan is stable: planning its own result changes nothing.
	again := make([]planItem, 0, len(moves))
	for _, m := range moves {
		it := m.planItem
		it.Tod, it.Sched = m.NewTod, m.NewSched
		again = append(again, it)
	}
	for _, m := range planDay(again, nil, planOpts) {
		if m.Changed() {
			t.Errorf("a second plan moved %s: %+v", m.Name, m)
		}
	}
}

func TestPlanDayRespectsFixedStretchesAndTheWindow(t *testing.T) {
	// A job outside the window moves into it; maintenance at 00:00 for an hour
	// on the same agent pushes it after.
	fixed := []busy{{Agent: "a", Repo: "r", Start: 0, Dur: 60}}
	moves := planDay([]planItem{item("late", "a", "r", 20*time.Minute, 3, 14*60)}, fixed, planOpts)
	if len(moves) != 1 || moves[0].NewTod != 60 || !moves[0].Changed() || moves[0].Clash {
		t.Fatalf("outside the window: %+v", moves)
	}
	// Different agents sharing a repository beyond its cap are spread out.
	three := []planItem{
		item("a1", "a", "r", 60*time.Minute, 4, 120),
		item("b1", "b", "r", 60*time.Minute, 4, 120),
		item("c1", "c", "r", 60*time.Minute, 4, 120),
	}
	got := planDay(three, nil, planOpts)
	at120 := 0
	for _, m := range got {
		if m.NewTod == 120 {
			at120++
		}
	}
	if at120 > planOpts.RepoCap {
		t.Fatalf("more than %d jobs at once on one repository: %+v", planOpts.RepoCap, got)
	}
}

func TestPlanItemGuess(t *testing.T) {
	for runs, want := range map[int]bool{0: true, 1: true, 2: false, 7: false} {
		if got := (planItem{Runs: runs}).Guess(); got != want {
			t.Errorf("Guess() with %d runs = %v, want %v", runs, got, want)
		}
	}
}

func TestSuggestAndPlanThroughThePages(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t) // "docs", daily at 03:00
	second, err := c.AddJob(ctx, JobInput{Name: "docs2", AgentID: job.AgentID, RepositoryID: job.RepositoryID,
		Paths: []string{t.TempDir()}, Schedule: "0 3 * * *", Keep: catalog.DefaultRetention, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)

	// The suggestion for a third job on the same agent and repository.
	resp, err := client.Get(srv.URL + "/api/suggest-schedule?agent=" + job.AgentID + "&repository=" + job.RepositoryID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	tidy.Close(resp.Body)
	var sug struct {
		Schedule, Text string
		Clash          bool
	}
	if err := json.Unmarshal(body, &sug); err != nil || resp.StatusCode != 200 {
		t.Fatalf("suggestion: %d %s %v", resp.StatusCode, body, err)
	}
	if sug.Schedule != "0 0 * * *" || sug.Clash || !strings.Contains(sug.Text, "at least twice") {
		t.Fatalf("suggestion: %+v", sug)
	}
	if r, _ := client.Get(srv.URL + "/api/suggest-schedule?agent=nope&repository=" + job.RepositoryID); r.StatusCode != 400 {
		t.Fatalf("an unknown agent: %d", r.StatusCode)
	}
	// The add forms carry the hook that fills the field in.
	get(t, client, srv.URL+"/jobs", "data-suggest")

	// Two jobs at 03:00 on one agent: the plan moves exactly one of them.
	page := get(t, client, srv.URL+"/schedule?plan=1", "Proposed start times")
	if !strings.Contains(page, "at least twice") || !strings.Contains(page, "Apply all 1") {
		t.Fatalf("the plan page:\n%s", page)
	}
	m := regexpMove(t, page)
	post(t, client, srv.URL+"/schedule/plan/apply", map[string][]string{"move": {m}})
	a, _ := c.Catalog.Job(ctx, job.ID)
	b, _ := c.Catalog.Job(ctx, second.ID)
	if a.Schedule == b.Schedule {
		t.Fatalf("both jobs still start together: %q %q", a.Schedule, b.Schedule)
	}
	get(t, client, srv.URL+"/schedule?plan=1", "Nothing to move")

	// A plan that is stale (the schedule changed since) is refused, not forced.
	post(t, client, srv.URL+"/schedule/plan/apply", map[string][]string{"move": {m}})
	if b2, _ := c.Catalog.Job(ctx, second.ID); b2.Schedule != b.Schedule {
		t.Fatalf("a stale plan line was applied: %q -> %q", b.Schedule, b2.Schedule)
	}
}

// regexpMove returns the value of the first move field on a plan page.
func regexpMove(t *testing.T, page string) string {
	t.Helper()
	m := regexp.MustCompile(`name="move" value="([^"]+)"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no move field on the plan page:\n%s", page)
	}
	return html.UnescapeString(m[1])
}
