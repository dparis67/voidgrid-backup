package server

import (
	"fmt"
	"sort"
	"time"

	"github.com/robfig/cron/v3"
)

const (
	overlapHorizon  = 24 * time.Hour
	overlapMaxStart = 50               // start times considered per job
	overlapDefault  = 15 * time.Minute // assumed length of a job that never ran
	overlapMin      = 5 * time.Minute  // a job occupies its host at least this long
)

type slot struct{ start, end time.Time }

// jobSlots lists when a scheduled job would occupy its host over the horizon:
// each start time, lasting as long as its last backup took.
func jobSlots(r jobRow, now time.Time) []slot {
	if !r.Enabled || r.Schedule == "" {
		return nil
	}
	sched, err := cron.ParseStandard(r.Schedule)
	if err != nil {
		return nil
	}
	d := overlapDefault
	if r.Last != nil && !r.Last.FinishedAt.IsZero() {
		d = max(r.Last.FinishedAt.Sub(r.Last.StartedAt), overlapMin)
	}
	var out []slot
	for t := sched.Next(now); t.Before(now.Add(overlapHorizon)) && len(out) < overlapMaxStart; t = sched.Next(t) {
		out = append(out, slot{t, t.Add(d)})
	}
	return out
}

// findClashes maps a job ID to a note for every other scheduled job it would
// run alongside in the next 24 hours: jobs on the same agent queue behind
// that agent's operation lock, and jobs on the same repository compete for
// its connections and bandwidth.
func findClashes(rows []jobRow, now time.Time) map[string][]string {
	slots := make([][]slot, len(rows))
	for i, r := range rows {
		slots[i] = jobSlots(r, now)
	}
	out := map[string][]string{}
	for i := range rows {
		for j := i + 1; j < len(rows); j++ {
			why := ""
			switch {
			case rows[i].AgentID == rows[j].AgentID:
				why = "same agent"
			case rows[i].RepositoryID == rows[j].RepositoryID:
				why = "same repository"
			default:
				continue
			}
			if at, ok := firstOverlap(slots[i], slots[j]); ok {
				out[rows[i].ID] = append(out[rows[i].ID], fmt.Sprintf("%s (%s, %s)", rows[j].Name, why, at.Format("15:04")))
				out[rows[j].ID] = append(out[rows[j].ID], fmt.Sprintf("%s (%s, %s)", rows[i].Name, why, at.Format("15:04")))
			}
		}
	}
	for _, v := range out {
		sort.Strings(v)
	}
	return out
}

// firstOverlap returns the earliest moment at which a slot of a and a slot of
// b are both active.
func firstOverlap(a, b []slot) (time.Time, bool) {
	var best time.Time
	for _, x := range a {
		for _, y := range b {
			if x.start.Before(y.end) && y.start.Before(x.end) {
				at := x.start
				if y.start.After(at) {
					at = y.start
				}
				if best.IsZero() || at.Before(best) {
					best = at
				}
			}
		}
	}
	return best, !best.IsZero()
}
