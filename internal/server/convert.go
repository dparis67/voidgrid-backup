package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// The one-time "convert schedules from UTC" step on the Jobs page; the logic
// is in shift.go.

type convertPage struct {
	base
	Rows []ScheduleConversion
	Done bool // already applied or hidden
	None bool // nothing to convert: the server's zone is UTC, or no job has a schedule
}

func (u *ui) convertForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := convertPage{base: u.newBase("Convert schedules", "jobs", r)}
	done, err := u.c.SchedulesConverted(ctx)
	if err == nil && !done {
		p.Rows, err = u.c.ConversionPreview(ctx, time.Now())
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p.Done, p.None = done, !done && len(p.Rows) == 0
	u.render(w, http.StatusOK, "convert", p)
}

func (u *ui) convertApply(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	now := time.Now()
	var notice string
	switch r.FormValue("action") {
	case "dismiss":
		if err := u.c.DismissConversion(ctx, now); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		notice = "The conversion offer is hidden."
	case "convert":
		n, skipped, err := u.c.ConvertSchedules(ctx, r.Form["job"], now)
		switch {
		case errors.Is(err, ErrAlreadyConverted):
			notice = err.Error() + "; nothing was changed."
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		default:
			notice = fmt.Sprintf("Converted %d schedule(s) to %s.", n, zoneLabel(now))
			if len(skipped) > 0 {
				notice += " Not converted: " + strings.Join(skipped, "; ") + "."
			}
		}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	redirectNotice(w, r, "/jobs", notice)
}
