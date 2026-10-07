-- Step 32: a search suggestion carries how close in meaning it is to its key.
--
-- The meaning filter's model scores every suggestion against its key, from
-- nought to one. NULL is a suggestion nobody measured — collected before the
-- model was downloaded, or before this step — and the export filters those by
-- their words alone. A job's old suggestions are measured later, from its page.
ALTER TABLE results ADD COLUMN similarity REAL;
