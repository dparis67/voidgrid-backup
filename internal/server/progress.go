package server

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

// progressEvery is how often a running job's agent is asked where the upload
// is, and progressTimeout bounds each question so a slow agent cannot stall
// the poller.
const (
	progressEvery   = 5 * time.Second
	progressTimeout = 3 * time.Second
)

// RunProgress is where a running job's upload was at the last poll.
type RunProgress struct {
	Path                            string
	Files, HashedFiles, CachedFiles int64
	HashedBytes, CachedBytes        int64
	EstimatedBytes                  int64 // 0 until Kopia has sized the source
	Dir                             string
	At                              time.Time
}

// Done is the share of the estimated size dealt with, 0 to 100, or -1 while
// there is no estimate yet. Files taken unchanged from the previous snapshot
// count as done.
func (p RunProgress) Done() int {
	if p.EstimatedBytes <= 0 {
		return -1
	}
	return int(min(100, (p.HashedBytes+p.CachedBytes)*100/p.EstimatedBytes))
}

// Text is the line shown next to "running": the percentage and sizes once the
// source has been sized, the file count before that, and the directory being
// read.
func (p RunProgress) Text() string {
	var parts []string
	if d := p.Done(); d >= 0 {
		parts = append(parts, fmt.Sprintf("%d%%", d),
			fmt.Sprintf("%s of %s", HumanBytes(p.HashedBytes+p.CachedBytes), HumanBytes(p.EstimatedBytes)))
	} else {
		parts = append(parts, fmt.Sprintf("scanning, %d files", p.Files))
	}
	if dir := p.relDir(); dir != "" {
		parts = append(parts, dir)
	}
	return strings.Join(parts, " · ")
}

// relDir is the directory being read, relative to the source when it is under
// it, and the last two path elements when it is still long.
func (p RunProgress) relDir() string {
	dir := p.Dir
	if p.Path != "" && p.Path != "/" {
		dir = strings.TrimPrefix(dir, strings.TrimRight(p.Path, "/"))
	}
	dir = strings.Trim(dir, "/")
	if dir == "" {
		return ""
	}
	parts := strings.Split(dir, "/")
	if len(parts) > 2 {
		return "…/" + path.Join(parts[len(parts)-2:]...)
	}
	return dir
}

// progressBook keeps the last RunProgress of each running job.
type progressBook struct {
	mu sync.Mutex
	m  map[string]RunProgress
}

func (b *progressBook) set(jobID string, p RunProgress) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = map[string]RunProgress{}
	}
	b.m[jobID] = p
}

func (b *progressBook) clear(jobID string) {
	b.mu.Lock()
	delete(b.m, jobID)
	b.mu.Unlock()
}

func (b *progressBook) get(jobID string) (RunProgress, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.m[jobID]
	return p, ok
}

// JobProgress is the last known progress of a running job's upload. ok is false
// when it is not running, its agent has not reported an upload yet (queued, or
// preparing), or the agent is too old to report one.
func (c *Controller) JobProgress(jobID string) (RunProgress, bool) {
	if !c.IsRunning(jobID) {
		return RunProgress{}, false
	}
	return c.progress.get(jobID)
}

// trackProgress polls fetch every `every` until the returned stop func is
// called, keeping what it reports under jobID. A reply with nothing running
// (or an error, such as an older agent without the call) leaves no progress.
func (c *Controller) trackProgress(jobID string, every time.Duration, fetch func(context.Context) (*agentpb.ProgressResponse, error)) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(context.Background(), progressTimeout)
				resp, err := fetch(ctx)
				cancel()
				if err != nil || !resp.GetRunning() {
					c.progress.clear(jobID)
					continue
				}
				c.progress.set(jobID, RunProgress{
					Path: resp.GetPath(), Files: resp.GetFiles(), HashedFiles: resp.GetHashedFiles(),
					CachedFiles: resp.GetCachedFiles(), HashedBytes: resp.GetHashedBytes(),
					CachedBytes: resp.GetCachedBytes(), EstimatedBytes: resp.GetEstimatedBytes(),
					Dir: resp.GetDir(), At: time.Now(),
				})
			}
		}
	}()
	return func() {
		close(done)
		<-finished
		c.progress.clear(jobID)
	}
}

// pollAgentProgress starts trackProgress against agent's Progress call.
func (c *Controller) pollAgentProgress(jobID string, agent catalog.Agent) (stop func()) {
	return c.trackProgress(jobID, progressEvery, func(ctx context.Context) (*agentpb.ProgressResponse, error) {
		return withAgent(c, agent, progressTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.ProgressResponse, error) {
			return cl.Progress(ctx, &agentpb.ProgressRequest{})
		})
	})
}
