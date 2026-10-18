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
