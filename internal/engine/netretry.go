package engine

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"syscall"
	"time"
)

const (
	// netRetryBudget is how long in all an unattended operation waits for a
	// repository's network to come back before it fails.
	netRetryBudget = 15 * time.Minute
	// netRetryFirst is the first wait; each later wait doubles, up to
	// netRetryMax.
	netRetryFirst = 15 * time.Second
	netRetryMax   = 5 * time.Minute
)

type retryKey struct{}

// WithNetworkRetry marks ctx for operations nobody is watching (scheduled
// backups, maintenance, checks): opening the repository then waits and tries
// again when the network or DNS fails, instead of failing at once. Calls from
// the UI, such as adding a repository, don't use it, so a typo in a host name
// is reported straight away.
func WithNetworkRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, retryKey{}, true)
}

// retrySleep waits d or until ctx ends. Tests replace it.
var retrySleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryTransient runs fn and, while it fails with a network error that may
// pass, waits 15 s, then twice as long each time (at most 5 min), until 15
// minutes have been spent waiting; then it returns the last error. Any other
// error, or the end of ctx, returns at once.
func retryTransient(ctx context.Context, what string, fn func() error) error {
	var waited time.Duration
	wait := netRetryFirst
	for {
		err := fn()
		if err == nil || !transientNetwork(err) || ctx.Err() != nil {
			return err
		}
		left := netRetryBudget - waited
		if left <= 0 {
			return err
		}
		wait = min(wait, left)
		slog.Warn("network error, trying again", "operation", what, "in", wait.String(), "waited", waited.String(), "err", summarize(err))
		if serr := retrySleep(ctx, wait); serr != nil {
			return err
		}
		waited += wait
		wait = min(wait*2, netRetryMax)
	}
}

// summarize shortens an error for a log line: Kopia's SFTP dial error carries a
// dump of the whole SSH client config.
func summarize(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, "}: "); i >= 0 && strings.Contains(s, "ssh.ClientConfig") {
		s = "unable to dial: " + s[i+3:]
	}
	return s
}

// transientNetwork says whether err is the kind of network failure that a
// moment later may not repeat: DNS, an unreachable network or host, a refused
// or reset connection, or a timeout. A refused login, a wrong key or a missing
// path is not. The text is checked too, because some of Kopia's wrapping hides
// the underlying error.
func transientNetwork(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	for _, e := range []error{syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ETIMEDOUT} {
		if errors.Is(err, e) {
			return true
		}
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	s := err.Error()
	for _, t := range []string{
		"network is unreachable", "no route to host", "no such host", "Temporary failure in name resolution",
		"connection refused", "connection reset", "i/o timeout", "TLS handshake timeout",
	} {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}
