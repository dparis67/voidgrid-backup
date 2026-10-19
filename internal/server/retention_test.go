package server

import (
	"context"
	"strings"
	"testing"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

// A retention count too big for the 32-bit number sent to the agent used to
// wrap: 4294967297 became 1, and retention then deleted every snapshot but one.
func TestRetentionCountsAreBounded(t *testing.T) {
	ctx := context.Background()
	c, job, _ := setupJob(t)
	base := JobInput{Name: "bounded", AgentID: job.AgentID, RepositoryID: job.RepositoryID,
		Paths: []string{t.TempDir()}, Keep: catalog.DefaultRetention}

	for name, keep := range map[string]catalog.Retention{
		"wraps to 1":      {Latest: 4294967297},
		"just over":       {Daily: maxKeep + 1},
		"another field":   {Latest: 1, Annual: 1 << 40},
		"negative":        {Latest: -1},
		"keeps nothing":   {},
		"ints that wrap":  {Weekly: 1 << 33},
		"every field big": {Latest: 1 << 31, Hourly: 1 << 31, Daily: 1 << 31, Weekly: 1 << 31, Monthly: 1 << 31, Annual: 1 << 31},
	} {
		in := base
		in.Keep = keep
		if _, err := c.AddJob(ctx, in); err == nil {
			t.Errorf("%s: a job with retention %+v was created", name, keep)
		}
	}

	in := base
	in.Keep = catalog.Retention{Latest: maxKeep, Hourly: maxKeep}
	if _, err := c.AddJob(ctx, in); err != nil {
		t.Fatalf("the largest allowed counts were refused: %v", err)
	}

	// The message says what is wrong.
	in = base
	in.Name = "message"
	in.Keep = catalog.Retention{Latest: 4294967297}
	if _, err := c.AddJob(ctx, in); err == nil || !strings.Contains(err.Error(), "cannot be more than") {
		t.Fatalf("message: %v", err)
	}
}
