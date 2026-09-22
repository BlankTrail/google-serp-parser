-- What a session carries beyond its fingerprint and its cookies.
--
-- exit is where the session goes out: "addr:" and the proxy address written
-- whole, login and password included — a provider that sets the exit by the
-- login has one host and port for every exit, and only the whole of it tells
-- two exits apart — or "gw:" and the name of a gateway. Empty is a session whose
-- address has gone, which takes a new one the next time it is used.
--
-- tickets are the TLS session tickets the proxy service holds for the session's
-- hosts, kept exactly as the service writes them out, so a session put back on
-- a port resumes TLS the way a returning visitor does.
--
-- release is the browser release of the fingerprint, so a job that names one is
-- never handed a session of another.
ALTER TABLE sessions ADD COLUMN exit    TEXT    NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN tickets TEXT    NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN release INTEGER NOT NULL DEFAULT 0;
