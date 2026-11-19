-- 0002_failures.sql: per-row retry state (PLAN.md §8.6/§8.7). A row that
-- fails is retried with backoff (next_attempt_at); after MaxFailures
-- consecutive failures it is parked until its Google event changes again.
-- last_error holds an error class only (no event content).
ALTER TABLE mappings ADD COLUMN failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE mappings ADD COLUMN next_attempt_at TEXT;
ALTER TABLE mappings ADD COLUMN last_error TEXT;
