package server

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

// The Schedule page: the next 24 hours as a timeline, one lane per agent or per
// repository, with each scheduled job drawn from its start time for as long as
// it is expected to run. A bar of a job with a simple daily schedule can be
// dragged to another time (see shift.go); the rest only show.

const (
	timelineSpan = 24 * time.Hour
	minBarWidth  = 0.6 // percent of the span, so a short job stays visible
)

type tlBar struct {
	JobID, Name string
	Left, Width float64 // percent of the span
	Row         int     // vertical position inside the lane when bars overlap
	Start, End  string
	Tip         string
	Clash       bool // overlaps another bar in the same lane
	Guess       bool // the duration is a default: no finished backup yet
	Past        bool
	Movable     bool   // the schedule is simple enough to move by dragging
	Was         string // the schedule this bar was drawn from, sent back with a move
}

type tlLane struct {
	Name string
	Rows int
	Bars []tlBar
}

type tlTick struct {
	Left  float64
	Label string // "" for an hour that is not labelled
}

type timelinePage struct {
	base
	By          string // "agent" or "repo"
	Lanes       []tlLane
	Ticks       []tlTick
	Now         float64 // the current time as a percent of the span
	Unscheduled []string
}

// moveSchedule shifts a job's schedule by a number of minutes, from dragging
// its bar. It replies 204; the page then redraws from the server.
func (u *ui) moveSchedule(w http.ResponseWriter, r *http.Request) {
	minutes, err := strconv.Atoi(r.FormValue("minutes"))
	if err != nil || minutes < -minutesPerDay || minutes > minutesPerDay {
		http.Error(w, "minutes must be a number within a day", http.StatusBadRequest)
		return
	}
	_, err = u.c.MoveSchedule(r.Context(), r.PathValue("id"), r.FormValue("was"), minutes)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, ErrRunning), errors.Is(err, ErrScheduleChanged), errors.Is(err, ErrNotMovable):
		http.Error(w, err.Error(), http.StatusConflict)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (u *ui) schedule(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	by := "agent"
	if r.URL.Query().Get("by") == "repo" {
		by = "repo"
	}
	jobs, err := u.c.Catalog.ListJobs(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	agents, err := u.c.Catalog.ListAgents(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	repos, err := u.c.Catalog.ListRepositories(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rows := make([]jobRow, 0, len(jobs))
	for _, j := range jobs {
		rows = append(rows, u.jobRow(ctx, j, agents, repos))
	}
	p := timelinePage{base: u.newBase("Schedule", "schedule", r), By: by}
	p.Lanes, p.Ticks, p.Now, p.Unscheduled = buildTimeline(rows, by, time.Now())
	u.render(w, http.StatusOK, "schedule", p)
}

// buildTimeline lays the next 24 hours out from the start of the current hour.
// Jobs that are manual or disabled are returned in unscheduled. Bars that
// overlap inside a lane are stacked in rows and marked as clashing.
func buildTimeline(rows []jobRow, by string, now time.Time) (lanes []tlLane, ticks []tlTick, nowPct float64, unscheduled []string) {
	from := now.Truncate(time.Hour)
	to := from.Add(timelineSpan)
	pct := func(t time.Time) float64 { return float64(t.Sub(from)) / float64(timelineSpan) * 100 }

	for h := 0; h < 24; h++ {
		t := from.Add(time.Duration(h) * time.Hour)
		label := ""
		if h%2 == 0 {
			label = t.Format("15:04")
		}
		ticks = append(ticks, tlTick{Left: pct(t), Label: label})
	}

	type placed struct {
		bar tlBar
		s   slot
	}
	byLane := map[string][]placed{}
	for _, r := range rows {
		slots := slotsBetween(r, from, to)
		if len(slots) == 0 {
			if !r.Enabled || r.Schedule == "" {
				unscheduled = append(unscheduled, r.Name)
			}
			continue
		}
		lane := r.Agent
		if by == "repo" {
			lane = r.Repo
		}
		for _, sl := range slots {
			left := pct(sl.start)
			width := max(pct(sl.end)-left, minBarWidth)
			if left+width > 100 {
				width = 100 - left
			}
			tip := fmt.Sprintf("%s: %s to %s (%s)", r.Name, sl.start.Format("15:04"), sl.end.Format("15:04"), took(sl.start, sl.end))
			if r.DurGuess {
				tip += ", estimated: no finished backup yet"
			}
			byLane[lane] = append(byLane[lane], placed{
				bar: tlBar{JobID: r.ID, Name: r.Name, Left: left, Width: width, Start: sl.start.Format("15:04"),
					End: sl.end.Format("15:04"), Tip: tip, Guess: r.DurGuess, Past: sl.start.Before(now),
					Movable: movable(r.Schedule), Was: r.Schedule},
				s: sl,
			})
		}
	}

	names := make([]string, 0, len(byLane))
	for n := range byLane {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ps := byLane[n]
		sort.SliceStable(ps, func(i, j int) bool { return ps[i].s.start.Before(ps[j].s.start) })
		var rowEnds []time.Time // when each row of the lane is free again
		for i := range ps {
			row := -1
			for k, end := range rowEnds {
				if !ps[i].s.start.Before(end) {
					row = k
					break
				}
			}
			if row < 0 {
				row = len(rowEnds)
				rowEnds = append(rowEnds, time.Time{})
			}
			rowEnds[row] = ps[i].s.end
			ps[i].bar.Row = row
			for j := range ps {
				if i != j && ps[i].s.start.Before(ps[j].s.end) && ps[j].s.start.Before(ps[i].s.end) {
					ps[i].bar.Clash = true
				}
			}
		}
		lane := tlLane{Name: n, Rows: len(rowEnds)}
		for _, p := range ps {
			lane.Bars = append(lane.Bars, p.bar)
		}
		lanes = append(lanes, lane)
	}
	sort.Strings(unscheduled)
	return lanes, ticks, pct(now), unscheduled
}
