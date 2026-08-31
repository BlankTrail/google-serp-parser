-- SPDX-License-Identifier: MIT

-- Step three. Like the step before it, every statement here runs exactly once
-- against a given database.

-- How many results this job threw away as repeats.
--
-- It is kept rather than worked out, because a dropped result is gone: the
-- filter never writes it, and nothing left in the database says it ever
-- arrived. Counting the rows that were kept answers a different question, and
-- an operator looking at three million results where they expected ten needs to
-- be told that seven million were repeats rather than left to guess.
--
-- It costs one row update per query recorded, and only for a job that dropped
-- something, so a job with no filter pays nothing for the column.
ALTER TABLE jobs ADD COLUMN dropped INTEGER NOT NULL DEFAULT 0;
