package server

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

func TestRunProgressText(t *testing.T) {
	cases := []struct {
		name string
		p    RunProgress
		done int
		want []string
	}{
		{"scanning", RunProgress{Path: "/src", Files: 1200, Dir: "/src/library/2024"}, -1, []string{"scanning, 1200 files", "library/2024"}},
		{"estimated", RunProgress{Path: "/src", HashedBytes: 1 << 30, CachedBytes: 1 << 30, EstimatedBytes: 8 << 30, Dir: "/src/a/b/c/d"}, 25, []string{"25%", "2.0 GiB of 8.0 GiB", "…/c/d"}},
		{"past the estimate", RunProgress{HashedBytes: 9 << 30, EstimatedBytes: 8 << 30}, 100, []string{"100%"}},
		{"no dir yet", RunProgress{Path: "/src", Dir: "/src"}, -1, []string{"scanning, 0 files"}},
	}
	for _, c := range cases {
		if got := c.p.Done(); got != c.done {
			t.Errorf("%s: Done() = %d, want %d", c.name, got, c.done)
		}
		text := c.p.Text()
		for _, w := range c.want {
			if !strings.Contains(text, w) {
				t.Errorf("%s: %q does not contain %q", c.name, text, w)
			}
		}
	}
	if strings.Contains(RunProgress{Path: "/src", Dir: "/src"}.Text(), "·") {
		t.Error("an empty directory still adds a separator")
	}
}

func TestTrackProgressKeepsTheLastReplyAndClearsAtTheEnd(t *testing.T) {
	c := newController(t)
	if !c.inflight.start("job1") {
		t.Fatal("could not mark the job running")
	}
	defer c.inflight.done("job1")

	calls := make(chan struct{}, 100)
	var fail bool
	stop := c.trackProgress("job1", 5*time.Millisecond, func(context.Context) (*agentpb.ProgressResponse, error) {
		calls <- struct{}{}
		if fail {
			return nil, errors.New("unimplemented")
		}
		return &agentpb.ProgressResponse{Running: true, Path: "/src", Files: 7, HashedBytes: 40, EstimatedBytes: 100, Dir: "/src/x"}, nil
	})
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for i := 0; i < 400; i++ {
			if ok() {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("timed out waiting for " + what)
	}
	waitFor("a reply", func() bool { _, ok := c.JobProgress("job1"); return ok })
	if p, _ := c.JobProgress("job1"); p.Done() != 40 || p.Files != 7 {
		t.Fatalf("progress: %+v", p)
	}
	// An agent that cannot answer (an older one) leaves no progress.
	fail = true
	waitFor("progress to clear on an error", func() bool { _, ok := c.JobProgress("job1"); return !ok })
	fail = false
	waitFor("a reply again", func() bool { _, ok := c.JobProgress("job1"); return ok })

	stop()
	if _, ok := c.JobProgress("job1"); ok {
		t.Fatal("progress is still reported after the poller stopped")
	}
	n := len(calls)
	time.Sleep(30 * time.Millisecond)
	if len(calls) != n {
		t.Fatal("the poller kept asking after stop")
	}
	// Not running: nothing is shown even if a stale entry existed.
	c.progress.set("job2", RunProgress{Files: 1})
	if _, ok := c.JobProgress("job2"); ok {
		t.Fatal("progress shown for a job that is not running")
	}
}

func TestAgentAnswersProgressWhenIdle(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	agent, err := c.Catalog.Agent(ctx, job.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := withAgent(c, agent, 5*time.Second, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.ProgressResponse, error) {
		return cl.Progress(ctx, &agentpb.ProgressRequest{})
	})
	if err != nil || resp.GetRunning() {
		t.Fatalf("idle agent: %+v %v", resp, err)
	}
}

func TestJobsPagesShowProgress(t *testing.T) {
	c, job, _ := setupJob(t)
	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)

	if !c.inflight.start(job.ID) {
		t.Fatal("could not mark the job running")
	}
	defer c.inflight.done(job.ID)
	list := get(t, client, srv.URL+"/jobs", "running")
	if strings.Contains(list, "scanning") || !strings.Contains(list, `data-every="3000"`) {
		t.Fatalf("a running job with no report yet: plain running, fast refresh:\n%s", list)
	}
	c.progress.set(job.ID, RunProgress{Path: "/src", Files: 9, HashedBytes: 1 << 20, EstimatedBytes: 4 << 20, Dir: "/src/docs"})
	list = get(t, client, srv.URL+"/jobs", "25%")
	if !strings.Contains(list, "1.0 MiB of 4.0 MiB") {
		t.Fatalf("the Jobs list does not show the progress:\n%s", list)
	}
	get(t, client, srv.URL+"/jobs/"+job.ID, "25%")
}
