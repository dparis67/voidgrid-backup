package catalog

import (
	"context"
	"time"

	"github.com/voidgrid/voidgrid-backup/internal/tidy"
)

// MaintenanceRun is one repository maintenance cycle.
type MaintenanceRun struct {
	ID           int64         `json:"id"`
	RepositoryID string        `json:"repository_id"`
	AgentID      string        `json:"agent_id"` // the agent that ran it, or "server"
	Status       string        `json:"status"`   // "ok" or "failed"
	StartedAt    time.Time     `json:"started_at"`
	FinishedAt   time.Time     `json:"finished_at"`
	Queued       time.Duration `json:"queued"` // waiting for the agent, inside StartedAt..FinishedAt
	Error        string        `json:"error"`
}

// Took is how long the cycle actually worked: from start to finish, less the
// time it waited for its agent.
func (m MaintenanceRun) Took() time.Duration {
	return max(m.FinishedAt.Sub(m.StartedAt)-m.Queued, 0)
}

func (c *Catalog) AddMaintenanceRun(ctx context.Context, m MaintenanceRun) error {
	_, err := c.db.ExecContext(ctx, `INSERT INTO maintenance_runs (repository_id, agent_id, status, started_at, finished_at, queued_ms, error) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		m.RepositoryID, m.AgentID, m.Status, formatTime(m.StartedAt), formatTime(m.FinishedAt), m.Queued.Milliseconds(), m.Error)
	return err
}

// ListMaintenanceRuns returns the repository's most recent cycles, newest first.
func (c *Catalog) ListMaintenanceRuns(ctx context.Context, repoID string, limit int) ([]MaintenanceRun, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT id, repository_id, agent_id, status, started_at, finished_at, queued_ms, error FROM maintenance_runs WHERE repository_id = ? ORDER BY started_at DESC, id DESC LIMIT ?`, repoID, limit)
	if err != nil {
		return nil, err
	}
	defer tidy.CloseWarn("rows", rows)
	var out []MaintenanceRun
	for rows.Next() {
		var m MaintenanceRun
		var started, finished string
		var queuedMs int64
		if err := rows.Scan(&m.ID, &m.RepositoryID, &m.AgentID, &m.Status, &started, &finished, &queuedMs, &m.Error); err != nil {
			return nil, err
		}
		if m.StartedAt, err = time.Parse(time.RFC3339Nano, started); err != nil {
			return nil, err
		}
		if m.FinishedAt, err = time.Parse(time.RFC3339Nano, finished); err != nil {
			return nil, err
		}
		m.Queued = time.Duration(queuedMs) * time.Millisecond
		out = append(out, m)
	}
	return out, rows.Err()
}
