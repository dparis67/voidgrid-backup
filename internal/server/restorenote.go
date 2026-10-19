package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/voidgrid/voidgrid-backup/internal/proto/agentpb"
)

// ownershipNote says who restored files belong to when the process that
// restored them is not allowed to give files to other users (it is not root and
// has no CAP_CHOWN/CAP_FOWNER). The files are complete: this is a fact about the
// restore, not a warning, so it goes in the run's summary and does not make it
// partial. It is "" when ownership was restored.
func ownershipNote(resp *agentpb.RestoreResponse, host string) string {
	if !resp.GetOwnershipSkipped() {
		return ""
	}
	return fmt.Sprintf("complete, files belong to uid:gid %d:%d (%s does not run as root, so the ownership in the backup was not restored)",
		resp.GetAsUid(), resp.GetAsGid(), host)
}

// withOwnershipNote appends the note to a restore's summary.
func withOwnershipNote(summary string, resp *agentpb.RestoreResponse, host string) string {
	if n := ownershipNote(resp, host); n != "" {
		return summary + "; " + n
	}
	return summary
}

// restoreHostNote is shown before a restore on host (an agent ID, or ServerHost)
// when files restored there will not get their original owners, so choosing it
// is an informed choice. "" when ownership will be restored.
func (c *Controller) restoreHostNote(ctx context.Context, host string) string {
	if host == ServerHost {
		if c.Engine == nil {
			return ""
		}
		uid, gid, canChown := c.Engine.RestoreIdentity()
		if canChown {
			return ""
		}
		return fmt.Sprintf("The server does not run as root, so files restored here belong to the server's own user, uid:gid %d:%d. "+
			"The ownership recorded in the backup is not restored. The files themselves are restored in full.", uid, gid)
	}
	ag, err := c.Catalog.Agent(ctx, host)
	if err != nil {
		return ""
	}
	for _, w := range ag.Warnings {
		if strings.HasPrefix(w, "CAP_CHOWN") || strings.HasPrefix(w, "CAP_FOWNER") {
			return fmt.Sprintf("%s does not run as root (or lacks CAP_CHOWN/CAP_FOWNER), so files restored there belong to the agent's own user. "+
				"The ownership recorded in the backup is not restored. The files themselves are restored in full.", ag.Name)
		}
	}
	return ""
}
