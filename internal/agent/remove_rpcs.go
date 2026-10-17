package agent

import (
	"context"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

func (a *Agent) DeleteSnapshot(ctx context.Context, req *agentpb.DeleteSnapshotRequest) (*agentpb.DeleteSnapshotResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	ids := req.GetSnapshotIds()
	if id := req.GetSnapshotId(); id != "" {
		ids = append([]string{id}, ids...)
	}
	logStarted("snapshot delete started", r.ID, "snapshots", len(ids))
	if err := a.engine.DeleteSnapshots(ctx, r, ids); err != nil {
		return nil, err
	}
	return &agentpb.DeleteSnapshotResponse{}, nil
}

func (a *Agent) WipeRepository(ctx context.Context, req *agentpb.WipeRepositoryRequest) (*agentpb.WipeRepositoryResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	logStarted("repository wipe started", r.ID, "location", r.Config.Location())
	blobs, bytes, err := a.engine.Wipe(ctx, r)
	if err != nil {
		return nil, err
	}
	return &agentpb.WipeRepositoryResponse{Blobs: blobs, Bytes: bytes}, nil
}

func (a *Agent) Maintain(ctx context.Context, req *agentpb.MaintainRequest) (*agentpb.MaintainResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	waiting := time.Now()
	a.opMu.Lock()
	defer a.opMu.Unlock()
	queued := time.Since(waiting)
	logStarted("maintenance started", r.ID)
	ran, owner, err := a.engine.Maintain(ctx, r)
	if err != nil {
		return nil, err
	}
	return &agentpb.MaintainResponse{Ran: ran, Owner: owner, QueuedMs: queued.Milliseconds()}, nil
}

// TestRepository opens the repository at the location in the request without
// creating anything, and reports its identity and maintenance owner.
func (a *Agent) TestRepository(ctx context.Context, req *agentpb.TestRepositoryRequest) (*agentpb.TestRepositoryResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	info, err := a.engine.Inspect(ctx, r)
	if err != nil {
		return nil, err
	}
	return &agentpb.TestRepositoryResponse{Warnings: info.Warnings, RepositoryId: info.ID, Owner: info.Owner}, nil
}

// TakeMaintenance makes this agent the repository's maintenance owner.
func (a *Agent) TakeMaintenance(ctx context.Context, req *agentpb.TakeMaintenanceRequest) (*agentpb.TakeMaintenanceResponse, error) {
	r, err := toRepo(req.GetRepository())
	if err != nil {
		return nil, err
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	logStarted("maintenance takeover started", r.ID)
	if err := a.engine.SetMaintenanceOwner(ctx, r, a.engine.UsernameAtHost()); err != nil {
		return nil, err
	}
	return &agentpb.TakeMaintenanceResponse{}, nil
}
