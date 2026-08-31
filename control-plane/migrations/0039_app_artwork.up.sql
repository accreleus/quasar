-- UI-P7 — cover artwork service.
--
-- Two crops, not one image scaled (spec §Phase 7 / notes §15): the library tile
-- frame is `aspect-ratio: 16/10` and the hero/detail panels are far wider, so a
-- single asset scaled into both makes the hero read as a stretched thumbnail.
-- `apps.cover_url` (already present, never written until now) is the TILE crop;
-- `apps.hero_url` is the new wide HERO crop. Both are NULL for an app with no
-- artwork, which is exactly today's behaviour — the client renders the gradient
-- tile, which is a deliberate design, not an error state.
ALTER TABLE apps ADD COLUMN hero_url TEXT;

-- app_artwork is the PROVENANCE row: where an app's art came from, which
-- locally-cached blobs back it, and whether an admin has overridden the fuzzy
-- match. apps.cover_url / apps.hero_url are the denormalised render URLs the
-- library reads (so the library query is untouched); this table is what the
-- fetcher and the admin override reason about.
--
-- ON DELETE CASCADE on app_id: artwork is a property OF the app and has no
-- meaning without it. The cached blobs themselves are content-addressed and
-- shared, so they are NOT deleted here — the service's orphan sweep reclaims
-- any blob no row references.
CREATE TABLE app_artwork (
    app_id UUID PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,

    -- 'provider' = matched and fetched from the configured artwork provider.
    -- 'manual'   = an admin picked/uploaded it (an override of a wrong match).
    -- 'none'     = we looked and there is nothing. This is a NEGATIVE CACHE and
    --              a first-class outcome, not an error: desktop apps (Blender,
    --              Firefox) are not in a games database and never will be, so
    --              recording "no art" stops every boot re-querying a third party
    --              for a row that can never match. The app renders the gradient
    --              tile, which is what the mockups already show.
    source TEXT NOT NULL CHECK (source IN ('provider', 'manual', 'none')),

    -- Which provider produced the match ('steamgriddb'), and its opaque id for
    -- the matched game. Empty for 'manual'/'none'.
    provider     TEXT NOT NULL DEFAULT '',
    provider_ref TEXT NOT NULL DEFAULT '',
    -- The provider-side title we matched. Surfaced in the admin UI so an
    -- operator can SEE that "Portal 2" matched "Portal 2: Community Update"
    -- before deciding whether to override it.
    matched_name TEXT NOT NULL DEFAULT '',

    -- Locally cached blob names, content-addressed: '<sha256>.<jpg|png|webp>'.
    -- NEVER a remote URL and never a remote-chosen filename — the name is
    -- derived from the SHA-256 of the bytes we stored plus an extension mapped
    -- from the SNIFFED content type, so a hostile response cannot pick its own
    -- path on disk. Empty = that crop is unavailable (the client falls back).
    tile_asset TEXT NOT NULL DEFAULT '',
    hero_asset TEXT NOT NULL DEFAULT '',

    -- Free-text credit line rendered next to the art when the provider's terms
    -- require attribution.
    attribution TEXT NOT NULL DEFAULT '',

    -- Set by any admin override. The automatic fetcher NEVER touches a locked
    -- row: fuzzy matching is wrong sometimes, and an operator who has fixed a
    -- match must not have that fix silently re-broken by the next sweep.
    locked BOOLEAN NOT NULL DEFAULT false,

    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The fetcher's work query is "apps with no artwork row yet", which is an
-- anti-join against this PK — already indexed. This partial index instead serves
-- the admin/GC direction: enumerate every blob still referenced.
CREATE INDEX idx_app_artwork_tile_asset ON app_artwork (tile_asset) WHERE tile_asset <> '';
CREATE INDEX idx_app_artwork_hero_asset ON app_artwork (hero_asset) WHERE hero_asset <> '';
