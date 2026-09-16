-- Dropping Labels is metadata plus discarding that column's files -- the raw
-- maps it derives from are untouched, so no log data is lost. A queryapi
-- built against this column must be rolled back first or its matchers will
-- fail to resolve.
ALTER TABLE logs DROP COLUMN IF EXISTS Labels
