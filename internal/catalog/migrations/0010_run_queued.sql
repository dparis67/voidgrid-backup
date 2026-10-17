-- How long a run waited for its agent to finish earlier operations before it
-- started. A run's length is finished_at - started_at - queued_ms. Runs from
-- before this column keep 0, so their length still includes any wait.
ALTER TABLE runs ADD COLUMN queued_ms INTEGER NOT NULL DEFAULT 0;
