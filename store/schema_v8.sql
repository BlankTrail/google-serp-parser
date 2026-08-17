-- SPDX-License-Identifier: MIT

-- Step eight. Like the steps before it, every statement here runs exactly once
-- against a given database, so nothing is written IF NOT EXISTS.

-- What the page carried besides its results.
--
-- Both have been parsed since the first version and neither was ever written
-- down, which made them the two things a reader could see on the page and not
-- in their file.
--
-- They hang on the page rather than on the job because that is what they are:
-- which ads a query drew is a fact about one capture of one page, and the same
-- query ten seconds later drew others — six and then none, measured. A table
-- keyed by the job would say "these are the advertisers on this query", which
-- is a claim this program cannot make.
--
-- They are separate tables and not columns on results because they are not
-- results: an ad has a placement and no rank, a related search is a phrase and
-- nothing else, and folding either into the results table would give every
-- result several columns that are always empty.
CREATE TABLE IF NOT EXISTS ads (
    id        INTEGER PRIMARY KEY,
    page_id   INTEGER NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    position  INTEGER NOT NULL,
    placement TEXT    NOT NULL,
    title     TEXT    NOT NULL DEFAULT '',
    host      TEXT    NOT NULL DEFAULT '',
    url       TEXT    NOT NULL DEFAULT '',
    snippet   TEXT    NOT NULL DEFAULT ''
);

-- The order is kept because the page had one, and a list of suggestions read
-- back in another order is a different suggestion about what Google offers
-- first.
CREATE TABLE IF NOT EXISTS related (
    id       INTEGER PRIMARY KEY,
    page_id  INTEGER NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    query    TEXT    NOT NULL
);
