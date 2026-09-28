-- Persist whether an owned RUNNING Job crossed the worker's durable
-- execution-start boundary. Existing RUNNING rows predate this marker and
-- already charged their attempt at claim time, so preserve that accounting.
ALTER TABLE analysis_jobs
    ADD COLUMN execution_started BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE analysis_jobs
SET execution_started = TRUE
WHERE status = 'RUNNING';
