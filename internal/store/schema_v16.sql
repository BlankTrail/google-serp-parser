-- What a profile's ports are made of, beyond where they go out.
--
-- Three settings the proxy takes when a port is opened, and which until now
-- this program never offered: the DNS a port resolves through, whether the
-- challenge solver is on it, and whether it may re-originate over HTTP/3.
--
-- The defaults are what every port already had, so nothing about a profile
-- written before this column existed changes: an empty vdns_mode is the
-- proxy's own default for it — on where the port has a tunnel, off where it
-- does not — the solver is on, and HTTP/3 is off.
ALTER TABLE proxy_profiles ADD COLUMN vdns_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE proxy_profiles ADD COLUMN js_solver INTEGER NOT NULL DEFAULT 1;
ALTER TABLE proxy_profiles ADD COLUMN http3 INTEGER NOT NULL DEFAULT 0;
