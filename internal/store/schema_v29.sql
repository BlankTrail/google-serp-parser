-- Step 29: a job can be a completions job, and says how it generates.
--
-- 'suggest' asks Google's completion box about each key instead of searching
-- it: the key typed again and again with one more letter around it, the way the
-- operator's link generator typed it, and every completion that comes back kept.
-- The kind is a CHECK on the column, and SQLite can widen a column but not a
-- constraint, so the table is built again under the new rule and every row
-- copied across — as step 5 did when the target arrived. Foreign keys are held
-- off by the upgrade while it runs; see Store.upgrade.
--
-- suggest_multiword is the generator's Multiword: a letter put between each two
-- words of a key as well. suggest_limit caps how many questions one key may
-- cost, nought for none — the generator had no cap. Both are nought on every job
-- that is not a completions job, and on every job written before this step.
CREATE TABLE jobs_with_completions (
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
                CHECK (unique_by IN ('', 'url', 'host')),
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
INSERT INTO jobs_with_completions(id, name, created_at, finished_at, pages, spec_name,
                                  country, language, kind, unique_by, plan_ready, dropped,
                                  ports, threads, target, tries, fields, device, cooldown_ms,
                                  profile_id, whole_pool, browser, os, browser_release,
                                  rest_up_to_ms, checks_met)
     SELECT id, name, created_at, finished_at, pages, spec_name,
            country, language, kind, unique_by, plan_ready, dropped,
            ports, threads, target, tries, fields, device, cooldown_ms,
            profile_id, whole_pool, browser, os, browser_release,
            rest_up_to_ms, checks_met
       FROM jobs;
DROP TABLE jobs;
ALTER TABLE jobs_with_completions RENAME TO jobs;
