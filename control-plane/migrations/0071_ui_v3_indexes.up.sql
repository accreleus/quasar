-- 0071_ui_v3_indexes.up.sql — indexes for the two UI v3 console aggregates.
-- Purely additive: no column, no constraint, no data change. Both are read-path
-- only; nothing in the launch or scheduling path depends on them.
--
-- (1) AdminApp.sessions_30d aggregates `sessions` over a 30-day window grouped
--     by app_id. (app_id, created_at DESC) serves both halves of that in one
--     index; sessions_user_created_idx (migration 0001) is keyed the wrong way
--     round for it.
CREATE INDEX IF NOT EXISTS sessions_app_created_idx ON sessions (app_id, created_at DESC);

-- (2) AdminUser.active_session_count counts a user's non-terminal sessions. A
--     PARTIAL index, because the overwhelming majority of rows in this table are
--     terminal and never match: the index stays roughly the size of the live
--     fleet rather than of all history. Postgres uses it only when it can prove
--     the query's WHERE implies this predicate, so the state list here and
--     auth.activeSessionPredicateSQL must stay identical — that is what earns the
--     index-only scan, and widening one without the other degrades the count to a
--     scan of all history.
CREATE INDEX IF NOT EXISTS sessions_user_active_idx ON sessions (user_id)
    WHERE state IN ('pending','assigned','starting','running','stopping');
