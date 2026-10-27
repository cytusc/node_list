-- Deploy the matching application version first: the view below hides nodes the
-- old liveness loop would keep publishing, and the backoff in node-liveness is
-- what makes the freshness window reachable.
-- Run with psql -v ON_ERROR_STOP=1 -f deploy/sql/018-publish-freshness.sql.
BEGIN;
SET LOCAL lock_timeout = '10s';
SET LOCAL statement_timeout = '120s';
SELECT pg_advisory_xact_lock(hashtext(current_schema()),hashtext('node-explorer-schema'));

-- 1. Rebuild the publishing views. live_facts now requires a recent
--    connectivity verdict and a confirmed exit, so "available" on the page
--    means "recently reachable and its exit was actually observed".
\ir ../../backend/internal/database/schema.sql

-- 2. Spread the liveness backlog. Every dead address used to return after one
--    interval, so the due queue reached ~18k rows and a full cycle took hours;
--    live nodes were re-checked hours late and the page kept publishing them.
--    Staggering the currently-unavailable rows over four hours lets the queue
--    drain at a rate the workers can sustain. Reachable nodes are untouched:
--    they keep their interval and their page slot.
UPDATE node_explorer_liveness_attempts a
 SET next_at = now() + random() * interval '4 hours'
 WHERE a.next_at <= now()
 AND NOT EXISTS (
  SELECT 1 FROM node_explorer_node_registry n
  WHERE n.identity = a.identity AND n.available
 );

INSERT INTO node_explorer_service_migrations(version) VALUES(18) ON CONFLICT DO NOTHING;
COMMIT;
