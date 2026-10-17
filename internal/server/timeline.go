package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

// The Schedule page: a window of the day (5 or 15 minutes, or 1, 3, 6, 12 or 24
// hours, from the current hour, or the current 5 or 15 minutes for the narrow
// two, or a time the operator picks) as a timeline, one
// lane per agent or per repository, with each scheduled job drawn from its
// start time for as long as it is expected to run. A bar of a job with a simple
// daily schedule can be dragged to another time (see shift.go); the rest only
// show.

const (
	minBarWidth = 0.6 // percent of the window, so a short job stays visible

	// A bar is never narrower than minBarPx on screen (a style rule). A name that
	// does not fit in its bar is written beside it, judged by assuming a track
	// spillTrackPx wide and letters spillCharPx wide: the page can't know the
	// real width, and a wrong guess only means the name is hover text or sits
	// beside a bar that could have held it.
	minBarPx     = 10.0
	spillTrackPx = 800.0
	spillCharPx  = 7.0
	spillGapPx   = 4.0

	// fromLayout is how the window's start is written in the page's address and
	// in its time field (an HTML datetime-local value), in the server's zone.
	fromLayout = "2006-01-02T15:04"
)

// tlZooms are the window lengths on offer, in minutes, narrowest first.
var tlZooms = []int{5, 15, 60, 180, 360, 720, 1440}

// tlScale says how a window (by its length in minutes) is divided: a tick every
// tick, labelled on every labelEvery-th one, and how finely a bar snaps when
// dragged, in minutes. A schedule has no seconds, so a minute is the finest.
var tlScale = map[int]struct {
	tick                time.Duration
	labelEvery, stepMin int
}{
	1440: {time.Hour, 2, 5},
	720:  {time.Hour, 1, 5},
	360:  {30 * time.Minute, 1, 5},
	180:  {15 * time.Minute, 1, 1},
	60:   {5 * time.Minute, 2, 1},
	15:   {time.Minute, 5, 1},
	5:    {30 * time.Second, 2, 1},
}

// zoomLabel names a window length: "15 m", "3 h".
func zoomLabel(minutes int) string {
	if minutes < 60 {
		return strconv.Itoa(minutes) + " m"
	}
	return strconv.Itoa(minutes/60) + " h"
}

// tlView is the window the page shows.
type tlView struct {
	From time.Time
	Span time.Duration
}

func (v tlView) minutes() int { return int(v.Span / time.Minute) }

// parseView reads the window from a request's query: span is a number of
// minutes from tlZooms (1440 if absent or not one of them), from is a
// datetime-local value in the server's zone. Without a valid from the window
// starts at the beginning of the current hour, or of the current 5 or 15
// minutes for those narrow zooms. explicit says the start was given.
func parseView(q url.Values, now time.Time) (v tlView, explicit bool) {
	v.Span = 24 * time.Hour
	if m, err := strconv.Atoi(q.Get("span")); err == nil && slices.Contains(tlZooms, m) {
		v.Span = time.Duration(m) * time.Minute
	}
	if t, err := time.ParseInLocation(fromLayout, q.Get("from"), time.Local); err == nil {
		v.From, explicit = t, true
	} else if v.Span < time.Hour {
		v.From = now.Truncate(v.Span)
	} else {
		v.From = now.Truncate(time.Hour)
	}
	return v, explicit
}

type tlBar struct {
	JobID, Name string
	Left, Width float64 // percent of the span
	Row         int     // the job's row in its lane: one row per job
	Start, End  string
	Tip         string
	Clash       bool // overlaps another bar in the same lane
	Guess       bool // the duration is a default: no finished backup yet
	Past        bool
	Movable     bool   // the schedule is simple enough to move by dragging
	Was         string // the schedule this bar was drawn from, sent back with a move
	Href        string // where clicking the bar goes
	Maint       bool   // a repository's maintenance cycle, not a job
	Spill       string // "right" or "left": the name is written beside the bar, which is too narrow for it
}

type tlLane struct {
	Name string
	Rows int
	Bars []tlBar
}

type tlTick struct {
	Left  float64
	Label string // "" for a tick that is not labelled
}

// tlZoom is one zoom button.
type tlZoom struct {
	Label string
	Href  string
	On    bool
}

type timelinePage struct {
	base
	By          string // "agent" or "repo"
	Lanes       []tlLane
	Ticks       []tlTick
	Now         float64 // the current time as a percent of the window
	ShowNow     bool    // the current time is inside the window
	Unscheduled []string

	Zooms            []tlZoom
	Earlier, Later   string // links that move the window by its own length
	NowHref          string // back to the window that starts at the current hour
	FromValue        string // the start, for the time field
	Range            string // the window in words
	SpanMin, StepMin int    // the window's length and a drag's snap, in minutes
}

// tlHref is the address of the Schedule page for a lane grouping and a window
// length in minutes. The start is left out (the window then follows the clock)
// unless from is set.
func tlHref(by string, minutes int, from *time.Time) string {
	q := url.Values{}
	if by == "repo" {
		q.Set("by", "repo")
	}
	if minutes != 1440 {
		q.Set("span", strconv.Itoa(minutes))
	}
	if from != nil {
		q.Set("from", from.Format(fromLayout))
	}
	if len(q) == 0 {
		return "/schedule"
	}
	return "/schedule?" + q.Encode()
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
	now := time.Now()
	view, explicit := parseView(r.URL.Query(), now)
	p := timelinePage{base: u.newBase("Schedule", "schedule", r), By: by}
	p.Lanes, p.Ticks, p.Now, p.Unscheduled = buildTimeline(rows, u.c.maintSlots(ctx, repos, jobs, agents, now), by, now, view)
	p.ShowNow = !now.Before(view.From) && now.Before(view.From.Add(view.Span))
	p.SpanMin, p.StepMin = view.minutes(), tlScale[view.minutes()].stepMin
	var start *time.Time
	if explicit {
		start = &view.From
	}
	for _, m := range tlZooms {
		p.Zooms = append(p.Zooms, tlZoom{Label: zoomLabel(m), Href: tlHref(by, m, start), On: m == view.minutes()})
	}
	earlier, later := view.From.Add(-view.Span), view.From.Add(view.Span)
	p.Earlier, p.Later = tlHref(by, view.minutes(), &earlier), tlHref(by, view.minutes(), &later)
	p.NowHref = tlHref(by, view.minutes(), nil)
	p.FromValue = view.From.Format(fromLayout)
	p.Range = view.From.Format("Mon 2 Jan 15:04") + " to " + view.From.Add(view.Span).Format("Mon 2 Jan 15:04")
	u.render(w, http.StatusOK, "schedule", p)
}

// buildTimeline lays the window view out.
// Jobs that are manual or disabled are returned in unscheduled. Maintenance
// cycles are drawn as bars too, as they hold their agent like a job. Bars that
// overlap inside a lane are marked as clashing; each job has its own row.
func buildTimeline(rows []jobRow, maints []maintSlot, by string, now time.Time, view tlView) (lanes []tlLane, ticks []tlTick, nowPct float64, unscheduled []string) {
	from, span := view.From, view.Span
	to := from.Add(span)
	pct := func(t time.Time) float64 { return float64(t.Sub(from)) / float64(span) * 100 }
	// place is a bar's left edge and width in percent of the window, clipped to
	// it, and still visible for a very short job.
	place := func(start, end time.Time) (left, width float64) {
		left, right := max(pct(start), 0), min(pct(end), 100)
		width = max(right-left, minBarWidth)
		if left+width > 100 {
			width = 100 - left
		}
		return left, width
	}

	sc, ok := tlScale[view.minutes()]
	if !ok {
		sc = tlScale[1440]
	}
	for t := from.Truncate(sc.tick); t.Before(to); t = t.Add(sc.tick) {
		if t.Before(from) {
			continue
		}
		label := ""
		midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
		if int(t.Sub(midnight)/sc.tick)%sc.labelEvery == 0 {
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
		// Start looking one run early: a run that began before the window may
		// still be going at its left edge.
		var slots []slot
		for _, sl := range slotsBetween(r, from.Add(-rowDur(r)), to) {
			if sl.end.After(from) {
				slots = append(slots, sl)
			}
		}
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
			left, width := place(sl.start, sl.end)
			tip := fmt.Sprintf("%s: %s to %s (%s)", r.Name, sl.start.Format("15:04"), sl.end.Format("15:04"), took(sl.start, sl.end))
			if r.DurGuess {
				tip += ", estimated: no finished backup yet"
			} else if r.Longest > r.Dur {
				tip += ", typical of its last runs; the longest was " + took(sl.start, sl.start.Add(r.Longest))
			}
			byLane[lane] = append(byLane[lane], placed{
				bar: tlBar{JobID: r.ID, Name: r.Name, Left: left, Width: width, Start: sl.start.Format("15:04"),
					End: sl.end.Format("15:04"), Tip: tip, Guess: r.DurGuess, Past: sl.start.Before(now),
					Movable: movable(r.Schedule), Was: r.Schedule, Href: "/jobs/" + r.ID},
				s: sl,
			})
		}
	}
	for _, m := range maints {
		end := m.Start.Add(m.Dur)
		if !m.Start.Before(to) || !end.After(from) {
			continue
		}
		lane := m.Agent
		if by == "repo" {
			lane = m.Repo
		}
		left, width := place(m.Start, end)
		tip := fmt.Sprintf("Maintenance of %s: %s to %s (%s)", m.Repo, m.Start.Format("15:04"), end.Format("15:04"), took(m.Start, end))
		if m.Guess {
			tip += ", estimated: no maintenance has finished yet"
		} else if m.Longest > m.Dur {
			tip += ", typical of its last cycles; the longest was " + took(m.Start, m.Start.Add(m.Longest))
		}
		byLane[lane] = append(byLane[lane], placed{
			bar: tlBar{Name: "maintenance: " + m.Repo, Left: left, Width: width, Start: m.Start.Format("15:04"),
				End: end.Format("15:04"), Tip: tip, Guess: m.Guess, Past: m.Start.Before(now), Href: "/repositories", Maint: true},
			s: slot{m.Start, end},
		})
	}

	names := make([]string, 0, len(byLane))
	for n := range byLane {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ps := byLane[n]
		sort.SliceStable(ps, func(i, j int) bool { return ps[i].s.start.Before(ps[j].s.start) })
		// One row per job (a repository's maintenance counts as one), jobs in
		// name order and maintenance after them, so a job keeps its row when
		// it is dragged. Every run of a job is on its row.
		type rowKey struct {
			maint bool
			name  string
		}
		var keys []rowKey
		seen := map[rowKey]bool{}
		for _, p := range ps {
			if k := (rowKey{p.bar.Maint, p.bar.Name}); !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].maint != keys[j].maint {
				return !keys[i].maint
			}
			return keys[i].name < keys[j].name
		})
		rowOf := map[rowKey]int{}
		for i, k := range keys {
			rowOf[k] = i
		}
		for i := range ps {
			ps[i].bar.Row = rowOf[rowKey{ps[i].bar.Maint, ps[i].bar.Name}]
			for j := range ps {
				if i != j && ps[i].s.start.Before(ps[j].s.end) && ps[j].s.start.Before(ps[i].s.end) {
					ps[i].bar.Clash = true
				}
			}
		}
		lane := tlLane{Name: n, Rows: len(keys)}
		for _, p := range ps {
			lane.Bars = append(lane.Bars, p.bar)
		}
		spillNames(lane.Bars)
		lanes = append(lanes, lane)
	}
	sort.Strings(unscheduled)
	return lanes, ticks, pct(now), unscheduled
}
