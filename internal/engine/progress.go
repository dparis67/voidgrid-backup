package engine

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/kopia/kopia/snapshot/upload"
)

// progressEvery is how often a running upload logs where it is.
var progressEvery = 30 * time.Second

// logProgress logs the counters of an upload every progressEvery until the
// returned stop function is called. path names the source in the log lines.
func logProgress(path string, p *upload.CountingUploadProgress) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(progressEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				c := p.Snapshot()
				args := []any{
					"path", path,
					"files", c.TotalHashedFiles + c.TotalCachedFiles,
					"hashed_files", c.TotalHashedFiles,
					"cached_files", c.TotalCachedFiles,
					"hashed_bytes", c.TotalHashedBytes,
					"dir", c.CurrentDirectory,
				}
				// Kopia estimates the source size in the background; until it
				// finishes there is no total to compare against.
				if c.EstimatedBytes > 0 {
					done := c.TotalHashedBytes + c.TotalCachedBytes
					args = append(args, "estimated_bytes", c.EstimatedBytes,
						"percent", fmt.Sprintf("%.0f", 100*float64(done)/float64(c.EstimatedBytes)))
				}
				slog.Info("backup progress", args...)
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// UploadProgress is what the upload running now has done so far.
type UploadProgress struct {
	Path           string
	Started        time.Time
	Files          int64 // hashed and unchanged files seen so far
	HashedFiles    int64
	CachedFiles    int64
	HashedBytes    int64
	CachedBytes    int64
	EstimatedBytes int64 // 0 until Kopia has finished sizing the source
	Dir            string
}

// trackUpload registers an upload so Progress can report it. The returned
// func ends it.
func (e *Engine) trackUpload(path string, p *upload.CountingUploadProgress) (done func()) {
	e.progMu.Lock()
	e.prog, e.progPath, e.progStart = p, path, time.Now()
	e.progMu.Unlock()
	return func() {
		e.progMu.Lock()
		if e.prog == p {
			e.prog = nil
		}
		e.progMu.Unlock()
	}
}

// Progress reports the upload running now; ok is false when none is.
func (e *Engine) Progress() (UploadProgress, bool) {
	e.progMu.Lock()
	p, path, start := e.prog, e.progPath, e.progStart
	e.progMu.Unlock()
	if p == nil {
		return UploadProgress{}, false
	}
	c := p.Snapshot()
	return UploadProgress{
		Path: path, Started: start,
		Files:       int64(c.TotalHashedFiles) + int64(c.TotalCachedFiles),
		HashedFiles: int64(c.TotalHashedFiles), CachedFiles: int64(c.TotalCachedFiles),
		HashedBytes: c.TotalHashedBytes, CachedBytes: c.TotalCachedBytes,
		EstimatedBytes: c.EstimatedBytes, Dir: c.CurrentDirectory,
	}, true
}
