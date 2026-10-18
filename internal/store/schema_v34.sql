-- Step 34: a profile says how many connections to the proxy its ports may hold
-- at once, and how many of those a challenge being solved takes.
--
-- A customer ran twenty threads through BlankTrail and the proxy provider saw
-- sixty connections at the start and a hundred ten minutes in, against a
-- tariff that allowed fewer: the hidden addresses were read beside the
-- searches with nothing holding them to the threads, and the service opens
-- more connections to the proxy than it is handed — retries, connections it
-- keeps, and three to five of the solver's own for every challenge
-- (measured 2026-10-18). Nought is no limit, which is every profile until
-- somebody sets one; five is what a solve was seen to open.
ALTER TABLE proxy_profiles ADD COLUMN conn_limit INTEGER NOT NULL DEFAULT 0 CHECK (conn_limit >= 0);
ALTER TABLE proxy_profiles ADD COLUMN solver_reserve INTEGER NOT NULL DEFAULT 5 CHECK (solver_reserve >= 0);
