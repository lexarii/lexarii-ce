-- SPDX-License-Identifier: AGPL-3.0-or-later

-- The queue's order is now one table, domain.JobType.Priority: scan 40,
-- extract 30, chunk 20, embed 10, enrich -10. Until now each place that
-- queued work chose its own number - scan 10, chunk and embed 5, enrich 1,
-- extract 0 - which put enrichment ahead of extraction. With enrichment
-- queued as books arrive, a first scan of a large library held every worker
-- at the model while extraction waited behind it.
--
-- Work already waiting is renumbered to match, so the order holds from the
-- moment the new binary starts rather than once the old queue drains.
--
-- No new index: enrichment's cooldown (needsEnrichment) finds a work's enrich
-- jobs by target, which jobs_target_idx (migration 4) already serves, and a
-- work has only a handful of them.

-- +goose Up

UPDATE jobs
   SET priority = CASE type
                      WHEN 'scan'    THEN 40
                      WHEN 'extract' THEN 30
                      WHEN 'chunk'   THEN 20
                      WHEN 'embed'   THEN 10
                      WHEN 'enrich'  THEN -10
                  END
 WHERE status = 'pending';

-- +goose Down

UPDATE jobs
   SET priority = CASE type
                      WHEN 'scan'    THEN 10
                      WHEN 'extract' THEN 0
                      WHEN 'chunk'   THEN 5
                      WHEN 'embed'   THEN 5
                      WHEN 'enrich'  THEN 1
                  END
 WHERE status = 'pending';
