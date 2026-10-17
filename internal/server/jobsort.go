package server

import (
	"net/url"
	"sort"
	"strings"
)

// jobCols are the sortable columns of the Jobs list, in display order.
var jobCols = []struct{ key, label string }{
	{"name", "Name"}, {"agent", "Agent"}, {"repo", "Repository"}, {"paths", "Paths"},
	{"schedule", "Schedule"}, {"last", "Last backup"}, {"took", "Took"}, {"next", "Next"},
}

func jobPaths(r jobRow) string {
	switch {
	case r.Stack != nil:
		return "stack " + r.Stack.Project
	case r.VM != nil:
		return "VM " + r.VM.Name
	}
	return strings.Join(r.Paths, ", ")
}

// sortJobs orders rows by column key (a key from jobCols); an unknown key keeps
// the order as given. Rows with no value (never run, manual, still running)
// go last in both directions. Returns the key actually used.
func sortJobs(rows []jobRow, key string, desc bool) string {
	var str func(jobRow) (string, bool)
	var num func(jobRow) (int64, bool)
	switch key {
	case "name":
		str = func(r jobRow) (string, bool) { return strings.ToLower(r.Name), true }
	case "agent":
		str = func(r jobRow) (string, bool) { return strings.ToLower(r.Agent), true }
	case "repo":
		str = func(r jobRow) (string, bool) { return strings.ToLower(r.Repo), true }
	case "paths":
		str = func(r jobRow) (string, bool) { return strings.ToLower(jobPaths(r)), true }
	case "schedule":
		str = func(r jobRow) (string, bool) { return r.Schedule, r.Schedule != "" }
	case "last":
		num = func(r jobRow) (int64, bool) {
			if r.Last == nil {
				return 0, false
			}
			return r.Last.StartedAt.UnixNano(), true
		}
	case "took":
		num = func(r jobRow) (int64, bool) {
			if r.Running || r.Last == nil || r.Last.FinishedAt.IsZero() {
				return 0, false
			}
			return int64(r.Last.Took()), true
		}
	case "next":
		num = func(r jobRow) (int64, bool) { return r.Next.UnixNano(), !r.Next.IsZero() }
	default:
		return ""
	}
	sort.SliceStable(rows, func(i, j int) bool {
		var less, eq, okI, okJ bool
		if str != nil {
			var a, b string
			a, okI = str(rows[i])
			b, okJ = str(rows[j])
			less, eq = a < b, a == b
		} else {
			var a, b int64
			a, okI = num(rows[i])
			b, okJ = num(rows[j])
			less, eq = a < b, a == b
		}
		if okI != okJ {
			return okI
		}
		if eq {
			return false
		}
		return less != desc
	})
	return key
}

// sortHead is one column header: the label, the link that sorts by it (a
// second click on the active column reverses it) and an arrow when active.
type sortHead struct{ Label, Href, Mark string }

func sortHeads(cur string, desc bool) []sortHead {
	var out []sortHead
	for _, c := range jobCols {
		dir, mark := "asc", ""
		if c.key == cur {
			if desc {
				mark = " ▼"
			} else {
				mark = " ▲"
				dir = "desc"
			}
		}
		out = append(out, sortHead{c.label, "?" + url.Values{"sort": {c.key}, "dir": {dir}}.Encode(), mark})
	}
	return out
}
