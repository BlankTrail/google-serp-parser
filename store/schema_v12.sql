-- Addresses this machine has found dead, and when it found them.
--
-- The pool rests an address that fails to carry a request, so it is not offered
-- again for a while. That record lived only in memory, so every restart began
-- by walking back into every address the day before had spent its time learning
-- to avoid — and on a list where most of them are dead, which is what a large
-- cheap list is, that is most of the first hour.
--
-- It is kept beside the history rather than in a file of its own because it is
-- the same kind of thing: what this machine has learned by running. A row here
-- is not a setting and nobody edits it.
--
-- The key is the address as the rotor knows it — scheme and host and port,
-- without the credentials, so the same proxy behind two logins is one address.
-- No credentials are written here.
--
-- since is when the rest began, in RFC 3339. Whether it has run out is decided
-- when it is read, by the length of rest the pool is configured with, so this
-- table holds no opinion about how long a rest is.
CREATE TABLE IF NOT EXISTS rested_upstreams (
    key   TEXT PRIMARY KEY,
    since TEXT NOT NULL
);
