package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

// reachResult is whether one place can open a repository.
type reachResult struct {
	Who    string // an agent's name, or "the server"
	OK     bool
	Detail string // why not, or a warning
}

// CheckReach asks every agent assigned to the repository, and the server, to
// open it, and reports each answer. It is how you find out, before something
// needs it, that the server can reach a repository its agents cannot, or the
// other way round. Opening creates nothing in the storage.
func (c *Controller) CheckReach(ctx context.Context, repoID string) ([]reachResult, error) {
	repo, err := c.Catalog.Repository(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if repo.InitializedAt.IsZero() {
		return nil, ErrNotInitialized
	}
	ids, err := c.Catalog.RepositoryAgents(ctx, repoID)
	if err != nil {
		return nil, err
	}
	results := make([]reachResult, len(ids)+1)
	var wg sync.WaitGroup
	for i, id := range ids {
		agent, err := c.Catalog.Agent(ctx, id)
		if err != nil {
			results[i] = reachResult{Who: id, Detail: err.Error()}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := withAgent(c, agent, initTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.TestRepositoryResponse, error) {
				return cl.TestRepository(ctx, &agentpb.TestRepositoryRequest{Repository: toProtoRepo(repo, agent.ID)})
			})
			switch {
			case err != nil:
				results[i] = reachResult{Who: agent.Name, Detail: unimplementedHint(err).Error()}
			case resp.GetRepositoryId() == "":
				results[i] = reachResult{Who: agent.Name, Detail: "too old to test the repository; update it"}
			default:
				results[i] = reachResult{Who: agent.Name, OK: true, Detail: strings.Join(resp.GetWarnings(), " ")}
			}
		}()
	}
	results[len(ids)] = c.serverReach(ctx, repo)
	wg.Wait()
	return results, nil
}

func (c *Controller) serverReach(ctx context.Context, repo catalog.Repository) reachResult {
	r := reachResult{Who: "the server"}
	if err := c.ServerCanOpen(repo); err != nil {
		r.Detail = err.Error()
		return r
	}
	er, err := serverRepo(repo)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	info, err := c.Engine.Inspect(ctx, er)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	r.OK, r.Detail = true, strings.Join(info.Warnings, " ")
	return r
}

// reachText is the results as one notice.
func reachText(name string, results []reachResult) string {
	var parts []string
	for _, r := range results {
		d := r.Detail
		if len(d) > 160 {
			d = d[:160] + "…"
		}
		switch {
		case r.OK && d == "":
			parts = append(parts, r.Who+": can open it")
		case r.OK:
			parts = append(parts, r.Who+": can open it (warning: "+d+")")
		default:
			parts = append(parts, r.Who+": cannot open it ("+d+")")
		}
	}
	return fmt.Sprintf("Access to %s. %s.", name, strings.Join(parts, "; "))
}

// reachRepository reports who can open a repository.
func (u *ui) reachRepository(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rp, err := u.c.Catalog.Repository(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	results, err := u.c.CheckReach(r.Context(), id)
	if err != nil {
		redirectNotice(w, r, "/repositories", "Not checked: "+err.Error())
		return
	}
	redirectNotice(w, r, "/repositories", reachText(rp.Name, results))
}
