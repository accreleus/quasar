-- 0036_launch_profiles.up.sql — UI-P4 (stream profiles / launch profiles), the
-- EXPAND half of an expand/contract pair.
-- Spec: docs/design/plans/2026-07-28-phase4-profile-restructure-respec.md
--
-- WHAT THIS DOES. It splits one object into two:
--   * a STREAM PROFILE is now ONE ENCODE RUNG — one codec, one resolution, one
--     frame rate, one bitrate. Not user-facing.
--   * a LAUNCH PROFILE is an ORDERED LIST of rungs, best first. It is what a
--     user picks, what the global default points at, what an app pins.
-- Existing ids are preserved as LAUNCH PROFILE ids (`1080p60` stays `1080p60`
-- and becomes a launch profile); rungs get new ids `<launch-profile-id>-<codec>`.
-- That is what keeps sessions.profile_id, user_device_profile_history.profile_id
-- and host_encoder_certification.profile_id (all un-FK'd TEXT) pointing at
-- something that still exists and still means what it meant, and what keeps the
-- hardcoded `conservativeDefaultID` / `lowerProfileRung` id lists correct.
--
-- THIS MIGRATION IS ADDITIVE. It does NOT drop `stream_profiles.codecs`, does
-- NOT delete the legacy (non-rung) stream_profiles rows, does NOT make
-- `stream_profiles.codec` NOT NULL, and does NOT populate
-- `stream_profile_policy.global_default_profile_id`. All of that belongs to a
-- separate CONTRACT migration, after a Tower soak. The split is the cheapest
-- risk reduction on this phase: a code-level revert (redeploy the previous
-- control-plane build) still finds its data, because the legacy rows and the
-- `codecs` column are exactly what the old binary reads.
--
-- ============================ TWO HONESTY CLAUSES ============================
--
--   1. Any admin write made after this migration was applied is LOST by the down
--      migration. The down path restores the pre-migration snapshot verbatim. If
--      an operator edited a stream profile, the policy, or an app's profile
--      assignment while 0036 was applied, that edit does not survive a rollback.
--
--   2. A launch profile created after this migration was applied is DROPPED, not
--      collapsed. It has no pre-migration counterpart and the fan-out cannot be
--      inverted. The down migration emits a RAISE NOTICE naming every such launch
--      profile before dropping it.
--
-- ─────────────────── EDITED IN PLACE AFTER FIRST DEPLOYMENT ──────────────────
--
-- The fan-out rule below was CORRECTED after this file had already been applied
-- to at least one deployment (Tower, `schema_migrations` = 36). golang-migrate
-- never re-runs an applied version, so on those databases the rule that actually
-- ran is the PRE-correction one. Editing an applied migration in place is only
-- ever defensible when the correction is a provable no-op for the data the
-- pre-correction version produced, so here is the proof rather than the promise.
--
-- WHAT CHANGED: the `elif h264 not in rungs: rungs := rungs || [h264]` clause.
-- Before the correction only a launchable list that was EMPTY after filtering
-- got a synthesised h264 floor; a list that was NON-EMPTY but held no launchable
-- h264 (e.g. `[{h264,unsupported},{av1,launchable}]`) fanned out to an
-- h264-less chain. Such a chain is unusable AND unrepairable: resolveRung's
-- unconditional floor degenerates to rungs[len-1] and dispatches AV1 to a host
-- that may not encode it, while write validation's h264 floor rule 400s every
-- PATCH, so the API can never fix it.
--
-- WHY IT IS A NO-OP FOR ALREADY-MIGRATED DATA: the corrected clause fires ONLY
-- for that shape. For every input the pre-correction version handled, the
-- filtered list either was empty (clause 1, unchanged) or already contained
-- h264 (clause 2 does not fire), so both versions emit byte-identical rungs.
-- Verified read-only against Tower's live database on 2026-07-28: all 8 launch
-- profiles carry exactly one h264 rung, none is rung-less, and the only
-- multi-rung chain (1080p60) is `av1 -> hevc -> h264` with the floor LAST, i.e.
-- exactly what the corrected rule produces. The corrective case is empty there.
--
-- CONSEQUENCE: fresh databases get the corrected rule; already-migrated ones
-- need no repair and therefore get no corrective migration. If a future
-- deployment is ever found with a chain lacking an h264 rung, that database
-- predates this note and needs a corrective migration — the check is
--   SELECT lp.id FROM launch_profiles lp WHERE NOT EXISTS (
--       SELECT 1 FROM launch_profile_rungs r
--       JOIN stream_profiles sp ON sp.id = r.stream_profile_id
--       WHERE r.launch_profile_id = lp.id AND sp.codec = 'h264');
--
-- ============================================================================
--
-- WHY DOWN IS A SNAPSHOT RESTORE AND NOT A COMPUTED COLLAPSE. The fan-out is
-- lossy in two directions: a single h264 rung cannot be distinguished from
-- "`codecs` was NULL" versus "`codecs` was the default list explicitly stored",
-- and `future`/`unsupported` entries leave no trace in the rungs at all. So the
-- up migration's FIRST act is to snapshot every affected table, and the down
-- migration refills from those snapshots.
BEGIN;

-- ── (0) Snapshot, before anything else. See the honesty clauses above. ───────
CREATE TABLE _backup_0036_stream_profiles           AS TABLE stream_profiles;
CREATE TABLE _backup_0036_stream_profile_policy     AS TABLE stream_profile_policy;
CREATE TABLE _backup_0036_user_profile_preferences  AS TABLE user_profile_preferences;
CREATE TABLE _backup_0036_apps_profile              AS
    SELECT id, default_profile_id, profile_policy FROM apps;

-- ── (1) The `custom` gate. ──────────────────────────────────────────────────
-- profile_policy = 'custom' is retired by UI-P4, and it CANNOT be converted to
-- another policy behaviour-neutrally: a `custom` app today resolves no profile
-- and lands on the legacy tier path, where the effective settings are
-- min(tier from the user's probe, the app defaults) — NOT simply the app
-- defaults. Converting to `force` would hand a low-bandwidth user the app's full
-- defaults; converting to `inherit` would hand them the global recommendation.
-- Both are silent quality changes on someone's library. So this migration does
-- not guess: it refuses, names the offending apps, and lets the operator convert
-- them deliberately. Migrations run in a transaction, so nothing is applied.
DO $$
DECLARE offenders TEXT;
BEGIN
    SELECT string_agg(name || ' (' || id::text || ')', ', ' ORDER BY name)
      INTO offenders
      FROM apps
     WHERE profile_policy = 'custom';
    IF offenders IS NOT NULL THEN
        RAISE EXCEPTION
            'migration 0036: profile_policy = ''custom'' is retired by UI-P4 and cannot be converted behaviour-neutrally. Point these apps at a launch profile first (inherit/prefer/force), then re-run: %',
            offenders;
    END IF;
END $$;

-- ── (2) stream_profiles.codec — the rung's single codec. ────────────────────
-- CATALOG vocabulary (`hevc`, not the wire `h265`), matching the existing
-- `stream_profiles.codecs`. session/codec.go's catalogToWire stays the one
-- bridge. NULL on every legacy row, non-NULL on every rung row. Made NOT NULL
-- by the later contract migration, once the legacy rows are gone.
ALTER TABLE stream_profiles
    ADD COLUMN codec TEXT NULL CHECK (codec IN ('h264', 'hevc', 'av1'));

-- ── (3) launch_profiles — the ordered chain a user actually picks. ──────────
CREATE TABLE launch_profiles (
    id           TEXT PRIMARY KEY,
    display_name TEXT        NOT NULL,
    description  TEXT        NOT NULL DEFAULT '',
    visibility   TEXT        NOT NULL DEFAULT 'user'
        CHECK (visibility IN ('user', 'debug', 'internal')),
    sort_order   INTEGER     NOT NULL DEFAULT 0,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER launch_profiles_set_updated_at BEFORE UPDATE ON launch_profiles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ── (4) launch_profile_rungs — ORDER IS PREFERENCE. ────────────────────────
-- ON DELETE RESTRICT on the stream-profile side is the BACKSTOP under the
-- application-layer 409 ("delete a rung listed by any launch profile is
-- refused"), never the gate — same posture as apps.runtime_preset_id (0035).
CREATE TABLE launch_profile_rungs (
    launch_profile_id TEXT    NOT NULL REFERENCES launch_profiles(id) ON DELETE CASCADE,
    stream_profile_id TEXT    NOT NULL REFERENCES stream_profiles(id) ON DELETE RESTRICT,
    position          INTEGER NOT NULL CHECK (position > 0),
    PRIMARY KEY (launch_profile_id, position),
    UNIQUE (launch_profile_id, stream_profile_id)
);

-- Postgres does not auto-index the referencing side of a foreign key, and both
-- the "Used by" editor lookup and the delete-in-use 409 are per-rung lookups
-- over this column.
CREATE INDEX launch_profile_rungs_stream_profile_id_idx
    ON launch_profile_rungs (stream_profile_id);

-- ── (5) The fan-out. A RULE, never a row count and never a special case. ────
--
-- for each row R in stream_profiles (EVERY visibility):
--     L := launch profile (id, display_name, visibility, sort_order from R)
--     codecs := R.codecs
--     if codecs IS NULL / not an array / empty / unparseable:
--         codecs := profile.DefaultCodecs() = [h264 launchable, hevc future, av1 future]
--     rungs := [ c for c in codecs if c.status = 'launchable' ]   -- STORED ORDER, verbatim
--     if rungs is empty:      rungs := [ h264 ]                   -- synthesised floor
--     elif h264 not in rungs: rungs := rungs || [ h264 ]          -- APPENDED floor
--     for i, c in enumerate(rungs):
--         S := new stream_profiles row, id = R.id || '-' || c, codec = c,
--              visibility = 'internal', EVERY other column copied from R verbatim
--         insert launch_profile_rungs(L.id, S.id, position = i + 1)
--
-- Three parts are load-bearing:
--
--   * `codecs IS NULL` resolves to the IN-CODE DEFAULT, not to "no codecs". The
--     default is [h264 launchable, hevc future, av1 future], and only
--     `launchable` becomes a rung, so a NULL column yields exactly ONE h264
--     rung. That is the state of every row on Tower today (SHIP-DARK), so in
--     practice almost every launch profile comes out single-rung — which is
--     correct: it is what those profiles stream today.
--
--   * STORED ORDER IS PRESERVED VERBATIM, INCLUDING H.264'S POSITION. Today's
--     default list puts h264 FIRST. If an admin has already flipped hevc or av1
--     to launchable, today's resolver still returns h264 because it is first and
--     passes every clamp. Reordering h264 to last here would flip that
--     host+client to AV1 — a behaviour change wearing a floor rule's clothes.
--     The floor is enforced at write time (validation) and at resolve time (the
--     unconditional last-h264-rung fallback), never by reordering here. An
--     h264-not-last chain is surfaced as a RAISE NOTICE so the operator sees it
--     in the deploy log.
--
--   * EVERY CHAIN LEAVES THIS MIGRATION WITH AN H264 RUNG — two clauses, not
--     one. A list with ZERO launchable entries synthesises the floor: such a
--     profile streams h264 today anyway (resolveCodec's guaranteed fallback
--     fires when no candidate survives), and a rung-less launch profile would
--     turn that silent, correct fallback into a launch with nothing to dispatch.
--     A NON-EMPTY list whose launchable entries exclude h264 —
--     [{h264,unsupported},{av1,launchable}] — gets h264 APPENDED for the same
--     reason: without it the chain's only rung is AV1, the resolver's floor
--     degenerates to rungs[len-1] and dispatches AV1 to a host that may not
--     encode it, and write validation's h264 floor rule 400s every PATCH, so the
--     chain can never be repaired through the API. Appending (not prepending)
--     keeps the launchable entries in their stored order and puts the floor
--     exactly where the resolver looks for it.
--
-- No per-codec tuning happens here: every rung inherits its parent's
-- width/height/fps/bitrate/abr_floor/playout0/h264_profile and every eligibility
-- threshold VERBATIM. An AV1 rung at a lower bitrate than its H.264 sibling is
-- the feature, and it is a deliberate operator data change after the soak —
-- doing it here would destroy the behaviour-neutrality diff, the only instrument
-- that can prove this migration safe.
DO $$
DECLARE
    r          stream_profiles%ROWTYPE;
    codec_list TEXT[];
    c          TEXT;
    i          INTEGER;
    rung_id    TEXT;
    h264_pos   INTEGER;
    label      TEXT;
BEGIN
    FOR r IN SELECT * FROM stream_profiles ORDER BY sort_order ASC, id ASC LOOP
        INSERT INTO launch_profiles (id, display_name, description, visibility, sort_order)
        VALUES (r.id, r.display_name, '', r.visibility, r.sort_order);

        IF r.codecs IS NULL
           OR jsonb_typeof(r.codecs) <> 'array'
           OR jsonb_array_length(r.codecs) = 0 THEN
            -- NULL / empty / non-array ⇒ profile.DefaultCodecs(), whose only
            -- launchable entry is h264.
            codec_list := ARRAY['h264'];
        ELSE
            SELECT array_agg(e.value ->> 'codec' ORDER BY e.ordinality)
              INTO codec_list
              FROM jsonb_array_elements(r.codecs) WITH ORDINALITY AS e(value, ordinality)
             WHERE e.value ->> 'status' = 'launchable'
               AND e.value ->> 'codec' IN ('h264', 'hevc', 'av1');
        END IF;

        -- Zero launchable entries ⇒ synthesise the h264 floor (see above).
        IF codec_list IS NULL OR array_length(codec_list, 1) IS NULL THEN
            codec_list := ARRAY['h264'];
            RAISE NOTICE '0036 fan-out: stream profile % had no launchable codec; synthesising the h264 floor rung (it streams h264 today via the resolver fallback).', r.id;
        -- A NON-EMPTY list with no LAUNCHABLE h264 needs the same floor. The
        -- zero-launchable clause above only fires when the filtered list is
        -- EMPTY, so a list like [{h264,unsupported},{av1,launchable}] slipped
        -- through as a single AV1 rung: the resolver's floor then falls through
        -- to rungs[len-1] and dispatches AV1 to a host that may not encode it,
        -- and the chain is permanently un-PATCHable because write validation
        -- 400s on every save. APPEND rather than prepend, so the stored order of
        -- the launchable entries is still preserved verbatim (the whole point of
        -- the previous clause) and the floor lands where the resolver looks for
        -- it — last.
        ELSIF array_position(codec_list, 'h264') IS NULL THEN
            -- The ::TEXT cast is load-bearing: `text[] || unknown` resolves to the
            -- array-array operator and Postgres tries to parse 'h264' as an array
            -- literal ("malformed array literal"). The cast picks array||element.
            codec_list := codec_list || 'h264'::TEXT;
            RAISE NOTICE '0036 fan-out: stream profile % listed launchable codecs but no launchable h264; appending the h264 floor rung (it streams h264 today via the resolver fallback, and the h264 floor rule would otherwise make the chain permanently un-editable).', r.id;
        END IF;

        h264_pos := array_position(codec_list, 'h264');
        IF h264_pos IS NOT NULL AND h264_pos < array_length(codec_list, 1) THEN
            RAISE NOTICE '0036 fan-out: launch profile % lists h264 at position % of % — every rung after it is UNREACHABLE, because h264 passes every clamp. This mirrors the stored codec order verbatim (deliberately: reordering here would be a behaviour change). Reorder it in the admin UI after the soak.',
                r.id, h264_pos, array_length(codec_list, 1);
        END IF;

        i := 0;
        FOREACH c IN ARRAY codec_list LOOP
            i := i + 1;
            rung_id := r.id || '-' || c;
            label := CASE c WHEN 'h264' THEN 'H.264' WHEN 'hevc' THEN 'HEVC' ELSE 'AV1' END;

            INSERT INTO stream_profiles (
                id, display_name, width, height, fps, h264_profile,
                nominal_bitrate_kbps, min_offer_bandwidth_kbps, recommended_offer_bandwidth_kbps,
                headroom_factor, abr_floor_kbps, max_startup_rtt_ms, min_decode_height,
                high_refresh_display, hardware_encoder_required, browser_client, playout0_ms,
                visibility, sort_order, codecs, codec
            ) VALUES (
                rung_id, label || ' · ' || r.display_name,
                r.width, r.height, r.fps, r.h264_profile,
                r.nominal_bitrate_kbps, r.min_offer_bandwidth_kbps, r.recommended_offer_bandwidth_kbps,
                r.headroom_factor, r.abr_floor_kbps, r.max_startup_rtt_ms, r.min_decode_height,
                r.high_refresh_display, r.hardware_encoder_required, r.browser_client, r.playout0_ms,
                -- A rung is NEVER offered standalone, only via the launch profile
                -- that lists it, so its own visibility is 'internal' regardless of
                -- the parent's. `codecs` is left NULL on a rung: `codec` is
                -- authoritative and the legacy column is vestigial until the
                -- contract migration drops it.
                'internal', r.sort_order, NULL, c
            );

            INSERT INTO launch_profile_rungs (launch_profile_id, stream_profile_id, position)
            VALUES (r.id, rung_id, i);
        END LOOP;
    END LOOP;
END $$;

-- ── (6) The three foreign-key repoints. ─────────────────────────────────────
-- THREE, not two. `user_profile_preferences.default_profile_id` is the one the
-- original spec missed, and it is the most dangerous omission in the phase
-- because of HOW it fails: the table has 0 rows on Tower, so the migration
-- succeeds, the tests stay green, the deploy looks clean, and the FIRST user who
-- sets a quality preference against a genuinely new launch profile gets a 500
-- from an FK violation. (For migrated ids it would even work, because the legacy
-- stream_profiles row still exists under this expand migration — so the bug
-- hides until an admin creates a new launch profile.)
--
-- The constraint names are looked up rather than hardcoded: earlier migrations
-- (0007/0014) recreated some of these, so the auto-generated names cannot be
-- assumed. Values are unchanged and still resolve, because existing ids became
-- launch profile ids.
DO $$
DECLARE con RECORD;
BEGIN
    FOR con IN
        SELECT c.conname, c.conrelid::regclass::text AS tbl
          FROM pg_constraint c
          JOIN pg_class ref ON ref.oid = c.confrelid
         WHERE c.contype = 'f'
           AND ref.relname = 'stream_profiles'
           AND c.conrelid::regclass::text IN
               ('stream_profile_policy', 'apps', 'user_profile_preferences')
    LOOP
        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', con.tbl, con.conname);
    END LOOP;
END $$;

ALTER TABLE stream_profile_policy
    ADD CONSTRAINT stream_profile_policy_global_default_profile_id_fkey
    FOREIGN KEY (global_default_profile_id) REFERENCES launch_profiles(id);

ALTER TABLE apps
    ADD CONSTRAINT apps_default_profile_id_fkey
    FOREIGN KEY (default_profile_id) REFERENCES launch_profiles(id);

ALTER TABLE user_profile_preferences
    ADD CONSTRAINT user_profile_preferences_default_profile_id_fkey
    FOREIGN KEY (default_profile_id) REFERENCES launch_profiles(id);

-- NOTE: stream_profile_policy.global_default_profile_id is deliberately NOT
-- given a value. It is NULL on Tower, so ResolveDefaultProfile falls through to
-- the per-user recommendation. Setting it here — even to the "obviously right"
-- 1080p60 — would change the effective resolution of every `inherit` app for
-- every user in one invisible step.

-- ── (7) sessions.stream_profile_id — the RESOLVED RUNG. ────────────────────
-- sessions.profile_id keeps holding the LAUNCH profile id: it answers "what did
-- the user pick". This new column answers "what did they get". Those are two
-- genuinely different questions and they now have two different columns. NULL
-- for every pre-0036 session and for every legacy/console/override launch.
ALTER TABLE sessions
    ADD COLUMN stream_profile_id TEXT NULL REFERENCES stream_profiles(id);

-- ── (8) Narrow the profile_policy CHECK now that `custom` is proven absent. ─
DO $$
DECLARE cn TEXT;
BEGIN
    SELECT conname INTO cn
      FROM pg_constraint
     WHERE conrelid = 'apps'::regclass
       AND contype = 'c'
       AND pg_get_constraintdef(oid) ILIKE '%profile_policy%';
    IF cn IS NOT NULL THEN
        EXECUTE format('ALTER TABLE apps DROP CONSTRAINT %I', cn);
    END IF;
END $$;

ALTER TABLE apps
    ADD CONSTRAINT apps_profile_policy_check
    CHECK (profile_policy IN ('inherit', 'prefer', 'force'));

COMMIT;
