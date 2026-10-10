-- Step 33: no profile works through an exit that terminates TLS itself unless
-- somebody turns it on.
--
-- Step 23 added the switch on, for every profile, and every profile still had
-- it on: on the live wingate list ten of fifteen addresses substituted the
-- origin's TLS, and refusing them made a working list look dead. The
-- operator's call of 2026-10-10 is the other way round — such exits give a lot
-- of trouble and Google rarely takes a search from one — so every profile goes
-- to off, and the one that wants them turns the switch back on.
UPDATE proxy_profiles SET allow_mitm = 0;
