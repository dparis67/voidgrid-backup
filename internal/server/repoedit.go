package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

// editTimeout bounds one location edit: the move is a single rename, but the
// agent may have to finish another operation first.
const editTimeout = 15 * time.Minute

// RepoEdit is what an edit may change. A blank field keeps what is stored, so
// secrets are never sent back to the browser. Kind, host, port, user, bucket,
// endpoint, prefix and the Kopia password are fixed: changing them is a
// different repository.
type RepoEdit struct {
	SFTPPath, SFTPKeyFile, SFTPPassword, SFTPKnownHosts string
	S3AccessKey, S3Secret                               string
	FSPath                                              string
}

// apply returns cfg with the edit's non-blank fields set.
func (e RepoEdit) apply(cfg repocfg.Config) (repocfg.Config, error) {
	out := cfg
	set := func(dst *string, v string) {
		if v = strings.TrimSpace(v); v != "" {
			*dst = v
		}
	}
	switch cfg.Kind {
	case repocfg.KindSFTP:
		s := *cfg.SFTP
		set(&s.Path, e.SFTPPath)
		set(&s.KeyFile, e.SFTPKeyFile)
		if e.SFTPPassword != "" {
			s.Password = e.SFTPPassword
		}
		if kh := strings.TrimSpace(e.SFTPKnownHosts); kh != "" {
			s.KnownHosts = kh + "\n"
		}
		out.SFTP = &s
	case repocfg.KindS3:
		s := *cfg.S3
		set(&s.AccessKeyID, e.S3AccessKey)
		if e.S3Secret != "" {
			s.SecretAccessKey = e.S3Secret
		}
		out.S3 = &s
	case repocfg.KindFilesystem:
		f := *cfg.Filesystem
		set(&f.Path, e.FSPath)
		out.Filesystem = &f
	}
	return out, out.Validate()
}

// dataPath is where the repository's data lives inside its storage, "" for a
// kind with no movable path.
func dataPath(cfg repocfg.Config) string {
	switch cfg.Kind {
	case repocfg.KindSFTP:
		return cfg.SFTP.Path
	case repocfg.KindFilesystem:
		return cfg.Filesystem.Path
	}
	return ""
}

// holdRepository takes every in-flight key that something working on the
// repository holds: each of its jobs (so a scheduled or manual run is refused
// while the hold lasts), its maintenance, its measuring and its wipe. It
// refuses when any is already taken.
func (c *Controller) holdRepository(ctx context.Context, repoID string) (release func(), err error) {
	jobs, err := c.Catalog.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	keys := []string{repoMaintRunKey(repoID), repoMeasureKey(repoID), repoWipeKey(repoID)}
	for _, j := range jobs {
		if j.RepositoryID == repoID {
			keys = append(keys, j.ID)
		}
	}
	var held []string
	release = func() {
		for _, k := range held {
			c.inflight.done(k)
		}
	}
	for _, k := range keys {
		if !c.inflight.start(k) {
			release()
			return nil, fmt.Errorf("a backup or another operation is running on this repository; try again when it has finished: %w", ErrRunning)
		}
		held = append(held, k)
	}
	return release, nil
}

// editAgent is the agent that does the move: one with a job on the repository,
// else one assigned to it.
func (c *Controller) editAgent(ctx context.Context, repoID string) (catalog.Agent, error) {
	jobs, err := c.Catalog.ListJobs(ctx)
	if err != nil {
		return catalog.Agent{}, err
	}
	for _, j := range jobs {
		if j.RepositoryID == repoID {
			return c.Catalog.Agent(ctx, j.AgentID)
		}
	}
	ids, err := c.Catalog.RepositoryAgents(ctx, repoID)
	if err != nil {
		return catalog.Agent{}, err
	}
	for _, id := range ids {
		return c.Catalog.Agent(ctx, id)
	}
	return catalog.Agent{}, errors.New("no agent is assigned to this repository; assign one first")
}

// EditRepository changes a repository's location or credentials. A new path
// moves the data in storage first (SFTP and filesystem only), through an agent,
// and the repository must open at the new place and be the same repository
// before the server saves the change; if it does not, the data is moved back.
// A credentials-only edit is tested before it is saved. Nothing may be running
// on the repository.
func (c *Controller) EditRepository(ctx context.Context, repoID string, in RepoEdit) (warnings []string, err error) {
	repo, err := c.Catalog.Repository(ctx, repoID)
	if err != nil {
		return nil, err
	}
	if repo.InitializedAt.IsZero() {
		return nil, ErrNotInitialized
	}
	newCfg, err := in.apply(repo.Config)
	if err != nil {
		return nil, err
	}
	if reflect.DeepEqual(newCfg, repo.Config) {
		return nil, errors.New("nothing was changed")
	}
	release, err := c.holdRepository(ctx, repoID)
	if err != nil {
		return nil, err
	}
	defer release()
	agent, err := c.editAgent(ctx, repoID)
	if err != nil {
		return nil, err
	}

	upd := repo
	upd.Config = newCfg
	moving := dataPath(repo.Config) != dataPath(newCfg)

	test := func(r catalog.Repository) (*agentpb.TestRepositoryResponse, error) {
		return withAgent(c, agent, initTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.TestRepositoryResponse, error) {
			return cl.TestRepository(ctx, &agentpb.TestRepositoryRequest{Repository: toProtoRepo(r, agent.ID)})
		})
	}
	move := func(from, to catalog.Repository) error {
		cfg, err := json.Marshal(to.Config.ForAgent(agent.ID))
		if err != nil {
			return err
		}
		_, err = withAgent(c, agent, editTimeout, func(ctx context.Context, cl agentpb.AgentClient) (*agentpb.MoveRepositoryResponse, error) {
			return cl.MoveRepository(ctx, &agentpb.MoveRepositoryRequest{Repository: toProtoRepo(from, agent.ID), NewConfigJson: cfg})
		})
		return err
	}
	// moveBack undoes a move whose result could not be used, and says plainly
	// where the data is if even that fails.
	moveBack := func(cause error) error {
		if err := move(upd, repo); err != nil {
			slog.Error("repository move could not be undone", "repo", repo.ID, "now_at", dataPath(newCfg), "was_at", dataPath(repo.Config), "err", err)
			return fmt.Errorf("%w. Moving it back failed too (%v): the data is now at %s, not %s", cause, err, dataPath(newCfg), dataPath(repo.Config))
		}
		return fmt.Errorf("%w. The data was moved back to %s and nothing was saved", cause, dataPath(repo.Config))
	}

	var before string
	if moving {
		resp, err := test(repo)
		if err != nil {
			return nil, fmt.Errorf("%s cannot open the repository at its current path, so it was not moved: %w", agent.Name, unimplementedHint(err))
		}
		if before = resp.GetRepositoryId(); before == "" {
			return nil, fmt.Errorf("%s is too old to move a repository; update it to the server's version", agent.Name)
		}
		if err := move(repo, upd); err != nil {
			return nil, fmt.Errorf("move %s to %s on %s (nothing was changed): %w", dataPath(repo.Config), dataPath(newCfg), agent.Name, unimplementedHint(err))
		}
	}
	resp, err := test(upd)
	if err != nil {
		err = fmt.Errorf("%s cannot open the repository with the new settings: %w", agent.Name, unimplementedHint(err))
		if moving {
			return nil, moveBack(err)
		}
		return nil, err
	}
	if moving && resp.GetRepositoryId() != before {
		return nil, moveBack(errors.New("the repository at the new path is not the one that was moved"))
	}
	if err := c.Catalog.UpdateRepositoryConfig(ctx, repoID, newCfg); err != nil {
		if moving {
			return resp.GetWarnings(), moveBack(fmt.Errorf("save the new settings: %w", err))
		}
		return resp.GetWarnings(), err
	}
	slog.Warn("repository settings changed", "repo", repo.ID, "name", repo.Name, "moved", moving, "from", dataPath(repo.Config), "to", dataPath(newCfg))
	return resp.GetWarnings(), nil
}
