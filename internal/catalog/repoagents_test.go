package catalog

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// An install from before assignments existed gets a row for every agent that
// has a job on a repository, and none for a repository without jobs.
func TestRepositoryAgentsBackfill(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "catalog.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "0009") || strings.HasPrefix(e.Name(), "0012") {
			continue // the one under test, and the one that builds on it
		}
		b, _ := fs.ReadFile(migrations, "migrations/"+e.Name())
		if _, err := db.Exec(string(b)); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		v := strings.TrimLeft(strings.SplitN(e.Name(), "_", 2)[0], "0")
		if _, err := db.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, 'x')`, v); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`INSERT INTO agents (id, name, address, cert_fingerprint, enrolled_at) VALUES ('a1','one','x:1','f','t'), ('a2','two','x:2','f','t')`,
		`INSERT INTO repositories (id, name, config, password, created_at) VALUES ('r1','used','{}','p','t'), ('r2','unused','{}','p','t')`,
		`INSERT INTO jobs (id, name, agent_id, repository_id, paths, keep_latest, keep_hourly, keep_daily, keep_weekly, keep_monthly, keep_annual, created_at)
		 VALUES ('j1','j1','a1','r1','[]',1,1,1,1,1,1,'t'), ('j2','j2','a1','r1','[]',1,1,1,1,1,1,'t'), ('j3','j3','a2','r1','[]',1,1,1,1,1,1,'t')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	c, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, err := c.RepositoryAgents(ctx, "r1")
	if err != nil || len(got) != 2 || !slices.Contains(got, "a1") || !slices.Contains(got, "a2") {
		t.Fatalf("backfilled agents of r1: %v %v", got, err)
	}
	if got, _ := c.RepositoryAgents(ctx, "r2"); len(got) != 0 {
		t.Fatalf("a repository without jobs got assignments: %v", got)
	}
	// Each agent has one repository, so that one is its default.
	if d, err := c.DefaultRepositories(ctx); err != nil || d["a1"] != "r1" || d["a2"] != "r1" {
		t.Fatalf("backfilled defaults: %v %v", d, err)
	}
	if err := c.AssignAgent(ctx, "r2", "a1"); err != nil {
		t.Fatal(err)
	}
	if err := c.AssignAgent(ctx, "r2", "a1"); err != nil {
		t.Fatalf("assigning twice: %v", err)
	}
	// A second repository does not take the default away.
	if d, _ := c.DefaultRepositories(ctx); d["a1"] != "r1" {
		t.Fatalf("a second assignment changed the default: %v", d)
	}
	if err := c.SetDefaultRepository(ctx, "a1", "r2"); err != nil {
		t.Fatal(err)
	}
	if d, _ := c.DefaultRepositories(ctx); d["a1"] != "r2" || d["a2"] != "r1" {
		t.Fatalf("after choosing r2 for a1: %v", d)
	}
	if err := c.SetDefaultRepository(ctx, "a2", "r2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("defaulting a repository the agent isn't assigned to: %v", err)
	}
	if d, _ := c.DefaultRepositories(ctx); d["a2"] != "r1" {
		t.Fatalf("a refused change altered the default: %v", d)
	}
	if err := c.UnassignAgent(ctx, "r2", "a1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.RepositoryAgents(ctx, "r2"); len(got) != 0 {
		t.Fatalf("after unassign: %v", got)
	}
}
