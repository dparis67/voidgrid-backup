package server

import (
	"testing"
	"time"
)

// A cron expression is read in the server's local zone, not in the zone the
// catalog stores times in (UTC): "0 20 * * *" means 20:00 where the server is.
func TestNextRunIsInTheServersZone(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("no time zone database: %v", err)
	}
	old := time.Local
	time.Local = chicago
	defer func() { time.Local = old }()

	utc := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v // UTC, the way the catalog returns it
	}
	for _, c := range []struct {
		name, schedule, last, want string
	}{
		// Last run Oct 14 20:00 CDT; the next 20:00 is Oct 15 20:00 CDT, not 20:00 UTC.
		{"daily in daylight time", "0 20 * * *", "2026-10-15T01:00:03Z", "2026-10-16T01:00:00Z"},
		// Daylight saving ends Nov 1 2026: the next 20:00 after Oct 31 20:00 CDT is
		// 20:00 CST, an hour later in UTC.
		{"across the end of daylight saving", "0 20 * * *", "2026-11-01T01:00:00Z", "2026-11-02T02:00:00Z"},
		// ... and the same evening time after it starts again.
		{"across the start of daylight saving", "0 20 * * *", "2027-03-14T02:00:00Z", "2027-03-15T01:00:00Z"},
		{"a weekday in local time", "30 6 * * 1", "2026-10-16T12:00:00Z", "2026-10-19T11:30:00Z"},
	} {
		got, err := nextRun(c.schedule, utc(c.last))
		if err != nil || !got.Equal(utc(c.want)) {
			t.Errorf("%s: nextRun(%q, %s) = %s, %v; want %s", c.name, c.schedule, c.last, got.UTC().Format(time.RFC3339), err, c.want)
		}
		if got.Location() != chicago {
			t.Errorf("%s: result is in %v, want the local zone", c.name, got.Location())
		}
	}
	if _, err := nextRun("not a schedule", time.Now()); err == nil {
		t.Error("an invalid schedule was accepted")
	}
}
