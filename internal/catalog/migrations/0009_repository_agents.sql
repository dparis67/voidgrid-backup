-- Which agents may use a repository. Existing installs get a row for every
-- agent that already has a job on the repository, so nothing stops working.
CREATE TABLE repository_agents (
  repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  agent_id      TEXT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
  created_at    TEXT NOT NULL,
  PRIMARY KEY (repository_id, agent_id)
) STRICT;

INSERT OR IGNORE INTO repository_agents (repository_id, agent_id, created_at)
  SELECT DISTINCT repository_id, agent_id, strftime('%Y-%m-%dT%H:%M:%fZ', 'now') FROM jobs;
