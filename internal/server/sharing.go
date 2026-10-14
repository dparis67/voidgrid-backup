package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

// ErrNotAssigned is returned when a job names an agent that has not been
// assigned to the repository.
var ErrNotAssigned = errors.New("that agent is not assigned to the repository: assign it on the Repositories page first")

// AssignAgent lets an agent use a repository, after proving that it can: the
// agent opens the repository (never creating anything) with the key file that
// will apply to it. keyFile, for an SFTP repository, is that agent's own key
// path inside its container; blank uses the repository's default one. The
// same call re-tests an assigned agent or changes its key.
func (c *Controller) AssignAgent(ctx context.Context, repoID, agentID, keyFile string) (warnings []string, err error) {
	repo, err := c.Catalog.Repository(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if repo.InitializedAt.IsZero() {
		return nil, ErrNotInitialized
	}
	agent, err := c.Catalog.Agent(ctx, agentID)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	if c.IsRunning(repoWipeKey(repoID)) {
		return nil, ErrRunning
	}
	changed := false
	if keyFile = strings.TrimSpace(keyFile); keyFile != "" {
		if repo.Config.SFTP == nil {
			return nil, errors.New("a key file only applies to an SFTP repository")
		}
		s := *repo.Config.SFTP
		keys := map[string]string{}
		for k, v := range s.KeyFiles {
			keys[k] = v
		}
		changed = keys[agentID] != keyFile
		keys[agentID] = keyFile
		s.KeyFiles = keys
		repo.Config.SFTP = &s
		if err := repo.Config.Validate(); err != nil {
			return nil, err
		}
	}
	resp, err := withAgent(c, agent, initTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.TestRepositoryResponse, error) {
		return cl.TestRepository(ctx, &agentpb.TestRepositoryRequest{Repository: toProtoRepo(repo, agent.ID)})
	})
	if err != nil {
		return nil, fmt.Errorf("%s cannot open the repository: %w", agent.Name, unimplementedHint(err))
	}
	if resp.GetRepositoryId() == "" {
		return nil, fmt.Errorf("%s is too old to test the repository; update it to the server's version", agent.Name)
	}
	if changed {
		if err := c.Catalog.UpdateRepositoryConfig(ctx, repoID, repo.Config); err != nil {
			return resp.GetWarnings(), err
		}
	}
	return resp.GetWarnings(), c.Catalog.AssignAgent(ctx, repoID, agentID)
}

// UnassignAgent takes an agent off a repository. It is refused while the agent
// has jobs on it.
func (c *Controller) UnassignAgent(ctx context.Context, repoID, agentID string) error {
	jobs, err := c.Catalog.ListJobs(ctx)
	if err != nil {
		return err
	}
	n := 0
	for _, j := range jobs {
		if j.RepositoryID == repoID && j.AgentID == agentID {
			n++
		}
	}
	if n > 0 {
		return fmt.Errorf("the agent still has %d job(s) on this repository; delete or move them first", n)
	}
	return c.Catalog.UnassignAgent(ctx, repoID, agentID)
}

// checkAssigned is the job-creation rule: the agent must be assigned to the
// repository. A repository nobody is assigned to yet (one that existed before
// assignments did and has no jobs) lets the first agent through and assigns it.
func (c *Controller) checkAssigned(ctx context.Context, repoID, agentID string) error {
	assigned, err := c.Catalog.RepositoryAgents(ctx, repoID)
	if err != nil {
		return err
	}
	if len(assigned) == 0 {
		return c.Catalog.AssignAgent(ctx, repoID, agentID)
	}
	if !slices.Contains(assigned, agentID) {
		return ErrNotAssigned
	}
	return nil
}

// ReposFor lists the repositories an agent can take a new job on.
func (c *Controller) ReposFor(ctx context.Context, agentID string) ([]catalog.Repository, error) {
	repos, err := c.Catalog.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	var out []catalog.Repository
	for _, r := range repos {
		assigned, err := c.Catalog.RepositoryAgents(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		if len(assigned) == 0 || slices.Contains(assigned, agentID) {
			out = append(out, r)
		}
	}
	return out, nil
}
