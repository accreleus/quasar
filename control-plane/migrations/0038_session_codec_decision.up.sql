-- 0038_session_codec_decision.up.sql — UI-P6 (codec decision surfacing).
--
-- Two additive, nullable columns on `sessions`. Both are OBSERVABILITY: nothing in
-- scheduling, admission, rung resolution or the agent wire reads either one, and a
-- NULL in both is exactly the pre-UI-P6 behaviour.
--
-- codec_decision — the record of HOW the session's rung/codec was resolved: every
--   rung walked in position order, which clamp rejected each one, and whether the
--   dispatched rung won on merit, was forced by an operator override, or is the
--   unconditional h264 floor. rung.go has produced this since UI-P4 and then thrown
--   it away after logging it; this is where it now lands so an operator can answer
--   "why did I get H.264 and not HEVC?" after the fact instead of hunting for a log
--   line on the right replica.
--
--   WHY A jsonb COLUMN AND NOT A TABLE. The obvious alternative is a
--   session_codec_decisions child table, one row per considered rung. It is not
--   worth it here: the decision is written EXACTLY ONCE per session (in the single
--   post-placement UpdateSessionStream that already exists — see rung.go's header),
--   it is never updated, never queried BY its contents, and is only ever read back
--   whole alongside the session row it belongs to. A child table would buy
--   queryability nobody needs and cost a join on the session read path, which every
--   session GET and the admin list share. Contrast migration 0037, which chose a
--   join table precisely because those entries ARE referenced and need foreign-key
--   integrity — this document references rung ids that may legitimately no longer
--   exist, and freezing them as text is the POINT: it records what was true at
--   launch, not what is true now.
--
--   No CHECK on the shape. Postgres would validate structure but not meaning, and a
--   malformed document must never fail the write that carries the resolved stream
--   values — a lost diagnostic is strictly better than a launch that dispatches
--   without persisting its resolution. The Go side marshals from a typed struct
--   (codec_decision.go), which is where the shape is actually guaranteed.
--
-- negotiated_codec — the WIRE codec the browser reports it is actually decoding
--   (getStats() mimeType, normalised at ingest to h264|h265|av1). Posted on the
--   existing telemetry path (POST /v1/sessions/{id}/stats) and stored here rather
--   than in session_metrics because it is a per-SESSION fact, not a time series:
--   the metrics dictionary is numeric by contract (schema.md), and one string that
--   changes at most once per session does not belong in a per-sample series that is
--   pruned on a rolling window. Storing it on the row also means the admin drill-
--   down and the session list get the comparison without a metrics fan-out.
--
--   Deliberately NOT constrained to the sessions.codec CHECK set. It records what
--   the RECEIVER said, including a codec the server never resolves (vp8/vp9) — that
--   disagreement is the single loudest defect signal this column exists to surface,
--   and a CHECK would throw it away at ingest. Length and character set are bounded
--   in Go at ingest (normaliseNegotiatedCodec), because this is untrusted client
--   input.
--
-- Nothing here disturbs the Phase 4 expand/contract state: stream_profiles.codecs
-- and the legacy rows are untouched and still await their own contract migration.
BEGIN;

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS codec_decision jsonb;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS negotiated_codec text;

COMMENT ON COLUMN sessions.codec_decision IS
	'UI-P6: how the rung/codec was resolved (considered rungs + per-rung rejecting clamp + override/floor markers). Observability only; NULL for every pre-UI-P6 session and for any launch that walked no rung chain.';
COMMENT ON COLUMN sessions.negotiated_codec IS
	'UI-P6: the codec the BROWSER reports decoding (normalised getStats mimeType). Compared against sessions.codec to flag a silent fallback or a mis-negotiated m-line; NULL until the client reports one.';

COMMIT;
