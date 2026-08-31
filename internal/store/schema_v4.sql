-- SPDX-License-Identifier: MIT

-- Step four. Like the steps before it, every statement here runs exactly once
-- against a given database, so nothing is written IF NOT EXISTS.

-- How many Google identities this job runs on at once, and how many queries it
-- keeps in flight over them.
--
-- They sit on the job rather than in the settings because a pool is now raised
-- for one job and taken down when that job lets go. Two jobs never share
-- personalities, so two jobs never have to agree on how many there are, and the
-- size stopped being a property of the machine the moment it stopped being
-- shared. A history read six months later needs them for the same reason it
-- needs the depth and the country: a night that took twice as long as the night
-- before is explained by these two numbers and by nothing else written down.
--
-- Zero is not a pool of nothing. It is a job that said nothing, and it is what
-- every job written before this step carries, which is why there is no UPDATE
-- here putting a number under them: a job from last week has to go on running,
-- and inventing a size for it would be filing a guess as something the operator
-- asked for. What an unspoken size becomes is decided where pools are raised;
-- see the note on JobSpec.Ports for why it cannot be decided here.
--
-- Below nothing is refused outright. The store reads a negative as nothing said,
-- so this only catches a writer that went around it, and there is no reading
-- under which minus three ports is a run somebody meant.
ALTER TABLE jobs ADD COLUMN ports   INTEGER NOT NULL DEFAULT 0 CHECK (ports   >= 0);
ALTER TABLE jobs ADD COLUMN threads INTEGER NOT NULL DEFAULT 0 CHECK (threads >= 0);
