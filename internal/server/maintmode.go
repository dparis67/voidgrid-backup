package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

// The two ways a repository's scheduled maintenance can be timed.
const (
	// MaintAfterJobs runs it once the repository's scheduled jobs have all run
	// since the last cycle. It is the default.
	MaintAfterJobs = "after"
	// MaintAtTime runs it at the times of a cron expression, whatever the jobs do.
	MaintAtTime = "at"
)

const (
	// maintAfterGap is the least time between the starts of two "after jobs"
	// cycles, so a job that runs every hour does not cause a cycle every hour.
	maintAfterGap = 20 * time.Hour
	// maintHorizon: a scheduled job that has not run since the last cycle only
	// holds up an "after jobs" cycle when it is due within this long. A weekly
	// job is not waited for by a daily cycle.
	maintHorizon = 12 * time.Hour
	// maintOverdue: an "after jobs" cycle that has waited this long past its
	// earliest time runs regardless, so a job that never runs cannot starve it.
	maintOverdue = 48 * time.Hour
)

// MaintSchedule is when a repository's scheduled maintenance runs.
type MaintSchedule struct {
	Mode string `json:"mode"`           // MaintAfterJobs or MaintAtTime
	Cron string `json:"cron,omitempty"` // for MaintAtTime
}

func repoMaintSchedKey(repoID string) string { return "repo_maint_sched:" + repoID }

// MaintSchedule returns the repository's maintenance timing: after its jobs
// unless it was set otherwise.
func (c *Controller) MaintSchedule(ctx context.Context, repoID string) MaintSchedule {
	v, err := c.Catalog.GetSetting(ctx, repoMaintSchedKey(repoID))
	var s MaintSchedule
	if err != nil || v == "" || json.Unmarshal([]byte(v), &s) != nil || s.Mode != MaintAtTime {
		return MaintSchedule{Mode: MaintAfterJobs}
	}
	if _, err := cron.ParseStandard(s.Cron); err != nil {
		return MaintSchedule{Mode: MaintAfterJobs}
	}
	return s
}

// SetMaintSchedule saves how the repository's maintenance is timed and moves
// its next cycle to match: the next cron time for MaintAtTime, and the end of
// the minimum gap after the last cycle for MaintAfterJobs. The cron is read in
// the server's local zone, like a job's.
func (c *Controller) SetMaintSchedule(ctx context.Context, repoID, mode, expr string) error {
	if _, err := c.Catalog.Repository(ctx, repoID); err != nil {
		return err
	}
	s := MaintSchedule{Mode: mode}
	switch mode {
	case MaintAfterJobs:
	case MaintAtTime:
		expr = strings.TrimSpace(expr)
		if _, err := cron.ParseStandard(expr); err != nil {
			return fmt.Errorf("the schedule %q is not a valid cron expression: %w", expr, err)
		}
		s.Cron = expr
	default:
		return errors.New(`the mode must be "after" or "at"`)
	}
	b, _ := json.Marshal(s) // plain fields always marshal
	if err := c.Catalog.SetSetting(ctx, repoMaintSchedKey(repoID), string(b)); err != nil {
		return err
	}
	if m, ok := c.RepoMaintenance(ctx, repoID); ok {
		m.NextDue = firstDue(s, m, time.Now())
		c.saveMaintenance(ctx, repoID, m)
	}
	return nil
}

// firstDue is the earliest start of the next cycle after the schedule changes
// or is first seen.
func firstDue(s MaintSchedule, m RepoMaintenance, now time.Time) time.Time {
	if s.Mode == MaintAtTime {
		return nextAt(s, now)
	}
	if !m.LastRun.IsZero() {
		return m.LastRun.Add(maintAfterGap)
	}
	return now.Add(maintRand(maintFirstSpread))
}

// nextAt is the next time of a MaintAtTime schedule after from.
func nextAt(s MaintSchedule, from time.Time) time.Time {
	sched, err := cron.ParseStandard(s.Cron)
	if err != nil {
		return from.Add(maintInterval)
	}
	return sched.Next(from.In(time.Local))
}

// nextAfterRun is when the cycle after one that finished at done is due.
func nextAfterRun(s MaintSchedule, started, done time.Time) time.Time {
	if s.Mode == MaintAtTime {
		return nextAt(s, done)
	}
	return started.Add(maintAfterGap)
}

// afterJobsDue says whether an "after jobs" repository is due. waiting is the
// number of its enabled scheduled jobs that have not run since the last cycle
// and are due within maintHorizon; ran says some job has run since. A failed
// cycle is retried at its NextDue whatever the jobs did.
func afterJobsDue(now time.Time, m RepoMaintenance, waiting int, ran bool) bool {
	if now.Before(m.NextDue) {
		return false
	}
	if m.Status == "failed" || now.Sub(m.NextDue) >= maintOverdue {
		return true
	}
	return waiting == 0 && ran
}

// jobsSince counts, for the repository's jobs, how many scheduled ones are
// still to run before an "after jobs" cycle, and whether any has run since the
// last cycle began. A job that failed has still run.
func (c *Controller) jobsSince(ctx context.Context, jobs []catalog.Job, last, now time.Time) (waiting int, ran bool) {
	for _, j := range jobs {
		lr, err := c.Catalog.LastRun(ctx, j.ID, "backup")
		hasRun := err == nil && lr.StartedAt.After(last)
		if hasRun {
			ran = true
			continue
		}
		if !j.Enabled || j.Schedule == "" {
			continue
		}
		if next, err := NextRun(ctx, c.Catalog, j); err == nil && next.Before(now.Add(maintHorizon)) {
			waiting++
		}
	}
	return waiting, ran
}

// maintenanceDue says whether the repository's scheduled cycle should start.
func (c *Controller) maintenanceDue(ctx context.Context, s MaintSchedule, m RepoMaintenance, jobs []catalog.Job, now time.Time) bool {
	if s.Mode == MaintAtTime {
		return !now.Before(m.NextDue)
	}
	waiting, ran := c.jobsSince(ctx, jobs, m.LastRun, now)
	return afterJobsDue(now, m, waiting, ran)
}

// describe is the schedule as the Repositories page shows it.
func (s MaintSchedule) describe() string {
	if s.Mode == MaintAtTime {
		return "at " + s.Cron
	}
	return "after its jobs"
}

// maintEstimate is when the next cycle is expected to start, for the Schedule
// page: the cron time, or for "after jobs" the later of the earliest time and
// the end of the last of the repository's jobs due within maintHorizon.
func (c *Controller) maintEstimate(ctx context.Context, s MaintSchedule, m RepoMaintenance, jobs []catalog.Job, now time.Time) time.Time {
	if s.Mode == MaintAtTime {
		return m.NextDue
	}
	at := m.NextDue
	for _, j := range jobs {
		if !j.Enabled || j.Schedule == "" {
			continue
		}
		next, err := NextRun(ctx, c.Catalog, j)
		if err != nil || !next.Before(now.Add(maintHorizon)) {
			continue
		}
		if next.Before(now) {
			next = now
		}
		d, _, _ := c.jobDuration(ctx, j.ID)
		if end := next.Add(d); end.After(at) {
			at = end
		}
	}
	return at
}
