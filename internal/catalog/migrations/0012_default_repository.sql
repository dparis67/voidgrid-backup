-- The repository a new job on an agent starts with. At most one per agent
-- (enforced by the code that sets it). An agent assigned to exactly one
-- repository gets that one.
ALTER TABLE repository_agents ADD COLUMN is_default INTEGER NOT NULL DEFAULT 0;

UPDATE repository_agents SET is_default = 1
  WHERE agent_id IN (SELECT agent_id FROM repository_agents GROUP BY agent_id HAVING COUNT(*) = 1);
