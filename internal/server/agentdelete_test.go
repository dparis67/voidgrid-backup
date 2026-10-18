package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

// postStatus posts a form the way the UI does and returns the status, the
// Location header and the body, without following the redirect.
func postStatus(t *testing.T, client *http.Client, u string, form url.Values) (int, string, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	nr := &http.Client{Jar: client.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := nr.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("Location"), string(b)
}

func TestDeleteAgentWithNothingElseToMaintain(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)

	_, err := c.DeleteAgent(ctx, job.AgentID)
	if blocked, ok := isAgentInUse(err); !ok || len(blocked.Jobs) != 1 || blocked.Jobs[0] != job.Name {
		t.Fatalf("an agent with a job was not refused: %v", err)
	}
	if err := c.Catalog.DeleteJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	// No other agent and no server access: who maintains the repository cannot
	// be read, which is reported but does not stop the delete.
	warnings, err := c.DeleteAgent(ctx, job.AgentID)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "local") {
		t.Fatalf("delete: %v %v", warnings, err)
	}
	if _, err := c.Catalog.Agent(ctx, job.AgentID); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("the agent is still there: %v", err)
	}
	if ids, _ := c.Catalog.RepositoryAgents(ctx, job.RepositoryID); len(ids) != 0 {
		t.Fatalf("assignments left behind: %v", ids)
	}
	if _, err := c.DeleteAgent(ctx, job.AgentID); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("deleting it twice: %v", err)
	}
}

func TestDeleteAgentThatMaintainsARepository(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	b, addrB := startAgent(t, t.TempDir())
	agB, err := enrollTestAgent(t, c, b, "box2", addrB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AssignAgent(ctx, job.RepositoryID, agB.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := c.Catalog.DeleteJob(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	repo, _ := c.Catalog.Repository(ctx, job.RepositoryID)

	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)

	// The first agent created the repository, so it owns its maintenance.
	code, _, _ := postStatus(t, client, srv.URL+"/agents/"+job.AgentID+"/delete", url.Values{"confirm": {"wrong"}})
	if code != http.StatusSeeOther {
		t.Fatalf("a wrong name: %d", code)
	}
	code, _, body := postStatus(t, client, srv.URL+"/agents/"+job.AgentID+"/delete", url.Values{"confirm": {"box"}})
	if code != http.StatusConflict || !strings.Contains(body, "cannot be deleted yet") ||
		!strings.Contains(body, "/repositories/"+repo.ID+"/maintainer") || !strings.Contains(body, "box2") {
		t.Fatalf("a refused delete did not offer the hand-over: %d\n%s", code, body)
	}
	if _, err := c.Catalog.Agent(ctx, job.AgentID); err != nil {
		t.Fatalf("a refused delete removed the agent: %v", err)
	}

	// Hand maintenance to the other agent from that form, then delete.
	code, loc, _ := postStatus(t, client, srv.URL+"/repositories/"+repo.ID+"/maintainer",
		url.Values{"from": {"agents"}, "confirm": {repo.Name}, "who": {"agent"}, "agent": {agB.ID}})
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, "/?notice=") {
		t.Fatalf("hand-over: %d %q", code, loc)
	}
	code, _, body = postStatus(t, client, srv.URL+"/agents/"+job.AgentID+"/delete", url.Values{"confirm": {"box"}})
	if code != http.StatusSeeOther {
		t.Fatalf("delete after the hand-over: %d\n%s", code, body)
	}
	if _, err := c.Catalog.Agent(ctx, job.AgentID); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("the agent is still there: %v", err)
	}
	if ids, _ := c.Catalog.RepositoryAgents(ctx, repo.ID); len(ids) != 1 || ids[0] != agB.ID {
		t.Fatalf("assignments after the delete: %v", ids)
	}
}
