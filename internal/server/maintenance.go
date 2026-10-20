package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

const (
	// maintInterval is the fallback gap if a cron schedule cannot be read.
	// maintJitter is added to a retry after a failure, and maintFirstSpread
	// spreads the first runs after the server starts, so repositories never all
	// begin together.
	maintInterval    = 24 * time.Hour
	maintJitter      = time.Hour
	maintFirstSpread = 2 * time.Hour
	// maintRetry is the wait before trying again after a failed run.
	maintRetry   = time.Hour
	maintTimeout = 6 * time.Hour
)

// maintRand returns a random duration in [0, max). Tests replace it.
var maintRand = func(maxd time.Duration) time.Duration {
	n, err := rand.Int(rand.Reader, big.NewInt(int64(maxd)))
	if err != nil {
		return 0 // no jitter this time: harmless
	}
	return time.Duration(n.Int64())
}

func repoMaintKey(repoID string) string    { return "repo_maint:" + repoID }
func repoMaintRunKey(repoID string) string { return "maint:" + repoID }

// RepoMaintenance is what the server knows about a repository's maintenance.
type RepoMaintenance struct {
	LastRun time.Time `json:"last_run"` // zero until it has run
	Status  string    `json:"status"`   // "ok" or "failed"
	Error   string    `json:"error,omitempty"`
	AgentID string    `json:"agent_id,omitempty"` // the agent that ran, or last tried
	NextDue time.Time `json:"next_due"`
	// OwnerGone is set when the last cycle failed because the Kopia maintenance
	// owner (Owner, "user@host") is not a registered agent any more, which only
	// a hand-over to the server or an agent can fix.
	OwnerGone bool   `json:"owner_gone,omitempty"`
	Owner     string `json:"owner,omitempty"`
}

// OwnerGoneError says the repository's maintenance owner is not a registered
// agent (a replaced or removed host), so no agent can run its maintenance.
type OwnerGoneError struct{ Owner string }

func (e OwnerGoneError) Error() string {
	return fmt.Sprintf("the maintenance owner is %q, which is not a registered agent (a replaced or removed host?), so no agent can run maintenance", e.Owner)
}

// RepoMaintenance returns the saved state, or ok=false if the repository has
// never been seen by the maintenance schedule.
func (c *Controller) RepoMaintenance(ctx context.Context, repoID string) (RepoMaintenance, bool) {
	v, err := c.Catalog.GetSetting(ctx, repoMaintKey(repoID))
	if err != nil || v == "" {
		return RepoMaintenance{}, false
	}
	var m RepoMaintenance
	if err := json.Unmarshal([]byte(v), &m); err != nil {
		return RepoMaintenance{}, false
	}
	return m, true
}

func (c *Controller) saveMaintenance(ctx context.Context, repoID string, m RepoMaintenance) {
	b, _ := json.Marshal(m) // plain fields always marshal
	if err := c.Catalog.SetSetting(ctx, repoMaintKey(repoID), string(b)); err != nil {
		slog.Error("save maintenance state", "repo", repoID, "err", err)
	}
}

// MaintainRepository runs a full maintenance cycle now, on the repository's
// maintenance owner, and records the result and when the next one is due.
// Kopia lets only the owner do it: the first agent asked (one with a job on
// the repository) answers with the owner when it is not, and the owner, whose
// agent ID is the host part of that answer, is asked next.
func (c *Controller) MaintainRepository(ctx context.Context, repoID string) (RepoMaintenance, error) {
	repo, err := c.Catalog.Repository(ctx, repoID)
	if err != nil {
		return RepoMaintenance{}, err
	}
	if repo.InitializedAt.IsZero() {
		return RepoMaintenance{}, ErrNotInitialized
	}
	if c.IsRunning(repoWipeKey(repoID)) || !c.inflight.start(repoMaintRunKey(repoID)) {
		return RepoMaintenance{}, ErrRunning
	}
	defer c.inflight.done(repoMaintRunKey(repoID))

	now := time.Now()
	m := RepoMaintenance{LastRun: now, Status: "ok"}
	agentID, queued, err := c.runMaintenance(ctx, repo)
	m.AgentID = agentID
	c.recordMaintenanceRun(context.WithoutCancel(ctx), catalog.MaintenanceRun{
		RepositoryID: repo.ID, AgentID: agentID, Status: "ok", StartedAt: now, FinishedAt: time.Now(), Queued: queued}, err)
	if err != nil {
		m.Status, m.Error = "failed", err.Error()
		var gone OwnerGoneError
		if errors.As(err, &gone) {
			m.OwnerGone, m.Owner = true, gone.Owner
		}
		m.NextDue = now.Add(maintRetry + maintRand(maintJitter))
		slog.Error("maintenance failed", "repo", repo.ID, "name", repo.Name, "err", err)
	} else {
		m.NextDue = nextAfterRun(c.MaintSchedule(ctx, repoID), now, time.Now())
		slog.Info("maintenance finished", "repo", repo.ID, "name", repo.Name, "agent", agentID, "next", m.NextDue.Format(time.RFC3339))
	}
	c.saveMaintenance(context.WithoutCancel(ctx), repoID, m)
	return m, err
}

// RunJobThenMaintain backs a job up and, if that did not fail, runs a full
// maintenance cycle on its repository whether or not one is due. A failed
// backup usually means the repository can't be reached, so maintenance is
// skipped then. Maintenance that is already running is left to finish. The
// cycle is recorded like a scheduled one; its error, if any, is logged.
func (c *Controller) RunJobThenMaintain(ctx context.Context, jobID, trigger string) (catalog.Run, error) {
	run, err := c.RunJob(ctx, jobID, trigger)
	if err != nil || run.Status == catalog.RunFailed {
		return run, err
	}
	job, err := c.Catalog.Job(ctx, jobID)
	if err != nil {
		return run, err
	}
	if _, err := c.MaintainRepository(ctx, job.RepositoryID); err != nil && !errors.Is(err, ErrRunning) {
		slog.Error("maintenance after a manual backup", "job", job.Name, "err", err)
	}
	return run, nil
}

// recordMaintenanceRun saves a finished cycle: failed, with err, if err is set.
func (c *Controller) recordMaintenanceRun(ctx context.Context, m catalog.MaintenanceRun, err error) {
	if err != nil {
		m.Status, m.Error = "failed", err.Error()
	}
	if rerr := c.Catalog.AddMaintenanceRun(ctx, m); rerr != nil {
		slog.Error("record maintenance run", "repo", m.RepositoryID, "err", rerr)
	}
}

// runMaintenance runs a cycle on the maintenance owner. It returns the agent
// that ran it and how long the cycle waited for agents to finish earlier work.
func (c *Controller) runMaintenance(ctx context.Context, repo catalog.Repository) (string, time.Duration, error) {
	release, err := c.storageSlot(ctx, repo)
	if err != nil {
		return "", 0, err
	}
	defer release()
	if c.MaintainedBy(ctx, repo.ID) == MaintByServer {
		id, err := c.runServerMaintenance(ctx, repo)
		return id, 0, err
	}
	agent, err := c.agentForRepo(ctx, repo.ID)
	if err != nil {
		return "", 0, err
	}
	var queued time.Duration
	tried := map[string]bool{}
	for range 3 {
		tried[agent.ID] = true
		resp, err := withAgent(c, agent, maintTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.MaintainResponse, error) {
			return cl.Maintain(ctx, &agentpb.MaintainRequest{Repository: toProtoRepo(repo, agent.ID)})
		})
		if err != nil {
			return agent.ID, queued, fmt.Errorf("on %s: %w", agent.Name, err)
		}
		queued += time.Duration(resp.GetQueuedMs()) * time.Millisecond
		if resp.GetRan() {
			return agent.ID, queued, nil
		}
		owner := resp.GetOwner()
		next, err := c.Catalog.Agent(ctx, owner[strings.LastIndex(owner, "@")+1:])
		if err != nil || tried[next.ID] {
			return agent.ID, queued, OwnerGoneError{Owner: owner}
		}
		agent = next
	}
	return agent.ID, queued, errors.New("could not find the maintenance owner")
}

// startMaintenance starts the full cycle of every repository that has jobs
// and is due (see MaintSchedule for what due means). A repository seen for the
// first time gets a start time (a random one within maintFirstSpread, or the next
// cron time). A repository with a running job waits for the next
// tick; repositories without jobs are never scheduled (the button still works).
func (c *Controller) startMaintenance(ctx context.Context, now time.Time) {
	jobs, err := c.Catalog.ListJobs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("scheduler: list jobs", "err", err)
		}
		return
	}
	busy := map[string]bool{} // repository ID -> has a job, true if one is running
	byRepo := map[string][]catalog.Job{}
	for _, j := range jobs {
		byRepo[j.RepositoryID] = append(byRepo[j.RepositoryID], j)
		busy[j.RepositoryID] = busy[j.RepositoryID] || c.IsRunning(j.ID)
	}
	repos, err := c.Catalog.ListRepositories(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("scheduler: list repositories", "err", err)
		}
		return
	}
	for _, r := range repos {
		running, hasJobs := busy[r.ID]
		if !hasJobs || r.InitializedAt.IsZero() {
			continue
		}
		sched := c.MaintSchedule(ctx, r.ID)
		m, ok := c.RepoMaintenance(ctx, r.ID)
		if !ok {
			c.saveMaintenance(ctx, r.ID, RepoMaintenance{NextDue: firstDue(sched, RepoMaintenance{}, now)})
			continue
		}
		if running || c.IsRunning(repoMaintRunKey(r.ID)) || !c.maintenanceDue(ctx, sched, m, byRepo[r.ID], now) {
			continue
		}
		go func(id string) {
			if _, err := c.MaintainRepository(context.WithoutCancel(ctx), id); err != nil && !errors.Is(err, ErrRunning) {
				slog.Error("scheduled maintenance", "repo", id, "err", err)
			}
		}(r.ID)
	}
}

// TakeOverMaintenance hands a repository's maintenance to to: "server", or the
// ID of an agent assigned to it. It is the fix for a maintenance owner that is
// not a registered agent any more. The repository must be idle, as for any
// change of maintainer; afterwards its next cycle is due at once.
func (c *Controller) TakeOverMaintenance(ctx context.Context, repoID, to string) ([]string, error) {
	who, agentID := MaintByAgent, to
	if to == MaintByServer {
		who, agentID = MaintByServer, ""
	}
	warnings, err := c.SetMaintainer(ctx, repoID, who, agentID, "")
	if err != nil {
		return warnings, err
	}
	if m, ok := c.RepoMaintenance(ctx, repoID); ok {
		m.OwnerGone, m.Owner, m.Error = false, "", ""
		m.Status = "ok"
		m.NextDue = time.Now()
		c.saveMaintenance(ctx, repoID, m)
	}
	return warnings, nil
}
