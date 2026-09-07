-- Whether this job spends the whole proxy list rather than a fixed number of
-- ports.
--
-- Nought is not a missing value and needs no backfill: it means the job runs on
-- the pool it always ran on — threads × ports per thread, opened before the run
-- and kept — which is what every job written before this column existed did.
ALTER TABLE jobs ADD COLUMN whole_pool INTEGER NOT NULL DEFAULT 0;
