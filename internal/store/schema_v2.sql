-- SPDX-License-Identifier: MIT

-- Step two. Every statement here runs exactly once against a given database,
-- so nothing is written IF NOT EXISTS: a step that meets its own work already
-- done has been run twice, and that must be an error rather than a shrug.

-- A job either asks Google for phrases and reads back positions, or hands it
-- addresses and reads back whether they are held at all.
ALTER TABLE jobs ADD COLUMN kind TEXT NOT NULL DEFAULT 'search'
    CHECK (kind IN ('search', 'index'));

-- Empty means every result is kept. The two filters drop a repeat of the same
-- address, or a second result from a host already seen.
ALTER TABLE jobs ADD COLUMN unique_by TEXT NOT NULL DEFAULT ''
    CHECK (unique_by IN ('', 'url', 'host'));

-- A list too large for one transaction is written in batches, and this is set
-- after the last row. It keeps what one transaction gave: a plan that broke off
-- part way is visibly unusable rather than quietly short.
ALTER TABLE jobs ADD COLUMN plan_ready INTEGER NOT NULL DEFAULT 0;

-- Jobs written before the flag existed had their whole plan committed at once,
-- so every one of them is whole and says so. Left at the default they would all
-- become unresumable on upgrade.
UPDATE jobs SET plan_ready = 1;

-- What a job has already let through, so a repeat can be dropped as it arrives
-- rather than found afterwards. INSERT OR IGNORE answers, by the row count,
-- whether a result is new; the key is the address or the host, whichever the
-- job asked for.
--
-- A unique index on results cannot do this: results carries no job id — the tie
-- to a job runs through pages and queries — and denormalising one in would put
-- the constraint on every job, including those that asked for no filter, which
-- a partial index cannot fix because its condition may not read another table.
--
-- WITHOUT ROWID makes the table one tree on exactly the pair it is searched by,
-- which at ten million keys is the difference worth having.
CREATE TABLE seen (
    job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    key    TEXT    NOT NULL,
    PRIMARY KEY (job_id, key)
) WITHOUT ROWID;

-- Nothing in this build reads this table. It is written here so that every
-- change the schema is taking claims one version number between them: two
-- builds each calling itself version 2 would leave a user who took both with a
-- database that says it is current while half of it is missing.
CREATE TABLE api_keys (
    id           INTEGER PRIMARY KEY,
    name         TEXT    NOT NULL,
    prefix       TEXT    NOT NULL,
    hash         TEXT    NOT NULL UNIQUE,
    created_at   TEXT    NOT NULL,
    last_used_at TEXT,
    revoked      INTEGER NOT NULL DEFAULT 0
);
