-- Step 32: a search suggestion carries how close in meaning it is to its key.
--
-- The meaning filter's model scores every suggestion against its key, from
-- nought to one. NULL is a suggestion nobody measured — collected before the
-- model was downloaded, or before this step — and the export filters those by
-- their words alone. A job's old suggestions are measured later, from its page.
ALTER TABLE results ADD COLUMN similarity REAL;

-- Scoring a job's old suggestions walks the rows with no score and counts them.
-- The index is keyed by page, so that both start from the job: its queries, the
-- pages of those, and then only the unmeasured rows of each page. Search,
-- position and index jobs never measure their results, so their rows stay in
-- this index forever; an index keyed by id alone would make every count read
-- all of those rows of every other job to find the few of this one. Keyed by
-- page, the rows of other jobs are never visited, and the rows of this job that
-- are measured have left the index.
CREATE INDEX results_unscored ON results(page_id) WHERE similarity IS NULL;
