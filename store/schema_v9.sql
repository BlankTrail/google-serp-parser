-- When each query settled.
--
-- The counts already say how many are done; they cannot say how fast, because a
-- count taken twice is only a speed if something remembers when the first count
-- was taken, and nothing between one page and the next does. A moment written
-- down beside each settled query answers it from the history alone: the speed
-- over the last few queries is the span between the oldest and newest of them.
--
-- Empty on every query written before this existed, and on every query that has
-- not settled. Both are the same thing to a reader — no moment to measure from —
-- and neither is a fault.
ALTER TABLE queries ADD COLUMN settled_at TEXT NOT NULL DEFAULT '';

-- Read by exactly one query: the last few settled ones of a job, newest first.
-- Without it that read walks every query the job has, which on a list of a
-- million is a million rows to find twenty.
CREATE INDEX IF NOT EXISTS queries_settled_at
    ON queries(job_id, settled_at DESC);
