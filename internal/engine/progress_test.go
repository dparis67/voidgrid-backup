package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kopia/kopia/snapshot/upload"
)

func TestLogProgressLogsUntilStopped(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)
	defer func(d time.Duration) { progressEvery = d }(progressEvery)
	progressEvery = 5 * time.Millisecond

	p := &upload.CountingUploadProgress{}
	p.EstimatedDataSize(10, 1000)
	p.HashedBytes(250)
	stop := logProgress("/src", p)
	time.Sleep(50 * time.Millisecond)
	stop()

	out := buf.String()
	if !strings.Contains(out, "backup progress") || !strings.Contains(out, "percent=25") {
		t.Fatalf("no progress line with the percentage: %q", out)
	}
	n := len(buf.String())
	time.Sleep(30 * time.Millisecond)
	if len(buf.String()) != n {
		t.Fatal("progress kept logging after stop")
	}
}

func TestProgressIsReportedWhileAnUploadRunsAndNotAfter(t *testing.T) {
	ctx := context.Background()
	e, r, src, _ := removeFixture(t)
	if _, ok := e.Progress(); ok {
		t.Fatal("progress reported with nothing running")
	}
	var during UploadProgress
	var running bool
	e.uploadHook = func() { during, running = e.Progress() }
	if res, err := e.Backup(ctx, r, []string{src}, nil, keepOne); err != nil || res[0].Err != nil {
		t.Fatalf("backup: %+v %v", res, err)
	}
	if !running || during.Path != src || during.Started.IsZero() {
		t.Fatalf("progress during the upload: running=%v %+v", running, during)
	}
	if _, ok := e.Progress(); ok {
		t.Fatal("progress still reported after the upload ended")
	}
}
