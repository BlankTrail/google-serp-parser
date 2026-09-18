-- Which identity a job's ports wear.
--
-- Empty and nought are what every job written before this existed carries, and
-- they mean the same thing they mean on the form: nothing named, so the ports
-- are spread over every browser and system this program knows, each at the
-- newest few releases the service holds. A job that names one of them narrows
-- that spread; a job that names all three runs on one identity.
ALTER TABLE jobs ADD COLUMN browser TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN os TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN browser_release INTEGER NOT NULL DEFAULT 0;
