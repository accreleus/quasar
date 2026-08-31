-- 0037_app_launch_profiles.up.sql — UI-P5 (per-app launchable launch profiles).
--
-- An app may constrain WHICH launch profiles a user can pick from the menu
-- beside Play. Empty set = today's behaviour (any launch profile the device is
-- eligible for); non-empty = INTERSECT with eligibility. The app's own default
-- (apps.default_profile_id, under profile_policy = 'prefer') is IMPLICITLY
-- always included and is deliberately NOT stored here — storing it would create
-- a second copy to keep in sync with the column every time the default changes.
--
-- A JOIN TABLE, NOT A COLUMN. A jsonb/text[] column would carry no referential
-- integrity: an id could name a launch profile that no longer exists, and
-- nothing would notice until a launch silently offered a menu entry that
-- resolves to nothing. Here every entry is a real foreign key.
--
-- CASCADE ON BOTH SIDES, chosen deliberately:
--
--   app_id -> apps(id) ON DELETE CASCADE.
--     The allow-list is a property OF the app and has no meaning once the app is
--     gone. Same posture as user_app_favourites (migration 0034), and the same
--     reason DELETE /v1/apps/{id} can stay a single statement.
--
--   launch_profile_id -> launch_profiles(id) ON DELETE CASCADE.
--     This is the side that had a real choice, so the reasoning is recorded.
--     RESTRICT would have made retiring a launch profile harder the more
--     carefully an operator had curated their apps: DELETE
--     /v1/admin/launch-profiles/{id} already refuses (409) while the profile is
--     an app's DEFAULT, the global default, or a user preference — the three
--     references that would otherwise leave something pointing at nothing. An
--     allow-list entry is not that: it is a RESTRICTION naming a catalogue
--     object, and removing it leaves the app fully functional. So the row goes
--     with the profile.
--     THE COST, stated rather than discovered: deleting the last launch profile
--     in an app's allow-list empties the list, and an empty list means
--     "unrestricted". A delete can therefore WIDEN an app's menu. That is
--     bounded — the widened set is still only what the device is eligible for,
--     which is the pre-UI-P5 behaviour, and this list is stream-quality curation
--     and never an authorization boundary. The admin activity log records the
--     apps affected by such a delete so it is not silent.
--
-- Only meaningful for profile_policy IN ('inherit','prefer'): 'force' pins the
-- app's profile outright, so no allow-list can ever apply. That is mirrored in
-- the write path (which refuses to store a list for a 'force' app and clears any
-- existing one), not by a CHECK here — the rule spans two tables.
--
-- Purely additive: one new table. No existing table, column, type, constraint,
-- default, or the session state machine changes. In particular it does NOT
-- touch the Phase-4 expand/contract state: stream_profiles.codecs and the legacy
-- (non-rung) stream_profiles rows stay exactly where migration 0036 left them,
-- awaiting the separate contract migration.
BEGIN;

CREATE TABLE app_launch_profiles (
    app_id            UUID NOT NULL REFERENCES apps(id)            ON DELETE CASCADE,
    launch_profile_id TEXT NOT NULL REFERENCES launch_profiles(id) ON DELETE CASCADE,
    PRIMARY KEY (app_id, launch_profile_id)
);

-- Postgres indexes the primary key (giving the (app_id, …) leading edge the
-- per-app read needs) but does NOT auto-index the referencing side of a foreign
-- key. Without this, both cascade deletes above have to sequential-scan this
-- table, and so does the "which apps allow-list this launch profile" lookup the
-- admin surface performs before every launch-profile delete.
CREATE INDEX app_launch_profiles_launch_profile_idx ON app_launch_profiles (launch_profile_id);

COMMIT;
