-- SPDX-License-Identifier: MIT

-- Step five. Like the steps before it, every statement here runs exactly once
-- against a given database, so nothing is written IF NOT EXISTS.

-- A third kind of job, and the site the new one is about.
--
-- The kind column arrived with a CHECK naming the two words it then had, and
-- SQLite cannot widen a CHECK in place: there is no ALTER that touches a
-- constraint. So the table is built again with the wider one and the rows are
-- carried across. It is the jobs table alone — one row per run, thousands at
-- the very most — and everything captured hangs off it by id.
--
-- The order below is the whole safety of this step, and it is the order SQLite's
-- own documentation gives for this. The new table is built beside the old one,
-- the rows are copied, the old one is dropped, and only then does the new one
-- take its name. The tables that point at jobs go on saying "jobs" throughout
-- and find the new table under that name at the end.
--
-- The other order — rename the old one aside first — cannot be used, and the
-- reason is worth writing down because it looks tidier: renaming a table
-- rewrites every REFERENCES clause pointing at it, so queries and seen would
-- follow the old table to its new name and then be deleted with it, whatever the
-- foreign key setting is. Measured, not assumed.
--
-- Dropping the old table is what needs foreign keys held off, which the upgrade
-- does for the length of the steps: with them on, the drop deletes the rows
-- first and every ON DELETE CASCADE aimed at jobs takes the history with it.
CREATE TABLE jobs_with_a_target (
    id          INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL,
    created_at  TEXT    NOT NULL,
    finished_at TEXT,
    pages       INTEGER NOT NULL,
    spec_name   TEXT    NOT NULL DEFAULT '',
    country     TEXT    NOT NULL DEFAULT '',
    language    TEXT    NOT NULL DEFAULT '',

    -- 'search' is the parse job, which is what the word has always meant here: a
    -- job that hands Google phrases and writes down the whole of what comes
    -- back. Every job ever recorded was that, whatever the screen called it, and
    -- rewriting the word under them would file a history of runs under a
    -- question none of them asked. So the value stays and only the name in the
    -- program changed; see KindParse.
    --
    -- 'position' is the new one, and it is the reason a target column exists: it
    -- asks where one named site stands for each phrase.
    kind        TEXT    NOT NULL DEFAULT 'search'
                CHECK (kind IN ('search', 'position', 'index')),
    unique_by   TEXT    NOT NULL DEFAULT ''
                CHECK (unique_by IN ('', 'url', 'host')),
    plan_ready  INTEGER NOT NULL DEFAULT 0,
    dropped     INTEGER NOT NULL DEFAULT 0,
    ports       INTEGER NOT NULL DEFAULT 0 CHECK (ports   >= 0),
    threads     INTEGER NOT NULL DEFAULT 0 CHECK (threads >= 0),

    -- The site a position check is about. Empty is every job that is not one,
    -- including every job written before this step, and it is a site nobody
    -- named rather than a site of nothing.
    target      TEXT    NOT NULL DEFAULT '',

    -- A position check with nothing to look for is unsayable rather than merely
    -- discouraged. Run, it would recognise nothing, and answer "not found" about
    -- every phrase in the list — a wrong answer with nothing left in the
    -- database to disbelieve it by.
    CHECK (kind <> 'position' OR target <> '')
);

-- The columns are named rather than left to SELECT *, so a step written later
-- that changes their order cannot silently file the country under the language.
INSERT INTO jobs_with_a_target(id, name, created_at, finished_at, pages, spec_name,
                               country, language, kind, unique_by, plan_ready,
                               dropped, ports, threads)
     SELECT id, name, created_at, finished_at, pages, spec_name,
            country, language, kind, unique_by, plan_ready,
            dropped, ports, threads
       FROM jobs;

DROP TABLE jobs;
ALTER TABLE jobs_with_a_target RENAME TO jobs;
