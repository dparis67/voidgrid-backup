package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/kopia/kopia/repo/blob"
)

// fakeStore records DeleteBlob calls and how many ran at once.
type fakeStore struct {
	blob.Storage
	mu       sync.Mutex
	gone     map[blob.ID]bool
	running  atomic.Int32
	peak     atomic.Int32
	failOn   blob.ID
	release  chan struct{}
	arrivals atomic.Int32
}

func (f *fakeStore) DeleteBlob(_ context.Context, id blob.ID) error {
	n := f.running.Add(1)
	defer f.running.Add(-1)
	for {
		p := f.peak.Load()
		if n <= p || f.peak.CompareAndSwap(p, n) {
			break
		}
	}
	if f.release != nil && f.arrivals.Add(1) == 4 {
		close(f.release)
	}
	if f.release != nil {
		<-f.release
	}
	if id == f.failOn {
		return errors.New("connection reset")
	}
	f.mu.Lock()
	f.gone[id] = true
	f.mu.Unlock()
	return nil
}

func metas(n int) []blob.Metadata {
	out := make([]blob.Metadata, n)
	for i := range out {
		out[i] = blob.Metadata{BlobID: blob.ID(fmt.Sprintf("p%03d", i)), Length: 10}
	}
	return out
}

func TestDeleteBlobsRunsSeveralAtOnce(t *testing.T) {
	f := &fakeStore{gone: map[blob.ID]bool{}, release: make(chan struct{})}
	var n atomic.Int32
	if err := deleteBlobs(context.Background(), f, metas(40), 4, func(blob.Metadata) { n.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if len(f.gone) != 40 || n.Load() != 40 {
		t.Fatalf("deleted %d, reported %d, want 40", len(f.gone), n.Load())
	}
	if f.peak.Load() != 4 {
		t.Fatalf("peak concurrency = %d, want 4", f.peak.Load())
	}
}

func TestDeleteBlobsStopsAtTheFirstFailure(t *testing.T) {
	f := &fakeStore{gone: map[blob.ID]bool{}, failOn: "p005"}
	err := deleteBlobs(context.Background(), f, metas(500), 2, func(blob.Metadata) {})
	if err == nil || !strings.Contains(err.Error(), "p005") || len(f.gone) >= 499 {
		t.Fatalf("err=%v deleted=%d: want an error and the rest not started", err, len(f.gone))
	}
}
