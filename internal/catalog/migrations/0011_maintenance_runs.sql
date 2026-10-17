-- One row per repository maintenance cycle, so how long each took and how long
-- it waited for its agent can be seen. A cycle's length is finished_at -
-- started_at - queued_ms.
CREATE TABLE maintenance_runs (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  agent_id      TEXT NOT NULL,          -- the agent that ran it, or 'server'
  status        TEXT NOT NULL,          -- 'ok' | 'failed'
  started_at    TEXT NOT NULL,
  finished_at   TEXT NOT NULL,
  queued_ms     INTEGER NOT NULL DEFAULT 0,
  error         TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX maintenance_runs_repo_started ON maintenance_runs (repository_id, started_at DESC);
