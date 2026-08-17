-- SPDX-License-Identifier: MIT

-- Step six. Like the steps before it, every statement here runs exactly once
-- against a given database, so nothing is written IF NOT EXISTS.

-- How many identities one query may be taken to before it is written off.
--
-- The machinery has been there since the run layer was built and nothing ever
-- set it: every job ran on the built-in three. Three was chosen as "enough to
-- get past a refusal aimed at the identity, and cheap enough that a query which
-- is genuinely unanswerable does not eat the budget", and the first live run to
-- measure it disagreed — 77% of requests refused, two answers out of ten
-- queries. On a poor list three tries lose the job, and what the operator sees
-- is not "the list is poor" but "the parser does not work".
--
-- It sits on the job because the answer depends on the list, and the list is
-- somebody's, not the machine's: one operator's addresses are fresh this
-- morning and another's have been hammered for a week.
--
-- Zero is a job that said nothing, exactly as with the pool above it, and every
-- job written before this step carries it. What an unspoken limit becomes is
-- decided where the run is set up, not here.
ALTER TABLE jobs ADD COLUMN tries INTEGER NOT NULL DEFAULT 0 CHECK (tries >= 0);
