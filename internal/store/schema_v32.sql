-- Step 32: a search suggestion carries how close in meaning it is to its key.
--
-- The meaning filter's model scores every suggestion against its key, from
-- nought to one. NULL is a suggestion nobody measured — collected before the
-- model was downloaded, or before this step — and the export filters those by
-- their words alone. A job's old suggestions are measured later, from its page.
ALTER TABLE results ADD COLUMN similarity REAL;

-- Scoring a job's old suggestions walks the rows with no score in id order and
-- counts them. Without this index both read the whole results table of a job of
-- half a million rows to find the few that are not measured yet; with it they
-- read only those, and it holds nothing at all once every row is measured.
CREATE INDEX results_unscored ON results(id) WHERE similarity IS NULL;
