-- The named sets of exits a job can be run through, and which set each job
-- named.
--
-- Before this, "which proxies does this job use" was not a property of the job
-- at all: there was one source, kept in the settings file, and every job was
-- run through whatever it happened to say at the moment the identities were
-- raised. Two lists meant editing the settings between two jobs and hoping
-- nobody started the wrong one in between, and the history could not say which
-- of them a finished job had run on.
--
-- A profile is a name and everything about the exits behind it. It is a row
-- rather than a file because a job points at one, and a pointer into a file
-- somebody edits by hand is a pointer that goes stale without saying so.
--
-- What is NOT here: the address of the control API and its key. Those are how
-- this machine reaches BlankTrail at all, they are the same for every profile,
-- and a second copy of a key is a second thing to leak. They stay in the
-- settings file.
--
-- location is a path or a URL, whichever kind names. A URL may carry the
-- credentials the provider issued, exactly as it does in the settings file
-- today, so this column is no less secret than that file was and no more.
--
-- gateways is one configuration name to a line. A line rather than a comma
-- because the names come from the subscription and are not ours to constrain:
-- a comma in one of them would silently split it into two names that match
-- nothing, and a newline cannot appear in one at all.
--
-- The two durations are in milliseconds, like every other duration in this
-- database: a duration is a number here and the unit is written down once.
CREATE TABLE IF NOT EXISTS proxy_profiles (
    id                   INTEGER PRIMARY KEY,
    name                 TEXT    NOT NULL,
    kind                 TEXT    NOT NULL DEFAULT '',
    location             TEXT    NOT NULL DEFAULT '',
    refresh_ms           INTEGER NOT NULL DEFAULT 0,
    ban_ms               INTEGER NOT NULL DEFAULT 0,
    threads_per_upstream INTEGER NOT NULL DEFAULT 0,
    renew_ms             INTEGER NOT NULL DEFAULT 0,
    protocol             TEXT    NOT NULL DEFAULT '',
    gateways             TEXT    NOT NULL DEFAULT '',
    is_default           INTEGER NOT NULL DEFAULT 0
);

-- Two profiles called the same thing are two things a reader cannot tell apart
-- on the one screen that lists them, and a job names one of them.
CREATE UNIQUE INDEX IF NOT EXISTS proxy_profiles_name ON proxy_profiles(name);

-- Exactly one profile is the default one: it is what the identities kept warm
-- between jobs are raised on, what the API's own search goes through, and what
-- a job that named no profile runs on. Two of them would make all three of
-- those questions have two answers, so the database refuses rather than leaving
-- the program to pick.
CREATE UNIQUE INDEX IF NOT EXISTS proxy_profiles_one_default
    ON proxy_profiles(is_default) WHERE is_default = 1;

-- Which profile this job runs through.
--
-- Nought is not a missing value and needs no backfill: it means the job named
-- no profile and runs on whichever one is default, which is what every job
-- written before this column did. It is also what a job keeps when the profile
-- it named is deleted — a job pointing at a row that is gone would refuse to
-- start, and a job that runs on the default instead can at least be seen doing
-- it.
ALTER TABLE jobs ADD COLUMN profile_id INTEGER NOT NULL DEFAULT 0;
