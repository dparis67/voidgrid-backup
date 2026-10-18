package server

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

// firstRunsNote is shown wherever a start time rests on a job that has not run
// twice yet.
const firstRunsNote = "A backup job needs to run at least twice before its time can be placed accurately: the first run uploads everything and is by far the longest, and until it has run its length is only a guess."

// backupCount is how many finished backups of the job (of its last 20 runs)
// its typical length rests on.
func (c *Controller) backupCount(ctx context.Context, jobID string) int {
	runs, err := c.Catalog.ListRuns(ctx, jobID, 20)
	if err != nil {
		return 0
	}
	n := 0
	for _, r := range runs {
		if r.Kind == "backup" && !r.FinishedAt.IsZero() && (r.Status == catalog.RunSuccess || r.Status == catalog.RunPartial) {
			n++
		}
	}
	return n
}

// dayOf is the start of the local day t is in.
func dayOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// stretches turns a job's slots for the day of now into busy stretches.
func stretches(r jobRow, now time.Time, o planOptions) []busy {
	day := dayOf(now)
	var out []busy
	for _, s := range slotsBetween(r, day, day.Add(overlapHorizon)) {
		out = append(out, busy{Agent: r.Agent, Repo: r.Repo, Start: todOf(s.start), Dur: minutesOf(s.end.Sub(s.start) + o.Buffer)})
	}
	return out
}

func maintStretch(m maintSlot, o planOptions) busy {
	return busy{Agent: m.Agent, Repo: m.Repo, Start: todOf(m.Start), Dur: minutesOf(m.Dur + o.Buffer)}
}

// planInputs splits the scheduled jobs into those the planner may move
// (enabled, one start a day, every day) and the fixed stretches it must work
// around: every other schedule, and the maintenance cycles.
func planInputs(rows []jobRow, maints []maintSlot, now time.Time, o planOptions, runs func(jobID string) int) (items []planItem, fixed []busy) {
	for _, r := range rows {
		if !r.Enabled || r.Schedule == "" {
			continue
		}
		if tod, dows, ok := parseMovable(r.Schedule); ok && dows == nil {
			items = append(items, planItem{ID: r.ID, Name: r.Name, Agent: r.Agent, Repo: r.Repo,
				Dur: rowDur(r), Runs: runs(r.ID), Sched: r.Schedule, Tod: tod})
			continue
		}
		fixed = append(fixed, stretches(r, now, o)...)
	}
	for _, m := range maints {
		fixed = append(fixed, maintStretch(m, o))
	}
	return items, fixed
}

// allStretches is every scheduled job and maintenance cycle on the day of now,
// for placing a job that does not exist yet.
func allStretches(rows []jobRow, maints []maintSlot, now time.Time, o planOptions) []busy {
	var out []busy
	for _, r := range rows {
		out = append(out, stretches(r, now, o)...)
	}
	for _, m := range maints {
		out = append(out, maintStretch(m, o))
	}
	return out
}

// scheduleRows reads the jobs the Schedule page and the planner work from.
func (u *ui) scheduleRows(ctx context.Context, now time.Time) (rows []jobRow, maints []maintSlot, err error) {
	jobs, err := u.c.Catalog.ListJobs(ctx)
	if err != nil {
		return nil, nil, err
	}
	agents, err := u.c.Catalog.ListAgents(ctx)
	if err != nil {
		return nil, nil, err
	}
	repos, err := u.c.Catalog.ListRepositories(ctx)
	if err != nil {
		return nil, nil, err
	}
	rows = make([]jobRow, 0, len(jobs))
	for _, j := range jobs {
		rows = append(rows, u.jobRow(ctx, j, agents, repos))
	}
	return rows, u.c.maintSlots(ctx, repos, jobs, agents, now), nil
}

// windowText is the plan window in words.
func (o planOptions) windowText() string {
	from, to := o.window()
	return todText(from) + " to " + todText(to)
}

// suggestNew proposes a schedule for a job about to be created on agentID and
// repoID, and says why, for the add-job forms.
func (u *ui) suggestNew(ctx context.Context, agentID, repoID string) (cron, text string, clash bool, err error) {
	agent, err := u.c.Catalog.Agent(ctx, agentID)
	if err != nil {
		return "", "", false, err
	}
	repo, err := u.c.Catalog.Repository(ctx, repoID)
	if err != nil {
		return "", "", false, err
	}
	now := time.Now()
	rows, maints, err := u.scheduleRows(ctx, now)
	if err != nil {
		return "", "", false, err
	}
	o := defaultPlanOptions()
	s := suggestStart(allStretches(rows, maints, now, o), agent.Name, repo.Name, overlapDefault, o)
	if s.Clash {
		text = fmt.Sprintf("Nothing between %s is free on %s and %s, so %s overlaps another job. ", o.windowText(), agent.Name, repo.Name, todText(s.Tod))
	} else {
		text = fmt.Sprintf("Suggested %s: free on %s and %s between %s. ", todText(s.Tod), agent.Name, repo.Name, o.windowText())
	}
	return s.Schedule, text + firstRunsNote + " Once it has, use Suggest a plan on the Schedule page.", s.Clash, nil
}

// apiSuggestSchedule answers the add-job forms: the start to prefill for an
// agent and repository.
func (u *ui) apiSuggestSchedule(w http.ResponseWriter, r *http.Request) {
	cron, text, clash, err := u.suggestNew(r.Context(), r.URL.Query().Get("agent"), r.URL.Query().Get("repository"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	replyJSON(w, map[string]any{"schedule": cron, "text": text, "clash": clash})
}

// planView is the proposed plan as the Schedule page shows it.
type planView struct {
	Moves     []planMove // the jobs that would move
	Kept      int        // jobs already in a good place
	Fixed     int        // jobs on schedules the planner leaves alone
	Window    string
	RepoCap   int
	BufferMin int
	Guesses   bool // some job's length rests on fewer than two runs
	Note      string
}

// Length is how long the job's run is expected to take, in words.
func (m planMove) Length() string {
	now := time.Time{}.Add(time.Hour)
	return took(now, now.Add(m.Dur))
}

// MoveLine is what one Apply form posts: job, the schedule it had, and the
// minutes to shift it by.
func (m planMove) MoveLine() string {
	return m.ID + "|" + m.Sched + "|" + strconv.Itoa(m.NewTod-m.Tod)
}

func (u *ui) buildPlan(ctx context.Context, rows []jobRow, maints []maintSlot, now time.Time) planView {
	o := defaultPlanOptions()
	items, fixed := planInputs(rows, maints, now, o, func(id string) int { return u.c.backupCount(ctx, id) })
	pv := planView{Window: o.windowText(), RepoCap: o.RepoCap, BufferMin: int(o.Buffer / time.Minute), Note: firstRunsNote}
	for _, mv := range planDay(items, fixed, o) {
		if mv.Changed() {
			pv.Moves = append(pv.Moves, mv)
		} else {
			pv.Kept++
		}
		pv.Guesses = pv.Guesses || mv.Guess()
	}
	for _, r := range rows {
		if r.Enabled && r.Schedule != "" && !movable(r.Schedule) {
			pv.Fixed++
		}
	}
	return pv
}

// applyPlan shifts the jobs a plan moves. Each line is "job|schedule it had|
// minutes", the same as the Apply forms post; a job whose schedule has changed
// since the plan was drawn is left alone. It returns how many moved and what
// went wrong for the others.
func (u *ui) applyPlan(ctx context.Context, lines []string) (moved int, problems []string) {
	for _, line := range lines {
		f := strings.Split(line, "|")
		if len(f) != 3 {
			problems = append(problems, "unreadable plan line")
			continue
		}
		minutes, err := strconv.Atoi(f[2])
		if err != nil {
			problems = append(problems, "unreadable plan line")
			continue
		}
		name := f[0]
		if j, err := u.c.Catalog.Job(ctx, f[0]); err == nil {
			name = j.Name
		}
		if _, err := u.c.MoveSchedule(ctx, f[0], f[1], minutes); err != nil {
			problems = append(problems, name+": "+err.Error())
			continue
		}
		moved++
	}
	return moved, problems
}

// applySchedulePlan is the Apply buttons of the plan on the Schedule page.
func (u *ui) applySchedulePlan(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	moved, problems := u.applyPlan(r.Context(), r.PostForm["move"])
	msg := fmt.Sprintf("Moved %d job(s).", moved)
	if len(problems) > 0 {
		msg += " Not moved: " + strings.Join(problems, "; ") + "."
	}
	redirectNotice(w, r, "/schedule?plan=1", msg)
}
