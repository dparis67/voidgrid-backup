package agent

import (
	"context"

	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

// Progress reports the upload running now. It does not take opMu: the point is
// to answer while a backup holds it.
func (a *Agent) Progress(ctx context.Context, _ *agentpb.ProgressRequest) (*agentpb.ProgressResponse, error) {
	p, ok := a.engine.Progress()
	if !ok {
		return &agentpb.ProgressResponse{}, nil
	}
	return &agentpb.ProgressResponse{
		Running: true, Path: p.Path, StartedUnix: p.Started.Unix(),
		Files: p.Files, HashedFiles: p.HashedFiles, CachedFiles: p.CachedFiles,
		HashedBytes: p.HashedBytes, CachedBytes: p.CachedBytes, EstimatedBytes: p.EstimatedBytes, Dir: p.Dir,
	}, nil
}
