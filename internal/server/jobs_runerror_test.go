package server

import (
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

func TestRunErrorNamesADroppedAgent(t *testing.T) {
	got := runError(status.Error(codes.Unavailable, "error reading from server: EOF"))
	if !strings.Contains(got, "agent stopped or lost its connection") || !strings.Contains(got, "EOF") {
		t.Fatalf("unavailable error = %q", got)
	}
	if got := runError(errors.New("disk full")); got != "disk full" {
		t.Fatalf("other errors must pass through, got %q", got)
	}
	if got := runError(status.Error(codes.Internal, "boom")); strings.Contains(got, "restarted") {
		t.Fatalf("only Unavailable is an interruption, got %q", got)
	}
}

func TestSummarizeBackupReportsWhatWasAdded(t *testing.T) {
	var run catalog.Run
	summarizeBackup(&run, []*agentpb.PathResult{{Path: "/a", Bytes: 1 << 30, AddedBytes: 5 << 20}})
	if !strings.Contains(run.Summary, "added to the repository") {
		t.Fatalf("summary = %q", run.Summary)
	}
	var old catalog.Run
	summarizeBackup(&old, []*agentpb.PathResult{{Path: "/a", Bytes: 1 << 30}})
	if strings.Contains(old.Summary, "added to the repository") {
		t.Fatalf("an older agent reports no figure, summary = %q", old.Summary)
	}
}
