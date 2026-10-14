package engine

import (
	"context"

	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/maintenance"
)

// UsernameAtHost is the identity this engine has as a Kopia client, which is
// what a repository records as its maintenance owner.
func (e *Engine) UsernameAtHost() string { return Username + "@" + e.hostname }

// OwnerOf returns the identity that is the maintenance owner of an agent
// whose ID is agentID (agents use their ID as the Kopia host).
func OwnerOf(agentID string) string { return Username + "@" + agentID }

// MaintenanceOwner returns the repository's recorded maintenance owner, "" if
// none.
func (e *Engine) MaintenanceOwner(ctx context.Context, r Repo) (string, error) {
	rep, err := e.open(ctx, r)
	if err != nil {
		return "", err
	}
	defer rep.Close(ctx) //nolint:errcheck // read-only use
	p, err := maintenance.GetParams(ctx, rep)
	if err != nil {
		return "", err
	}
	return p.Owner, nil
}

// SetMaintenanceOwner makes owner (a username@host) the maintenance owner,
// replacing whoever was. Other maintenance parameters are kept. The caller
// must make sure no maintenance is running: Kopia has no repository-wide lock,
// so two owners would overlap.
func (e *Engine) SetMaintenanceOwner(ctx context.Context, r Repo, owner string) error {
	rep, err := e.open(ctx, r)
	if err != nil {
		return err
	}
	defer rep.Close(ctx) //nolint:errcheck // the write below reports errors
	p, err := maintenance.GetParams(ctx, rep)
	if err != nil {
		return err
	}
	p.Owner = owner
	return repo.WriteSession(ctx, rep, repo.WriteSessionOptions{Purpose: "voidgrid-backup:set-maintenance-owner"},
		func(ctx context.Context, w repo.RepositoryWriter) error {
			return maintenance.SetParams(ctx, w, p)
		})
}
