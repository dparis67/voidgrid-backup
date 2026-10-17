package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/engine"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

// EngineHost is the Kopia host name of the server's own engine, so the
// maintenance owner it records is stable: voidgrid-backup@voidgrid-server.
const EngineHost = "voidgrid-server"

// Who maintains a repository.
const (
	MaintByAgent  = "agent"  // the default: whichever agent owns it in Kopia
	MaintByServer = "server" // the server, through its own engine
)

func repoMaintByKey(repoID string) string { return "repo_maintby:" + repoID }

// MaintainedBy says who runs maintenance for a repository. Anything but an
// explicit "server" is the agent, which is how every existing repository
// behaves.
func (c *Controller) MaintainedBy(ctx context.Context, repoID string) string {
	if v, err := c.Catalog.GetSetting(ctx, repoMaintByKey(repoID)); err == nil && v == MaintByServer {
		return MaintByServer
	}
	return MaintByAgent
}

// serverRepo is the repository as the server's engine opens it.
func serverRepo(r catalog.Repository) (engine.Repo, error) {
	cfg, err := r.Config.ForServer()
	if err != nil {
		return engine.Repo{}, err
	}
	return engine.Repo{ID: r.ID, Config: cfg, Password: r.Password}, nil
}

// lockRepoIdle takes every in-flight key that touches the repository (its
// jobs, maintenance, wipe) or fails with ErrRunning, so that nothing writes
// while the maintenance owner changes. The returned function releases them.
func (c *Controller) lockRepoIdle(ctx context.Context, repoID string) (func(), error) {
	jobs, err := c.Catalog.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	keys := []string{repoMaintRunKey(repoID), repoWipeKey(repoID)}
	for _, j := range jobs {
		if j.RepositoryID == repoID {
			keys = append(keys, j.ID)
		}
	}
	var held []string
	release := func() {
		for _, k := range held {
			c.inflight.done(k)
		}
	}
	for _, k := range keys {
		if !c.inflight.start(k) {
			release()
			return nil, ErrRunning
		}
		held = append(held, k)
	}
	return release, nil
}

// SetMaintainer changes who runs maintenance for a repository.
//
// To "server": the server opens the repository itself first (never creating
// anything). For a filesystem repository, where the server reaches the data
// through its own mount (serverPath, blank keeps the stored one), an agent that
// uses the repository must also report the same repository ID, so a wrong or
// unmounted directory is refused; if no agent can answer, nothing changes.
// Only then does the server record itself as the Kopia maintenance owner.
//
// To "agent": agentID, a registered agent, opens the repository and makes
// itself the owner. The server needs no access to the repository for this.
func (c *Controller) SetMaintainer(ctx context.Context, repoID, who, agentID, serverPath string) (warnings []string, err error) {
	repo, err := c.Catalog.Repository(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if repo.InitializedAt.IsZero() {
		return nil, ErrNotInitialized
	}
	switch who {
	case MaintByServer, MaintByAgent:
	default:
		return nil, fmt.Errorf("unknown maintainer %q", who)
	}
	release, err := c.lockRepoIdle(ctx, repoID)
	if err != nil {
		return nil, err
	}
	defer release()

	if who == MaintByAgent {
		agent, err := c.Catalog.Agent(ctx, agentID)
		if err != nil {
			return nil, fmt.Errorf("agent: %w", err)
		}
		if _, err := withAgent(c, agent, initTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.TakeMaintenanceResponse, error) {
			return cl.TakeMaintenance(ctx, &agentpb.TakeMaintenanceRequest{Repository: toProtoRepo(repo, agent.ID)})
		}); err != nil {
			return nil, fmt.Errorf("on %s: %w", agent.Name, unimplementedHint(err))
		}
		return nil, c.Catalog.SetSetting(ctx, repoMaintByKey(repoID), MaintByAgent)
	}

	if c.Engine == nil {
		return nil, errors.New("this server has no repository engine")
	}
	if sp := strings.TrimSpace(serverPath); sp != "" {
		if repo.Config.Filesystem == nil {
			return nil, errors.New("a path on the server only applies to a filesystem repository")
		}
		f := *repo.Config.Filesystem
		f.ServerPath = sp
		repo.Config.Filesystem = &f
		if err := repo.Config.Validate(); err != nil {
			return nil, err
		}
	}
	er, err := serverRepo(repo)
	if err != nil {
		return nil, err
	}
	info, err := c.Engine.Inspect(ctx, er)
	if err != nil {
		return nil, fmt.Errorf("the server cannot open the repository: %w", err)
	}
	warnings = info.Warnings
	if repo.Config.Filesystem != nil {
		if err := c.sameRepositoryAsAgent(ctx, repo, info.ID); err != nil {
			return warnings, err
		}
	}
	if err := c.Engine.SetMaintenanceOwner(ctx, er, c.Engine.UsernameAtHost()); err != nil {
		return warnings, fmt.Errorf("take over maintenance: %w", err)
	}
	if serverPath != "" {
		if err := c.Catalog.UpdateRepositoryConfig(ctx, repoID, repo.Config); err != nil {
			return warnings, err
		}
	}
	return warnings, c.Catalog.SetSetting(ctx, repoMaintByKey(repoID), MaintByServer)
}

// unimplementedHint turns "agent does not have this call" into advice.
// ApplyMaintainer carries out the add-job form's choice of who maintains the
// job's repository: "" leaves it, "server" or "agent" (the job's own agent)
// sets it. The setting belongs to the repository, so it holds for every job on
// it. It returns a sentence for the notice, "" when nothing was asked.
func (c *Controller) ApplyMaintainer(ctx context.Context, job catalog.Job, choice string) string {
	var who, agentID, name string
	switch choice {
	case "":
		return ""
	case MaintByServer:
		who, name = MaintByServer, "the server"
	case MaintByAgent:
		who, agentID, name = MaintByAgent, job.AgentID, "its agent"
		if a, err := c.Catalog.Agent(ctx, job.AgentID); err == nil {
			name = a.Name
		}
	default:
		return " Maintenance was not changed: unknown choice."
	}
	if _, err := c.SetMaintainer(ctx, job.RepositoryID, who, agentID, ""); err != nil {
		return " Maintenance was not changed: " + err.Error()
	}
	return " Maintenance of the repository is now done by " + name + "."
}

func unimplementedHint(err error) error {
	if status.Code(err) == codes.Unimplemented {
		return errors.New("this agent is too old for that; update it to the same version as the server")
	}
	return err
}

// sameRepositoryAsAgent asks the agents that use the repository (all agents if
// none does) to open it at their own path and checks that the repository ID
// matches serverID, the one the server read through its mapped path. The first
// agent that answers decides; if none can, it is an error and nothing changes.
func (c *Controller) sameRepositoryAsAgent(ctx context.Context, repo catalog.Repository, serverID string) error {
	jobs, err := c.Catalog.ListJobs(ctx)
	if err != nil {
		return err
	}
	agents, err := c.Catalog.ListAgents(ctx)
	if err != nil {
		return err
	}
	using := map[string]bool{}
	for _, j := range jobs {
		if j.RepositoryID == repo.ID {
			using[j.AgentID] = true
		}
	}
	var candidates []catalog.Agent
	for _, a := range agents {
		if using[a.ID] {
			candidates = append(candidates, a)
		}
	}
	if len(candidates) == 0 {
		candidates = agents
	}
	var problems []string
	for _, a := range candidates {
		resp, err := withAgent(c, a, initTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.TestRepositoryResponse, error) {
			return cl.TestRepository(ctx, &agentpb.TestRepositoryRequest{Repository: toProtoRepo(repo, a.ID)})
		})
		switch {
		case err != nil:
			problems = append(problems, fmt.Sprintf("%s: %v", a.Name, unimplementedHint(err)))
		case resp.GetRepositoryId() == "":
			problems = append(problems, a.Name+": too old to report the repository ID; update it")
		case resp.GetRepositoryId() != serverID:
			return fmt.Errorf("the path on the server is not the same repository as the agent's (%s sees a different one); check the mount", a.Name)
		default:
			return nil
		}
	}
	if len(problems) == 0 {
		return errors.New("no agent is registered to confirm the path on the server is the same repository")
	}
	return fmt.Errorf("no agent could confirm the path on the server is the same repository, so nothing was changed (%s)", strings.Join(problems, "; "))
}

// runServerMaintenance is a full maintenance cycle run by the server's own
// engine. If someone else became the owner meanwhile it says so rather than
// running.
func (c *Controller) runServerMaintenance(ctx context.Context, repo catalog.Repository) (string, error) {
	if c.Engine == nil {
		return "server", errors.New("this server has no repository engine")
	}
	er, err := serverRepo(repo)
	if err != nil {
		return "server", err
	}
	ctx, cancel := context.WithTimeout(ctx, maintTimeout)
	defer cancel()
	ran, owner, err := c.Engine.Maintain(ctx, er)
	if err != nil {
		return "server", err
	}
	if !ran {
		return "server", fmt.Errorf("the maintenance owner is now %q, not this server; set the repository's maintainer again", owner)
	}
	return "server", nil
}
