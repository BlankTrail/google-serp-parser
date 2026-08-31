-- SPDX-License-Identifier: MIT

-- Step seven. Like the steps before it, every statement here runs exactly once
-- against a given database, so nothing is written IF NOT EXISTS.

-- What each result of this job keeps, as the names themselves, comma separated.
--
-- The empty string is everything, which is what every job written before this
-- step carries and what somebody who never touched the choice means. Names
-- rather than a bitmask because this column is read by eye when something is
-- wrong with a job, and because adding a part later must not change the meaning
-- of what is already written down.
--
-- It exists for room. A result costs about 247 bytes on average and most of
-- that is the snippet, so a job that wants a list of addresses and nothing else
-- writes a fraction of what it would otherwise — on ten million results that is
-- the difference between one file and several.
ALTER TABLE jobs ADD COLUMN fields TEXT NOT NULL DEFAULT '';

-- The path as the page displayed it: "example.com › docs › thing". The parser
-- has read it since the first version and nothing has ever kept it, which made
-- it the one thing a reader could see on the page and not in their file.
ALTER TABLE results ADD COLUMN display_path TEXT NOT NULL DEFAULT '';
