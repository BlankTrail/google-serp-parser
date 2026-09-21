-- The sessions this program searches through.
--
-- A session is a fingerprint, named by a profile the proxy service holds, and
-- the cookies the search engine has handed it. They used to live inside the
-- ports, and went when a port was closed; with keep_sessions off a port keeps
-- nothing, the cookies come back with the answer, and a session becomes this
-- program's to keep — across the ports of one run, and across runs.
--
-- used_at is what the twelve hours an unused session is kept for are counted
-- from, and failures counts the refusals in a row: two, and the session is
-- given up. cookies is the jar written down, every host it touched.
CREATE TABLE sessions (
    id         INTEGER PRIMARY KEY,
    profile    TEXT    NOT NULL,
    browser    TEXT    NOT NULL DEFAULT '',
    os         TEXT    NOT NULL DEFAULT '',
    device     TEXT    NOT NULL DEFAULT '',
    cookies    TEXT    NOT NULL DEFAULT '[]',
    created_at TEXT    NOT NULL,
    used_at    TEXT    NOT NULL,
    failures   INTEGER NOT NULL DEFAULT 0
);

-- A run asks for the sessions of one kind of result page that have been used
-- recently, and the sweep asks for the ones that have not.
CREATE INDEX sessions_by_device_and_use ON sessions(device, used_at);
