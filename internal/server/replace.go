package server

import (
	"strings"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/catalog"
)

// agentLiveWithin is how recent a successful check makes an agent count as
// still connected: well over a few missed polls.
const agentLiveWithin = 5 * time.Minute

// replaceCandidate is the approved agent a registration looks like a rebuild
// of, and whether that agent is still answering.
type replaceCandidate struct {
	Agent catalog.Agent
	Live  bool
}

// replaceFor returns the one approved agent whose hostname is the same as the
// registration's (ignoring case and spaces), or nil when there is none or
// several. It only suggests: approving as a replacement is always the
// operator's choice, because it hands that agent's jobs, repositories and
// maintenance to the registering host.
func replaceFor(reg catalog.Registration, agents []catalog.Agent, now time.Time) *replaceCandidate {
	host := strings.ToLower(strings.TrimSpace(reg.Hostname))
	if host == "" {
		return nil
	}
	var found *replaceCandidate
	for _, a := range agents {
		if strings.ToLower(strings.TrimSpace(a.Hostname)) != host {
			continue
		}
		if found != nil {
			return nil
		}
		found = &replaceCandidate{Agent: a, Live: !a.LastSeen.IsZero() && now.Sub(a.LastSeen) < agentLiveWithin}
	}
	return found
}
