-- Step 31: a completion carries whether it has anything of its key in it.
--
-- A search suggestions job collects what Google offers for a key, and some of
-- it is about something else entirely: a letter typed after the key led Google
-- to another word, "kafka on the shore" for "coffee maker". Such a completion is kept and
-- marked rather than left out, so a reader can see what was set aside and an
-- export can leave it out or keep it as it is asked. Every row written before
-- this step reads as related: nothing was measured about it, and an old file
-- is not to come out shorter for an upgrade.
ALTER TABLE results ADD COLUMN offtopic INTEGER NOT NULL DEFAULT 0;
