package server

import (
	"fmt"
	"sort"
	"time"
)

// The planner works on one representative day, as minutes after midnight on a
// circle (a job that starts at 23:50 and runs 30 minutes also holds 00:00 to
// 00:20). Jobs on one agent queue behind that agent's operation lock, so an
// agent's jobs must not overlap at all; jobs on one repository compete for its
// connections, so at most RepoCap of them run at once.

const (
	planStep = 5 // candidate start times are this many minutes apart
	dayMin   = 24 * 60
)

// planOptions are the rules a plan follows.
type planOptions struct {
	// Start times are chosen from WindowStart (inclusive) to WindowEnd
	// (exclusive), minutes after midnight. A job may run past the window's end.
	WindowStart, WindowEnd int
	RepoCap                int           // jobs that may run at once on one repository
	Buffer                 time.Duration // idle gap kept after each job
}

func defaultPlanOptions() planOptions {
	return planOptions{WindowStart: 0, WindowEnd: 7 * 60, RepoCap: 2, Buffer: 10 * time.Minute}
}

func (o planOptions) window() (from, to int) {
	if o.WindowEnd <= o.WindowStart || o.WindowStart < 0 || o.WindowEnd > dayMin {
		return 0, dayMin
	}
	return o.WindowStart, o.WindowEnd
}

// busy is a stretch of the day that holds an agent and a repository. Agent and
// Repo are names, which are unique, so jobs and maintenance cycles share keys.
type busy struct {
	Agent, Repo string
	Start, Dur  int // minutes; Dur includes the buffer
}

func modDay(x int) int { return ((x % dayMin) + dayMin) % dayMin }

// circOverlap reports whether two stretches of the day, each given by a start
// and a length in minutes, share any minute.
func circOverlap(s1, d1, s2, d2 int) bool {
	return modDay(s2-s1) < d1 || modDay(s1-s2) < d2
}

// todOf is the minute of the day of t.
func todOf(t time.Time) int { return t.Hour()*60 + t.Minute() }

// minutesOf rounds a duration up to whole minutes, at least one.
func minutesOf(d time.Duration) int {
	m := int((d + time.Minute - 1) / time.Minute)
	return max(m, 1)
}

// clash scores starting at t for d minutes against what is already busy: how
// many stretches on the same agent it overlaps (never allowed) and how many
// more than the repository can take. Zero for both means it fits.
func (o planOptions) clash(b []busy, agent, repo string, t, d int) (agentHits, repoOver int) {
	repoHits := 0
	for _, x := range b {
		if !circOverlap(t, d, x.Start, x.Dur) {
			continue
		}
		switch {
		case x.Agent == agent:
			agentHits++
		case x.Repo == repo:
			repoHits++
		}
	}
	return agentHits, max(0, repoHits-(max(o.RepoCap, 1)-1))
}

// firstGap returns the earliest start in the window where a job of d minutes on
// agent and repo overlaps nothing it must not. When the window has no such
// start, it returns the start that clashes least and ok=false.
func (o planOptions) firstGap(b []busy, agent, repo string, d int) (tod int, ok bool) {
	from, to := o.window()
	best, bestCost := from, -1
	for t := from; t < to; t += planStep {
		a, r := o.clash(b, agent, repo, t, d)
		if a == 0 && r == 0 {
			return t, true
		}
		if cost := a*1000 + r; bestCost < 0 || cost < bestCost {
			best, bestCost = t, cost
		}
	}
	return best, false
}

// cronAt is the schedule that runs every day at a minute of the day.
func cronAt(tod int) string { return fmt.Sprintf("%d %d * * *", modDay(tod)%60, modDay(tod)/60) }

// todText formats a minute of the day as HH:MM.
func todText(tod int) string { return fmt.Sprintf("%02d:%02d", modDay(tod)/60, modDay(tod)%60) }

// planItem is a job the planner may move: enabled, with one start time a day.
type planItem struct {
	ID, Name, Agent, Repo string
	Dur                   time.Duration // the median of its recent backups, or a default
	Runs                  int           // finished backups behind Dur
	Sched                 string
	Tod                   int // its current start, minutes after midnight
}

// Guess is true while Dur rests on fewer than two finished backups: the first
// run uploads everything and is far longer than the ones after it, so one run
// says little about how long the job normally takes.
func (i planItem) Guess() bool { return i.Runs < 2 }

// planMove is where the planner puts a job.
type planMove struct {
	planItem
	NewTod   int
	NewSched string
	Clash    bool // the window had no free gap: this is the least bad start
}

// Changed reports whether the job moves.
func (m planMove) Changed() bool { return m.NewTod != m.Tod }

// planDay places every item, longest first (long jobs are the hardest to fit),
// keeping a job where it is when that is inside the window and clashes with
// nothing placed so far, else at the earliest free start in the window. fixed is
// everything the planner may not move: jobs with other schedules, and
// maintenance cycles. The result is ordered by new start time.
func planDay(items []planItem, fixed []busy, o planOptions) []planMove {
	order := append([]planItem(nil), items...)
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].Dur != order[j].Dur {
			return order[i].Dur > order[j].Dur
		}
		return order[i].Name < order[j].Name
	})
	from, to := o.window()
	b := append([]busy(nil), fixed...)
	out := make([]planMove, 0, len(order))
	for _, it := range order {
		d := minutesOf(it.Dur + o.Buffer)
		mv := planMove{planItem: it, NewTod: it.Tod}
		a, r := o.clash(b, it.Agent, it.Repo, it.Tod, d)
		if it.Tod < from || it.Tod >= to || a != 0 || r != 0 {
			var ok bool
			mv.NewTod, ok = o.firstGap(b, it.Agent, it.Repo, d)
			mv.Clash = !ok
		}
		mv.NewSched = cronAt(mv.NewTod)
		b = append(b, busy{Agent: it.Agent, Repo: it.Repo, Start: mv.NewTod, Dur: d})
		out = append(out, mv)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].NewTod < out[j].NewTod })
	return out
}

// suggestion is the start proposed for a job that does not exist yet.
type suggestion struct {
	Tod      int
	Schedule string
	Clash    bool // the window had no free gap
}

// suggestStart finds the earliest start in the window for a new job of dur on
// agent and repo that overlaps nothing in busy (every existing job and
// maintenance cycle).
func suggestStart(b []busy, agent, repo string, dur time.Duration, o planOptions) suggestion {
	tod, ok := o.firstGap(b, agent, repo, minutesOf(dur+o.Buffer))
	return suggestion{Tod: tod, Schedule: cronAt(tod), Clash: !ok}
}
