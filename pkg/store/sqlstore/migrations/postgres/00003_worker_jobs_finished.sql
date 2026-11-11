-- +goose Up
-- PurgeJobs filters on finished_at alone, and the failure breaker on kind and finished_at; without
-- these two indexes both scan the whole job history, which grows with the retention an administrator
-- chooses.
CREATE INDEX idx_worker_jobs_finished ON worker_jobs (finished_at);
CREATE INDEX idx_worker_jobs_kind_finished ON worker_jobs (kind, finished_at);

-- +goose Down
DROP INDEX idx_worker_jobs_kind_finished;
DROP INDEX idx_worker_jobs_finished;
