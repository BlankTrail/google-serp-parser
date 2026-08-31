-- Which kind of result page a job asks Google for.
--
-- Google answers a phone and a desktop with different pages: different results,
-- in a different order, with different things around them. Which one a run
-- wanted cannot be worked out from the results afterwards and cannot be changed
-- part way through — it is a property of the identities the requests go
-- through, settled when the ports are opened.
--
-- The old column it replaces named a template on the operator's own service.
-- Nothing ever opened a named template, so every non-empty value in it was a
-- name no machine had, and a job carrying one failed on every query it made.
-- Whatever is in there is dropped rather than read: it was a name, and this is
-- a kind.
--
-- Empty is the desktop, which is what every job written before this ran on.
ALTER TABLE jobs ADD COLUMN device TEXT NOT NULL DEFAULT ''
    CHECK (device IN ('', 'desktop', 'mobile'));
