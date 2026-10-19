package server

import (
	"context"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

// maintSlot is a repository's next maintenance cycle as the Schedule page
// draws it.
type maintSlot struct {
	Repo    string        // the repository's name
	Agent   string        // the lane in the by-agent view: who runs it
	Start   time.Time     // when it is due; now if that has passed
	Dur     time.Duration // the median of its last 10 successful cycles
	Longest time.Duration // the longest of them
	Guess   bool          // no cycle has finished yet: Dur is a default
}

// maintDuration is how long a cycle of the repository is expected to take: the
// median of its last 10 successful cycles (see typicalDuration), never under
// overlapMin, or overlapDefault (guess) when none has finished. longest is the
// longest of them.
func maintDuration(runs []catalog.MaintenanceRun) (d, longest time.Duration, guess bool) {
	var took []time.Duration
	for _, r := range runs {
		if r.Status == "ok" {
			took = append(took, r.Took())
		}
	}
	return typicalDuration(took)
}

// maintSlots lists the next maintenance cycle of every repository that has jobs
// and a schedule. Cycles that are running now, or not yet scheduled, are left
// out.
func (c *Controller) maintSlots(ctx context.Context, repos []catalog.Repository, jobs []catalog.Job, agents []catalog.Agent, now time.Time) []maintSlot {
	agentName := map[string]string{}
	for _, a := range agents {
		agentName[a.ID] = a.Name
	}
	firstAgent := map[string]string{} // repository ID -> the agent of its first job
	for _, j := range jobs {
		if _, ok := firstAgent[j.RepositoryID]; !ok {
			firstAgent[j.RepositoryID] = agentName[j.AgentID]
		}
	}
	var out []maintSlot
	for _, r := range repos {
		lane, hasJobs := firstAgent[r.ID]
		if !hasJobs || r.InitializedAt.IsZero() || c.IsRunning(repoMaintRunKey(r.ID)) {
			continue
		}
		m, ok := c.RepoMaintenance(ctx, r.ID)
		if !ok || m.NextDue.IsZero() {
			continue
		}
		if c.MaintainedBy(ctx, r.ID) == MaintByServer {
			lane = "the server"
		} else if n := agentName[m.AgentID]; n != "" {
			lane = n
		}
		runs, err := c.Catalog.ListMaintenanceRuns(ctx, r.ID, 10)
		if err != nil {
			continue
		}
		d, longest, guess := maintDuration(runs)
		var repoJobs []catalog.Job
		for _, j := range jobs {
			if j.RepositoryID == r.ID {
				repoJobs = append(repoJobs, j)
			}
		}
		start := c.maintEstimate(ctx, c.MaintSchedule(ctx, r.ID), m, repoJobs, now)
		if start.Before(now) {
			start = now // overdue: it starts at the next scheduler tick
		}
		out = append(out, maintSlot{Repo: r.Name, Agent: lane, Start: start, Dur: d, Longest: longest, Guess: guess})
	}
	return out
}
