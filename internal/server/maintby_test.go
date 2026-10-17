package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/engine"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

func TestServerMaintainsRepository(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	eng, err := engine.New(t.TempDir(), EngineHost)
	if err != nil {
		t.Fatal(err)
	}
	c.Engine = eng
	repo, _ := c.Catalog.Repository(ctx, job.RepositoryID)
	dir := repo.Config.Filesystem.Path

	if got := c.MaintainedBy(ctx, repo.ID); got != MaintByAgent {
		t.Fatalf("default maintainer = %q, want agent", got)
	}

	// An agent-local path cannot be opened by the server until it is mapped.
	if _, err := c.SetMaintainer(ctx, repo.ID, MaintByServer, "", ""); err == nil || !strings.Contains(err.Error(), "agent host") {
		t.Fatalf("unmapped filesystem repository: %v", err)
	}

	// An empty directory is refused and nothing is created in it.
	empty := t.TempDir()
	if _, err := c.SetMaintainer(ctx, repo.ID, MaintByServer, "", empty); err == nil {
		t.Fatal("an empty directory was accepted as the repository")
	}
	if names, _ := os.ReadDir(empty); len(names) != 0 {
		t.Fatalf("the test wrote into the mapped directory: %v", names)
	}
	if got, _ := c.Catalog.Repository(ctx, repo.ID); got.Config.Filesystem.ServerPath != "" {
		t.Fatal("a failed switch stored the server path")
	}

	// A directory that holds a different repository (same password, so it opens)
	// is refused: the agent reports another repository ID.
	other := filepath.Join(t.TempDir(), "other")
	oc := repocfg.Config{Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: other}}
	if _, _, err := c.AddRepository(ctx, "other", oc, repo.Password, job.AgentID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetMaintainer(ctx, repo.ID, MaintByServer, "", other); err == nil || !strings.Contains(err.Error(), "not the same repository") {
		t.Fatalf("a different repository was accepted: %v", err)
	}
	if got, _ := c.Catalog.Repository(ctx, repo.ID); got.Config.Filesystem.ServerPath != "" || c.MaintainedBy(ctx, repo.ID) != MaintByAgent {
		t.Fatal("a refused switch changed the repository")
	}

	// A running job blocks the switch.
	c.inflight.start(job.ID)
	if _, err := c.SetMaintainer(ctx, repo.ID, MaintByServer, "", dir); !errors.Is(err, ErrRunning) {
		t.Fatalf("with a job running: %v", err)
	}
	c.inflight.done(job.ID)

	before, err := eng.MaintenanceOwner(ctx, engine.Repo{ID: repo.ID, Config: repo.Config, Password: repo.Password})
	if err != nil || before != engine.OwnerOf(job.AgentID) {
		t.Fatalf("owner before = %q, %v; want the agent", before, err)
	}

	if _, err := c.SetMaintainer(ctx, repo.ID, MaintByServer, "", dir); err != nil {
		t.Fatal(err)
	}
	if c.MaintainedBy(ctx, repo.ID) != MaintByServer {
		t.Fatal("setting not recorded")
	}
	got, _ := c.Catalog.Repository(ctx, repo.ID)
	if got.Config.Filesystem.ServerPath != dir {
		t.Fatalf("server path = %q", got.Config.Filesystem.ServerPath)
	}
	if strings.Contains(string(toProtoRepo(got, "").GetConfigJson()), "server_path") {
		t.Fatal("the server-only path was sent to the agent")
	}
	if owner, _ := eng.MaintenanceOwner(ctx, engine.Repo{ID: repo.ID, Config: got.Config.ForAgent(""), Password: repo.Password}); owner != eng.UsernameAtHost() {
		t.Fatalf("owner = %q, want the server", owner)
	}

	m, err := c.MaintainRepository(ctx, repo.ID)
	if err != nil || m.Status != "ok" || m.AgentID != "server" {
		t.Fatalf("server maintenance: %+v %v", m, err)
	}

	// And back to the agent, which needs no access from the server.
	c.Engine = nil
	if _, err := c.SetMaintainer(ctx, repo.ID, MaintByAgent, job.AgentID, ""); err != nil {
		t.Fatal(err)
	}
	if c.MaintainedBy(ctx, repo.ID) != MaintByAgent {
		t.Fatal("not handed back")
	}
	if owner, _ := eng.MaintenanceOwner(ctx, engine.Repo{ID: repo.ID, Config: got.Config.ForAgent(""), Password: repo.Password}); owner != engine.OwnerOf(job.AgentID) {
		t.Fatalf("owner = %q, want the agent", owner)
	}
	if _, err := c.SetMaintainer(ctx, repo.ID, MaintByAgent, "nobody", ""); err == nil {
		t.Fatal("handed to an unknown agent")
	}
}

func TestServerPathStaysOutOfFingerprint(t *testing.T) {
	a := repocfg.Config{Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: "/mnt/r"}}
	b := repocfg.Config{Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: "/mnt/r", ServerPath: "/srv/r"}}
	if a.Fingerprint() != b.Fingerprint() {
		t.Fatal("setting the server path changes what agents key their local state by")
	}
	if err := (repocfg.Config{Kind: repocfg.KindFilesystem, Filesystem: &repocfg.Filesystem{Path: "/mnt/r", ServerPath: "rel"}}).Validate(); err == nil {
		t.Fatal("a relative server path was accepted")
	}
}

func TestApplyMaintainerFromTheAddJobForm(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	if got := c.ApplyMaintainer(ctx, job, ""); got != "" {
		t.Fatalf("no choice must say nothing: %q", got)
	}
	if got := c.ApplyMaintainer(ctx, job, "agent"); !strings.Contains(got, "done by box") {
		t.Fatalf("agent choice: %q", got)
	}
	if got := c.MaintainedBy(ctx, job.RepositoryID); got != MaintByAgent {
		t.Fatalf("maintained by %q, want the agent", got)
	}
	// The server can't reach this filesystem repository, so the choice is refused and nothing changes.
	if got := c.ApplyMaintainer(ctx, job, "server"); !strings.Contains(got, "Maintenance was not changed") {
		t.Fatalf("server choice on an unreachable repository: %q", got)
	}
	if got := c.ApplyMaintainer(ctx, job, "nobody"); !strings.Contains(got, "not changed") {
		t.Fatalf("unknown choice: %q", got)
	}
	if got := c.MaintainedBy(ctx, job.RepositoryID); got != MaintByAgent {
		t.Fatalf("a refused choice changed the maintainer to %q", got)
	}
}
