package server

import (
	"context"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

func TestReplaceFor(t *testing.T) {
	now := time.Now()
	reg := catalog.Registration{Hostname: " Harbinger "}
	a := catalog.Agent{ID: "a", Name: "habringer", Hostname: "harbinger", LastSeen: now.Add(-time.Minute)}
	b := catalog.Agent{ID: "b", Name: "gibson", Hostname: "gibson"}

	if got := replaceFor(reg, []catalog.Agent{a, b}, now); got == nil || got.Agent.ID != "a" || !got.Live {
		t.Fatalf("one match, recently seen: %+v", got)
	}
	old := a
	old.LastSeen = now.Add(-time.Hour)
	if got := replaceFor(reg, []catalog.Agent{old}, now); got == nil || got.Live {
		t.Fatalf("one match, not seen for an hour: %+v", got)
	}
	never := a
	never.LastSeen = time.Time{}
	if got := replaceFor(reg, []catalog.Agent{never}, now); got == nil || got.Live {
		t.Fatalf("one match, never reached: %+v", got)
	}
	if got := replaceFor(reg, []catalog.Agent{b}, now); got != nil {
		t.Fatalf("no match: %+v", got)
	}
	dup := a
	dup.ID = "c"
	if got := replaceFor(reg, []catalog.Agent{a, dup}, now); got != nil {
		t.Fatalf("two matches must suggest nothing: %+v", got)
	}
	if got := replaceFor(catalog.Registration{}, []catalog.Agent{{ID: "x"}}, now); got != nil {
		t.Fatalf("no hostname on either side must not match: %+v", got)
	}
}

func TestApproveFormPreselectsTheAgentWithTheSameHostname(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	ag, err := c.Catalog.Agent(ctx, job.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Catalog.RecordSeen(ctx, ag.ID, time.Now(), "rebuilt-host", "v", nil); err != nil {
		t.Fatal(err)
	}
	register := func(id, host string) {
		t.Helper()
		if _, err := c.Catalog.RegisterAgent(ctx, catalog.Registration{ID: id, Fingerprint: "fp-" + id, AgentCert: []byte{1},
			Hostname: host, Address: "10.0.0.1:9443", Version: "v", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	register("reg-same", "Rebuilt-Host")
	register("reg-other", "somewhere-else")

	srv := httptest.NewServer(NewHandler(c))
	defer srv.Close()
	client := authedClient(t, c, srv)
	page := get(t, client, srv.URL+"/agents", "Waiting for approval")

	same := regexp.MustCompile(`(?s)registrations/reg-same/approve.*?</form>`).FindString(page)
	other := regexp.MustCompile(`(?s)registrations/reg-other/approve.*?</form>`).FindString(page)
	if same == "" || other == "" {
		t.Fatalf("approve forms not found:\n%s", page)
	}
	if !strings.Contains(same, `value="`+ag.ID+`" selected`) || !strings.Contains(page, "Same hostname as "+ag.Name) {
		t.Fatalf("the agent with the same hostname is not preselected:\n%s", same)
	}
	if !strings.Contains(page, "disconnects it at once") {
		t.Fatalf("a recently reached agent is not flagged as connected:\n%s", page)
	}
	if strings.Contains(other, "selected") {
		t.Fatalf("a registration with another hostname preselected something:\n%s", other)
	}
}
