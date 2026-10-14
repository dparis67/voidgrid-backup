package catalog

import (
	"context"
	"time"
)

// AssignAgent records that an agent may use a repository. It is idempotent.
func (c *Catalog) AssignAgent(ctx context.Context, repoID, agentID string) error {
	_, err := c.db.ExecContext(ctx, `INSERT OR IGNORE INTO repository_agents (repository_id, agent_id, created_at) VALUES (?, ?, ?)`,
		repoID, agentID, formatTime(time.Now()))
	return err
}

// UnassignAgent removes the assignment. It does not look at jobs; the caller
// refuses when the agent still has some on the repository.
func (c *Catalog) UnassignAgent(ctx context.Context, repoID, agentID string) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM repository_agents WHERE repository_id = ? AND agent_id = ?`, repoID, agentID)
	return err
}

// RepositoryAgents returns the IDs of the agents assigned to a repository.
func (c *Catalog) RepositoryAgents(ctx context.Context, repoID string) ([]string, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT agent_id FROM repository_agents WHERE repository_id = ? ORDER BY created_at, agent_id`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
