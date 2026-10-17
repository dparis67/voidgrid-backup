package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/docker/dockertest"
	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
)

func TestStackJobEndToEnd(t *testing.T) {
	ctx := context.Background()
	workdir := filepath.Join(t.TempDir(), "notes")
	os.MkdirAll(filepath.Join(workdir, "pgdata"), 0o755)
	os.WriteFile(filepath.Join(workdir, "docker-compose.yaml"), []byte("services: {}"), 0o644)
	os.WriteFile(filepath.Join(workdir, "pgdata", "PG_VERSION"), []byte("16"), 0o644)

	var mu sync.Mutex
	var imported string
	fake := dockertest.New(t,
		dockertest.ComposeContainer("app1", "notes", workdir, "app", "notes:1", "running"),
		dockertest.ComposeContainer("db1", "notes", workdir, "db", "postgres:16", "running",
			dockertest.Bind(filepath.Join(workdir, "pgdata"), "/var/lib/postgresql/data")),
		dockertest.ComposeContainer("tz", "notes", workdir, "clock", "busybox", "running",
			dockertest.Bind("/etc/localtime", "/etc/localtime")),
	)
	fake.Exec = func(_ string, cmd []string, stdin io.Reader, stdout, stderr io.Writer) int {
		script := strings.Join(cmd, " ")
		switch {
		case strings.Contains(script, "pg_dumpall"):
			io.WriteString(stdout, "CREATE TABLE notes();\n")
		case strings.Contains(script, "psql"):
			b, _ := io.ReadAll(stdin)
			mu.Lock()
			imported = string(b)
			mu.Unlock()
		default:
			return 1
		}
		return 0
	}

	a, addr := startAgentWithDocker(t, t.TempDir(), fake.Socket)
	c := newController(t)
	ag, err := enrollTestAgent(t, c, a, "box", addr)
	if err != nil {
		t.Fatal(err)
	}
	repo, _, err := c.AddRepository(ctx, "local", repocfg.Config{Kind: repocfg.KindFilesystem,
		Filesystem: &repocfg.Filesystem{Path: filepath.Join(t.TempDir(), "repo")}}, "pw", ag.ID)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)

	// Discovery page, with defaults: pgdata unticked (dumped), /etc/localtime unticked (system).
	stacks, err := c.Stacks(ctx, ag.ID)
	if err != nil || len(stacks) != 1 {
		t.Fatalf("stacks: %+v %v", stacks, err)
	}
	v := buildStackView(stacks[0])
	for _, m := range v.Mounts {
		if m.Default {
			t.Fatalf("mount %s ticked by default", m.Source)
		}
	}
	if len(v.Dumps) != 1 || v.Dumps[0].Default != "postgres" {
		t.Fatalf("dump defaults: %+v", v.Dumps)
	}
	page := get(t, client, srv.URL+"/agents/"+ag.ID+"/stacks", "Create stack job")
	if !strings.Contains(page, `<details class="fold">`) || !strings.Contains(page, "no job") {
		t.Fatalf("stack card without a job:\n%s", page)
	}

	// Create the job through the form, the way the page submits it.
	form := url.Values{
		"agent": {ag.ID}, "project": {"notes"}, "working_dir": {workdir}, "name": {"notes"},
		"repository": {repo.ID}, "mount": {filepath.Join(workdir, "pgdata"), "/etc/localtime"},
		"dump:db": {"postgres"}, "quiesce": {"pause"}, "schedule": {""},
		"keep_latest": {"3"}, "keep_hourly": {"0"}, "keep_daily": {"7"}, "keep_weekly": {"0"}, "keep_monthly": {"0"}, "keep_annual": {"0"},
	}
	post(t, client, srv.URL+"/jobs/stack", form)
	job, err := c.Catalog.JobByName(ctx, "notes")
	if err != nil || job.Stack == nil || len(job.Stack.Exclude) != 2 || job.Stack.Dumps["db"] != "postgres" {
		t.Fatalf("stack job: %+v %v", job, err)
	}

	// With a job the card says so and holds the edit form instead of the create form.
	page = get(t, client, srv.URL+"/agents/"+ag.ID+"/stacks", "backed up")
	if !strings.Contains(page, `action="/jobs/`+job.ID+`/edit"`) || strings.Contains(page, "Create stack job") {
		t.Fatalf("stack card with a job:\n%s", page)
	}

	// A nested directory for the browse-link check below.
	if err := os.MkdirAll(filepath.Join(workdir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(workdir, "config", "app.toml"), []byte("x"), 0o644)

	run, err := c.RunJob(ctx, job.ID, "manual")
	if err != nil || run.Status != catalog.RunSuccess {
		t.Fatalf("stack backup: %+v %v", run, err)
	}
	if calls := fake.Calls(); !slices.Equal(calls, []string{"pause app1", "pause tz", "unpause tz", "unpause app1"}) {
		t.Fatalf("quiesce: %v", calls)
	}
	snaps, err := c.Snapshots(ctx, job.ID)
	if err != nil || len(snaps) != 1 || snaps[0].Path != "/stacks/notes" {
		t.Fatalf("snapshots: %+v %v", snaps, err)
	}
	body := get(t, client, srv.URL+"/jobs/"+job.ID+"/snapshots/"+snaps[0].ID, "Restore the whole stack")
	if !strings.Contains(body, "Import into running db") {
		t.Fatal("browse page offers no import for the db dump")
	}

	// A link into a nested directory is escaped once: %252F would be decoded
	// to the literal "stack%2Fconfig", which is no entry.
	inner := get(t, client, srv.URL+"/jobs/"+job.ID+"/snapshots/"+snaps[0].ID+"?path=stack", "config")
	if inner = strings.ToLower(inner); !strings.Contains(inner, "?path=stack%2fconfig") || strings.Contains(inner, "%252f") {
		t.Fatalf("browse links are not escaped exactly once:\n%s", inner)
	}

	// In-place restore needs the project name typed in.
	post(t, client, srv.URL+"/jobs/"+job.ID+"/snapshots/"+snaps[0].ID+"/restore-stack", url.Values{"stop": {"on"}})
	if runs, _ := c.Catalog.ListRuns(ctx, job.ID, 10); len(runs) != 1 {
		t.Fatalf("unconfirmed in-place restore ran: %+v", runs)
	}

	os.WriteFile(filepath.Join(workdir, "docker-compose.yaml"), []byte("broken"), 0o644)
	rr, err := c.RestoreStack(ctx, job.ID, "", snaps[0].ID, "", true)
	if err != nil || !restoreOK(rr) {
		t.Fatalf("stack restore: %+v %v", rr, err)
	}
	if b, _ := os.ReadFile(filepath.Join(workdir, "docker-compose.yaml")); string(b) != "services: {}" {
		t.Fatalf("compose file after restore: %q", b)
	}

	// On another agent only a restore into a directory is allowed.
	a2, addr2 := startAgent(t, t.TempDir())
	other, err := enrollTestAgent(t, c, a2, "other", addr2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RestoreStack(ctx, job.ID, other.ID, snaps[0].ID, "", true); !errors.Is(err, ErrNeedsTarget) {
		t.Fatalf("in-place stack restore on another agent: %v", err)
	}
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	if rr, err := c.RestoreStack(ctx, job.ID, other.ID, snaps[0].ID, elsewhere, false); err != nil || !restoreOK(rr) {
		t.Fatalf("stack restore into a directory on another agent: %+v %v", rr, err)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "stack", "docker-compose.yaml")); err != nil {
		t.Fatalf("stack files on the other agent: %v", err)
	}

	// On the server: a directory restore needs no Docker, in place is refused.
	mapRepoToServer(t, c, job.RepositoryID)
	if _, err := c.RestoreStack(ctx, job.ID, ServerHost, snaps[0].ID, "", true); !errors.Is(err, ErrNeedsTarget) {
		t.Fatalf("in-place stack restore on the server: %v", err)
	}
	onServer := filepath.Join(t.TempDir(), "onserver")
	if rr, err := c.RestoreStack(ctx, job.ID, ServerHost, snaps[0].ID, onServer, false); err != nil || !restoreOK(rr) {
		t.Fatalf("stack restore into a directory on the server: %+v %v", rr, err)
	}
	if _, err := os.Stat(filepath.Join(onServer, "stack", "docker-compose.yaml")); err != nil {
		t.Fatalf("stack files on the server: %v", err)
	}

	ir, err := c.ImportDump(ctx, job.ID, snaps[0].ID, "db")
	if err != nil || ir.Status != catalog.RunSuccess {
		t.Fatalf("import: %+v %v", ir, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if imported != "CREATE TABLE notes();\n" {
		t.Fatalf("imported %q", imported)
	}
}

func get(t *testing.T, client *http.Client, u, want string) string {
	t.Helper()
	resp, err := client.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), want) {
		t.Fatalf("GET %s = %d, missing %q:\n%s", u, resp.StatusCode, want, b)
	}
	return string(b)
}

func post(t *testing.T, client *http.Client, u string, form url.Values) {
	t.Helper()
	req, _ := http.NewRequest("POST", u, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	noRedirect := &http.Client{Jar: client.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	time.Sleep(10 * time.Millisecond)
}
