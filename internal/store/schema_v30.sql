-- Step 30: a completions job can drop a completion the job has already kept.
--
-- Completions of different keys repeat each other — "coffee maker" and "best
-- coffee maker" are offered many of the same — and the user wants a job's file
-- to carry each once by default. The filter for repeats tells results apart by
-- 'url' or 'host'; a completion has neither, only its text, so the column takes
-- a fourth word, 'text'. The word is a CHECK, which SQLite can widen only by
-- building the table again, as steps 5 and 29 did; every row is copied across
-- and nothing else changes.
CREATE TABLE jobs_with_text_filter (
    id          INTEGER PRIMARY KEY,
    name        TEXT    NOT NULL,
    created_at  TEXT    NOT NULL,
    finished_at TEXT,
    pages       INTEGER NOT NULL,
    spec_name   TEXT    NOT NULL DEFAULT '',
    country     TEXT    NOT NULL DEFAULT '',
    language    TEXT    NOT NULL DEFAULT '',
    kind        TEXT    NOT NULL DEFAULT 'search'
                CHECK (kind IN ('search', 'position', 'index', 'suggest')),
    unique_by   TEXT    NOT NULL DEFAULT ''
                CHECK (unique_by IN ('', 'url', 'host', 'text')),
    plan_ready  INTEGER NOT NULL DEFAULT 0,
    dropped     INTEGER NOT NULL DEFAULT 0,
    ports       INTEGER NOT NULL DEFAULT 0 CHECK (ports   >= 0),
    threads     INTEGER NOT NULL DEFAULT 0 CHECK (threads >= 0),
    target      TEXT    NOT NULL DEFAULT '',
    tries       INTEGER NOT NULL DEFAULT 0 CHECK (tries >= 0),
    fields      TEXT    NOT NULL DEFAULT '',
    device      TEXT    NOT NULL DEFAULT '' CHECK (device IN ('', 'desktop', 'mobile')),
    cooldown_ms INTEGER NOT NULL DEFAULT 0 CHECK (cooldown_ms >= 0),
    profile_id  INTEGER NOT NULL DEFAULT 0,
    whole_pool  INTEGER NOT NULL DEFAULT 0,
    browser     TEXT    NOT NULL DEFAULT '',
    os          TEXT    NOT NULL DEFAULT '',
    browser_release INTEGER NOT NULL DEFAULT 0,
    rest_up_to_ms   INTEGER NOT NULL DEFAULT 0 CHECK (rest_up_to_ms >= 0),
    checks_met      INTEGER NOT NULL DEFAULT 0,
    suggest_multiword INTEGER NOT NULL DEFAULT 0,
    suggest_limit     INTEGER NOT NULL DEFAULT 0 CHECK (suggest_limit >= 0),
    CHECK (kind <> 'position' OR target <> '')
);
INSERT INTO jobs_with_text_filter(id, name, created_at, finished_at, pages, spec_name,
                                  country, language, kind, unique_by, plan_ready, dropped,
                                  ports, threads, target, tries, fields, device, cooldown_ms,
                                  profile_id, whole_pool, browser, os, browser_release,
                                  rest_up_to_ms, checks_met, suggest_multiword, suggest_limit)
     SELECT id, name, created_at, finished_at, pages, spec_name,
            country, language, kind, unique_by, plan_ready, dropped,
            ports, threads, target, tries, fields, device, cooldown_ms,
            profile_id, whole_pool, browser, os, browser_release,
            rest_up_to_ms, checks_met, suggest_multiword, suggest_limit
       FROM jobs;
DROP TABLE jobs;
ALTER TABLE jobs_with_text_filter RENAME TO jobs;
