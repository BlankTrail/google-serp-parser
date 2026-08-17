-- The gap this job leaves between two requests on one identity.
--
-- It was a setting of the machine, and it is a property of the run: how hard a
-- list may be pushed depends on the list and on what is being asked of it, and
-- one machine runs a careful job and a fast one on the same afternoon. A single
-- machine-wide answer meant changing it for the job in hand changed it for
-- every job after, and nothing on either job's page said so.
--
-- Milliseconds, because a duration in a database has to be a number and the
-- unit has to be written down somewhere. The box a person fills in is in
-- seconds; the conversion happens where the form is read.
--
-- Nought is a job that named none, and what an unnamed gap becomes is decided
-- where the pool is opened — which is the same rule the ports, the threads and
-- the retry limit already follow.
ALTER TABLE jobs ADD COLUMN cooldown_ms INTEGER NOT NULL DEFAULT 0
    CHECK (cooldown_ms >= 0);
