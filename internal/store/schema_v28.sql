-- How many of Google's checks a job has paid for over all its runs: the
-- captchas the challenge solver solved for it.
--
-- It was a figure of the run in hand and went with the run, so two jobs run at
-- different rests could not be compared once they were done — which is the
-- comparison the rest is chosen by: how fast, and at what cost to the solver.
-- Every run adds what it paid for as it lets go. Jobs run before this step read
-- nought: what they cost was never written down.
ALTER TABLE jobs ADD COLUMN checks_met INTEGER NOT NULL DEFAULT 0;
