package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/voidgrid/voidgrid-backup/internal/repocfg"
	"github.com/voidgrid/voidgrid-backup/internal/tidy"
)

func fakeSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	old := retrySleep
	retrySleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	t.Cleanup(func() { retrySleep = old })
	return &slept
}

func TestRetryTransientBacksOffToFifteenMinutesThenFails(t *testing.T) {
	slept := fakeSleep(t)
	calls := 0
	dnsErr := &net.DNSError{Err: "no such host", Name: "box.example", IsNotFound: true}
	err := retryTransient(context.Background(), "test", func() error { calls++; return fmt.Errorf("unable to dial: %w", dnsErr) })
	if !errors.Is(err, dnsErr) {
		t.Fatalf("want the last error, got %v", err)
	}
	want := []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 135 * time.Second}
	if fmt.Sprint(*slept) != fmt.Sprint(want) {
		t.Fatalf("waits %v, want %v", *slept, want)
	}
	var total time.Duration
	for _, d := range *slept {
		total += d
	}
	if total != netRetryBudget || calls != len(want)+1 {
		t.Fatalf("waited %v in %d calls, want %v in %d", total, calls, netRetryBudget, len(want)+1)
	}
}

func TestRetryTransientStopsWhenTheNetworkComesBack(t *testing.T) {
	slept := fakeSleep(t)
	calls := 0
	err := retryTransient(context.Background(), "test", func() error {
		calls++
		if calls < 3 {
			return fmt.Errorf("dial tcp [2001:db8::1]:23: %w", syscall.ENETUNREACH)
		}
		return nil
	})
	if err != nil || calls != 3 || len(*slept) != 2 {
		t.Fatalf("err %v after %d calls, waits %v", err, calls, *slept)
	}
}

func TestRetryTransientDoesNotRetryOtherErrors(t *testing.T) {
	slept := fakeSleep(t)
	for _, e := range []error{errors.New("ssh: handshake failed: unable to authenticate"), errors.New("repository not found"), context.Canceled} {
		calls := 0
		if err := retryTransient(context.Background(), "test", func() error { calls++; return e }); !errors.Is(err, e) || calls != 1 {
			t.Errorf("%v: err %v after %d calls", e, err, calls)
		}
	}
	if len(*slept) != 0 {
		t.Fatalf("waited %v", *slept)
	}
	// The end of the context stops the waiting too.
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	if err := retryTransient(ctx, "test", func() error { calls++; cancel(); return syscall.ECONNREFUSED }); calls != 1 || err == nil {
		t.Fatalf("a cancelled context retried: %d calls, %v", calls, err)
	}
}

func TestTransientNetworkRecognisesKopiasWrappedText(t *testing.T) {
	for _, s := range []string{
		"unable to dial [h:23]: &ssh.ClientConfig{...}: dial tcp [2a01::1]:23: connect: network is unreachable",
		"dial tcp: lookup u1.your-storagebox.de: Temporary failure in name resolution",
		"read tcp 10.0.0.2:5->1.2.3.4:23: i/o timeout",
	} {
		if !transientNetwork(errors.New(s)) {
			t.Errorf("not recognised: %s", s)
		}
	}
}

// newTestEngineRepoUnreachable returns an engine and an SFTP repository on a
// local port nothing listens on, so opening it fails with "connection refused".
func newTestEngineRepoUnreachable(t *testing.T) (*Engine, Repo) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	tidy.CloseWarn("listener", l)
	known := knownhosts.Line([]string{fmt.Sprintf("[127.0.0.1]:%d", port)}, testKey(t)) + "\n"
	return newEngine(t, "host-a"), Repo{ID: "gone", Password: "pw", Config: repocfg.Config{
		Kind: repocfg.KindSFTP,
		SFTP: &repocfg.SFTP{Host: "127.0.0.1", Port: port, User: "u", Path: "/x", Password: "pw", KnownHosts: known},
	}}
}

func TestOpenRetriesOnlyWhenAskedTo(t *testing.T) {
	// A repository on an unreachable address: without the marker one attempt
	// fails; with it, the waits happen (faked) and the last error comes back.
	slept := fakeSleep(t)
	e, r := newTestEngineRepoUnreachable(t)
	if _, err := e.open(context.Background(), r); err == nil {
		t.Fatal("open of an unreachable repository succeeded")
	}
	if len(*slept) != 0 {
		t.Fatalf("an unmarked open waited: %v", *slept)
	}
	if _, err := e.open(WithNetworkRetry(context.Background()), r); err == nil {
		t.Fatal("open of an unreachable repository succeeded")
	}
	if len(*slept) == 0 {
		t.Fatal("a marked open did not wait")
	}
}
