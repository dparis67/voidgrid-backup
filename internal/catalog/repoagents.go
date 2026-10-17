package catalog

import (
	"context"
	"time"
)

// AssignAgent records that an agent may use a repository. It is idempotent. An
// agent's first repository becomes its default.
func (c *Catalog) AssignAgent(ctx context.Context, repoID, agentID string) error {
	_, err := c.db.ExecContext(ctx, `INSERT OR IGNORE INTO repository_agents (repository_id, agent_id, created_at, is_default)
		VALUES (?, ?, ?, NOT EXISTS (SELECT 1 FROM repository_agents WHERE agent_id = ?))`,
		repoID, agentID, formatTime(time.Now()), agentID)
	return err
}

// SetDefaultRepository makes a repository the one new jobs on the agent start
// with, replacing any other. The agent must be assigned to it.
func (c *Catalog) SetDefaultRepository(ctx context.Context, agentID, repoID string) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE repository_agents SET is_default = (repository_id = ?) WHERE agent_id = ?
		AND EXISTS (SELECT 1 FROM repository_agents WHERE agent_id = ? AND repository_id = ?)`, repoID, agentID, agentID, repoID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// DefaultRepositories maps each agent ID to its default repository ID.
func (c *Catalog) DefaultRepositories(ctx context.Context) (map[string]string, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT agent_id, repository_id FROM repository_agents WHERE is_default = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var a, r string
		if err := rows.Scan(&a, &r); err != nil {
			return nil, err
		}
		out[a] = r
	}
	return out, rows.Err()
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
