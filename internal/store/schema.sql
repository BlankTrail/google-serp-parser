-- SPDX-License-Identifier: MIT

-- Step one, and the shape the first release shipped. It is history now: every
-- database in the world has already run it, so changing it changes nothing for
-- anyone and only makes the next step read as if it were building on something
-- else. Later changes go in a file of their own; see steps in store.go.

CREATE TABLE IF NOT EXISTS jobs (
    id          INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL,
    created_at  TEXT    NOT NULL,
    finished_at TEXT,
    pages       INTEGER NOT NULL,
    spec_name   TEXT    NOT NULL DEFAULT '',
    country     TEXT    NOT NULL DEFAULT '',
    language    TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS queries (
    id      INTEGER PRIMARY KEY,
    job_id  INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
    ordinal INTEGER NOT NULL,
    text    TEXT    NOT NULL,
    state   TEXT    NOT NULL DEFAULT 'pending',
    err     TEXT    NOT NULL DEFAULT '',
    UNIQUE (job_id, ordinal)
);

CREATE TABLE IF NOT EXISTS pages (
    id       INTEGER PRIMARY KEY,
    query_id INTEGER NOT NULL REFERENCES queries(id) ON DELETE CASCADE,
    number   INTEGER NOT NULL,
    origin   TEXT    NOT NULL DEFAULT '',
    UNIQUE (query_id, number)
);

CREATE TABLE IF NOT EXISTS results (
    id      INTEGER PRIMARY KEY,
    page_id INTEGER NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    rank    INTEGER NOT NULL,
    title   TEXT    NOT NULL DEFAULT '',
    url     TEXT    NOT NULL DEFAULT '',
    link    TEXT    NOT NULL DEFAULT '',
    host    TEXT    NOT NULL DEFAULT '',
    snippet TEXT    NOT NULL DEFAULT ''
);

-- A rank history is read by host across jobs, in time order, and that is the
-- one query this schema exists to make fast.
CREATE INDEX IF NOT EXISTS results_by_host ON results(host);
CREATE INDEX IF NOT EXISTS queries_by_job  ON queries(job_id, state);
