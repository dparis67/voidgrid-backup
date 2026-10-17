package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/robfig/cron/v3"
)

// Dragging a bar on the Schedule page moves its job's time of day. That is only
// well defined for a schedule with one start time per day, so these helpers
// accept just "M H * * D": a plain minute and hour, no day of the month or
// month, and a day of the week that is "*" or a list of numbers. Anything else
// (steps, ranges, several hours, @daily) is edited on the Jobs page.

var (
	ErrNotMovable      = errors.New("this schedule can't be moved by dragging; edit it on the Jobs page")
	ErrScheduleChanged = errors.New("the schedule changed since this page was loaded")
)

const minutesPerDay = 24 * 60

// parseMovable splits a schedule into its time of day (minutes after midnight)
// and the days of the week it is limited to (nil for every day).
func parseMovable(spec string) (tod int, dows []int, ok bool) {
	f := strings.Fields(spec)
	if len(f) != 5 || (f[2] != "*" && f[2] != "?") || f[3] != "*" {
		return 0, nil, false
	}
	m, err1 := strconv.Atoi(f[0])
	h, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil || m < 0 || m > 59 || h < 0 || h > 23 {
		return 0, nil, false
	}
	if f[4] != "*" && f[4] != "?" {
		for _, s := range strings.Split(f[4], ",") {
			d, err := strconv.Atoi(s)
			if err != nil || d < 0 || d > 7 {
				return 0, nil, false
			}
			dows = append(dows, d%7) // cron allows both 0 and 7 for Sunday
		}
	}
	return h*60 + m, dows, true
}

// movable reports whether shiftSchedule can move spec.
func movable(spec string) bool {
	_, _, ok := parseMovable(spec)
	return ok
}

// shiftSchedule moves a movable schedule by minutes (negative is earlier). A
// move across midnight also moves the days of the week it is limited to.
func shiftSchedule(spec string, minutes int) (string, error) {
	tod, dows, ok := parseMovable(spec)
	if !ok {
		return "", ErrNotMovable
	}
	total := tod + minutes
	days := total / minutesPerDay
	if total%minutesPerDay < 0 {
		days-- // floor, not truncate toward zero
	}
	tod = total - days*minutesPerDay

	dow := "*"
	if dows != nil {
		seen := map[int]bool{}
		var moved []int
		for _, d := range dows {
			d = ((d+days)%7 + 7) % 7
			if !seen[d] {
				seen[d] = true
				moved = append(moved, d)
			}
		}
		sort.Ints(moved)
		parts := make([]string, len(moved))
		for i, d := range moved {
			parts[i] = strconv.Itoa(d)
		}
		dow = strings.Join(parts, ",")
	}
	out := fmt.Sprintf("%d %d * * %s", tod%60, tod/60, dow)
	if _, err := cron.ParseStandard(out); err != nil {
		return "", fmt.Errorf("moved schedule %q is invalid: %w", out, err)
	}
	return out, nil
}

// MoveSchedule shifts a job's schedule by minutes, provided it is still the
// schedule the caller saw (was). It returns the new schedule.
func (c *Controller) MoveSchedule(ctx context.Context, id, was string, minutes int) (string, error) {
	if !c.inflight.start(id) {
		return "", ErrRunning
	}
	defer c.inflight.done(id)
	j, err := c.Catalog.Job(ctx, id)
	if err != nil {
		return "", err
	}
	if j.Schedule != was {
		return "", ErrScheduleChanged
	}
	moved, err := shiftSchedule(j.Schedule, minutes)
	if err != nil {
		return "", err
	}
	j.Schedule = moved
	return moved, c.Catalog.UpdateJob(ctx, j)
}
