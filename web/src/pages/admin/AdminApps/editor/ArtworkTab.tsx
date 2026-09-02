// Artwork tab (handoff §A.10, UI-P7). Two crops, the facts about where they
// came from, and the two overrides: pick a different provider match, or supply
// an image yourself. Fuzzy matching is wrong sometimes ("Portal" matches
// "Portal Knights"), which is why both exist.
//
// The provider credential is read-only here: set/replace/clear lives on
// /admin/settings, since an instance-wide credential must be reachable with
// zero apps. This tab keeps only the indicator and the link.

import { useRef, useState } from "react";
import { Link } from "react-router-dom";
import * as adminApi from "../../../../api/admin";
import { ApiError } from "../../../../api/client";
import type { AppArtwork, AppArtworkCandidate, AppArtworkEnvelope, ArtworkCrop } from "../../../../api/types";
import { Button } from "../../../../components/Button";
import { IconSearch } from "../../../../components/icons";
import { ResourceStates } from "../../../../components/ResourceStates";
import { TextField } from "../../../../components/TextField";
import { useToast } from "../../../../components/Toast";
import { useResource } from "../../../../lib/resource/react";
import { appGlyph } from "../../../../lib/appGlyph";
import { AppFrame, Fact, Section } from "./primitives";

/** How a `source` reads to an operator. `none` is a normal outcome, not a fault. */
const SOURCE_LABEL: Record<string, string> = {
  provider: "Matched automatically",
  manual: "Set by an admin",
  none: "No artwork found, because a games database has no entry for it",
};

/** Kinds a games artwork provider will never have an entry for — must mirror
 *  control-plane/internal/artwork/service.go's `artworklessKinds` (spec §4.5.4). */
const ARTWORKLESS_KINDS = new Set(["desktop", "launcher"]);

const ARTWORKLESS_KIND_LABEL: Record<string, string> = {
  desktop: "Desktop",
  launcher: "Launcher",
};

/** How the provider credential's `provider_origin` reads to an operator. */
const PROVIDER_ORIGIN_LABEL: Record<string, string> = {
  database: "set on the Settings page",
  environment: "from this server's environment",
  static: "built into this deployment",
  none: "not configured",
};

interface ArtworkTabProps {
  appId: string;
  appName: string;
  token: string;
  /** Presentation-only (UI-P1). Drives the never-looked-up explanation. */
  kind: string;
}

export function ArtworkTab({ appId, appName, token, kind }: ArtworkTabProps) {
  const toast = useToast();
  const artwork = useResource<AppArtworkEnvelope>(
    {
      label: "artwork",
      fetch: (ctx) => adminApi.getAppArtwork(ctx.token, appId),
    },
    [appId],
  );

  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [candidates, setCandidates] = useState<AppArtworkCandidate[] | null>(null);
  const [query, setQuery] = useState(appName);
  const [tileUrl, setTileUrl] = useState("");
  const [heroUrl, setHeroUrl] = useState("");

  const tileInput = useRef<HTMLInputElement>(null);
  const heroInput = useRef<HTMLInputElement>(null);

  /** Runs a mutating call, folding its envelope back into the resource. */
  const run = async (fn: () => Promise<AppArtworkEnvelope>, ok: string) => {
    setBusy(true);
    try {
      const next = await fn();
      artwork.setData(() => next);
      setActionError(null);
      toast.addToast({ variant: "success", title: ok });
    } catch (e: unknown) {
      const msg = e instanceof ApiError ? e.message : "That did not work.";
      setActionError(msg);
      toast.addToast({ variant: "danger", title: msg });
    } finally {
      setBusy(false);
    }
  };

  const onSearch = async () => {
    setBusy(true);
    try {
      const res = await adminApi.searchAppArtwork(token, appId, query);
      setCandidates(res.candidates);
      setActionError(null);
      if (res.candidates.length === 0) {
        toast.addToast({ variant: "info", title: `No artwork found for "${query}".` });
      }
    } catch (e: unknown) {
      const msg = e instanceof ApiError ? e.message : "Search failed.";
      setActionError(msg);
      toast.addToast({ variant: "danger", title: msg });
    } finally {
      setBusy(false);
    }
  };

  const onUpload = async (crop: ArtworkCrop, file: File | undefined) => {
    if (!file) return;
    await run(
      () => adminApi.uploadAppArtwork(token, appId, crop, file),
      `${crop === "tile" ? "Tile" : "Hero"} artwork uploaded.`,
    );
  };

  const onClear = async () => {
    setBusy(true);
    try {
      await adminApi.clearAppArtwork(token, appId);
      setCandidates(null);
      setActionError(null);
      toast.addToast({
        variant: "success",
        title: "Artwork cleared, so this app shows its gradient tile.",
      });
      artwork.refresh();
    } catch (e: unknown) {
      const msg = e instanceof ApiError ? e.message : "Could not clear artwork.";
      setActionError(msg);
      toast.addToast({ variant: "danger", title: msg });
    } finally {
      setBusy(false);
    }
  };

  const env = artwork.data ?? null;
  const art: AppArtwork | null = env?.artwork ?? null;
  const providerOn = env?.provider_configured ?? false;
  const isArtworkless = ARTWORKLESS_KINDS.has(kind);

  return (
    <>
      <Section
        title="Artwork"
        desc="The library tile and the wider hero banner. Two separate crops, because a tile stretched into the hero frame reads as a blown-up thumbnail."
      >
        <ResourceStates loading={artwork.loading} error={artwork.errorMessage} />
        {actionError && (
          <p className="form-error" role="alert">
            {actionError}
          </p>
        )}

        {!artwork.loading && (
          <>
            <div className="ae-crops">
              <figure className="ae-crop">
                <AppFrame name={appName} url={art?.tile_url} variant="tile" />
                <figcaption>Tile · 2:3</figcaption>
              </figure>
              <figure className="ae-crop">
                <AppFrame name={appName} url={art?.hero_url} variant="hero" />
                <figcaption>Hero · wide</figcaption>
              </figure>
            </div>

            <div className="ae-facts">
              <Fact label="Source">
                {art
                  ? (SOURCE_LABEL[art.source] ?? art.source)
                  : "No artwork, showing the gradient tile"}
              </Fact>
              {art?.matched_name && <Fact label="Matched">“{art.matched_name}”</Fact>}
              <Fact label="Automatic matching">
                {art?.locked ? "Locked, an admin override that is never overwritten" : "Runs on the next sweep"}
              </Fact>
              {art?.attribution && <Fact label="Credit">{art.attribution}</Fact>}
            </div>

            {!providerOn && (
              <div className="note">
                <div>
                  {env?.provider_problem ??
                    "No artwork provider is configured on this deployment, so nothing is fetched automatically and no app details leave this server."}{" "}
                  Upload artwork below, or{" "}
                  <Link to="/admin/settings">set an API key on the Settings page</Link> after
                  reading the provider&rsquo;s terms.
                </div>
              </div>
            )}
            <span className="hint">
              {providerOn ? (
                <>
                  {`An artwork provider key is configured, ${PROVIDER_ORIGIN_LABEL[env?.provider_origin ?? "none"] ?? env?.provider_origin}.`}{" "}
                  <Link to="/admin/settings">Manage provider credentials</Link>
                </>
              ) : (
                <>
                  No artwork provider key is configured.{" "}
                  <Link to="/admin/settings">Set one on the Settings page</Link>
                </>
              )}
            </span>

            {providerOn && isArtworkless && (
              <div className="note">
                <div>
                  This is a <strong>{ARTWORKLESS_KIND_LABEL[kind] ?? "Desktop"}</strong> app, so it
                  is never looked up automatically. A games database will not have an entry for it.
                  Upload artwork or paste an image URL below.
                </div>
              </div>
            )}
          </>
        )}
      </Section>

      <Section
        title="Set artwork"
        desc="Fuzzy matching is wrong sometimes, and a desktop app is not in a games database at all. Both overrides live here."
      >
        {/* The provider half of this section is absent, never present-and-broken,
            when no credential is configured. */}
        {providerOn && (
          <>
            <div className="field" style={{ maxWidth: 560 }}>
              <label className="label" htmlFor="artwork-search">
                Search the artwork provider
              </label>
              <div className="rowflex">
                <input
                  id="artwork-search"
                  className="input"
                  aria-label="Search the artwork provider"
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                />
                <Button onClick={() => void onSearch()} disabled={busy}>
                  <IconSearch width={13} height={13} />
                  Search
                </Button>
                <Button
                  variant="ghost"
                  disabled={busy}
                  onClick={() =>
                    void run(
                      () => adminApi.setAppArtwork(token, appId, { rematch: true }),
                      "Re-matched.",
                    )
                  }
                >
                  Match automatically
                </Button>
              </div>
              <span className="hint">
                Matching is fuzzy. Check the title before accepting a result.
              </span>
            </div>

            {candidates && candidates.length > 0 && (
              <ul className="ae-cands">
                {candidates.map((c) => (
                  <li key={c.ref}>
                    <button
                      type="button"
                      className="ae-cand"
                      disabled={busy}
                      onClick={() =>
                        void run(
                          () => adminApi.setAppArtwork(token, appId, { provider_ref: c.ref }),
                          `Using artwork for “${c.name}”.`,
                        )
                      }
                    >
                      {/* A data: URI, inlined by the control plane (#80): the
                          CSP never allowed a remote image, and the hotlinking
                          rule means it never will. Never stored, never shown
                          to users. */}
                      <i>
                        {c.thumb_url ? (
                          <img src={c.thumb_url} alt="" loading="lazy" />
                        ) : (
                          appGlyph(c.name)
                        )}
                      </i>
                      <b>{c.name}</b>
                    </button>
                  </li>
                ))}
              </ul>
            )}
            {candidates && candidates.length === 0 && (
              <span className="hint">No matches. Upload artwork below instead.</span>
            )}
          </>
        )}

        <div className="rowflex" style={{ flexWrap: "wrap" }}>
          <Button disabled={busy} onClick={() => tileInput.current?.click()}>
            Upload tile image
          </Button>
          <Button disabled={busy} onClick={() => heroInput.current?.click()}>
            Upload hero image
          </Button>
          {art && (
            <Button variant="ghost" disabled={busy} onClick={() => void onClear()}>
              Reset to gradient
            </Button>
          )}
        </div>
        <input
          ref={tileInput}
          type="file"
          accept="image/png,image/jpeg,image/webp"
          hidden
          aria-label="Upload tile image"
          onChange={(e) => {
            void onUpload("tile", e.target.files?.[0]);
            e.target.value = "";
          }}
        />
        <input
          ref={heroInput}
          type="file"
          accept="image/png,image/jpeg,image/webp"
          hidden
          aria-label="Upload hero image"
          onChange={(e) => {
            void onUpload("hero", e.target.files?.[0]);
            e.target.value = "";
          }}
        />

        <div className="grid g2" style={{ maxWidth: 560 }}>
          <TextField
            label="Tile image URL"
            aria-label="Tile image URL"
            value={tileUrl}
            placeholder="https://…"
            onChange={(e) => setTileUrl(e.target.value)}
          />
          <TextField
            label="Hero image URL"
            aria-label="Hero image URL"
            value={heroUrl}
            placeholder="https://…"
            onChange={(e) => setHeroUrl(e.target.value)}
          />
        </div>
        <div className="rowflex">
          <Button
            disabled={busy || (!tileUrl.trim() && !heroUrl.trim())}
            onClick={() =>
              void run(async () => {
                const res = await adminApi.setAppArtwork(token, appId, {
                  ...(tileUrl.trim() ? { tile_url: tileUrl.trim() } : {}),
                  ...(heroUrl.trim() ? { hero_url: heroUrl.trim() } : {}),
                });
                setTileUrl("");
                setHeroUrl("");
                return res;
              }, "Artwork updated.")
            }
          >
            Fetch from URL
          </Button>
          <span className="hint">
            Fetched once and cached here, so an image is never hotlinked. Public http and https
            addresses only.
          </span>
        </div>
      </Section>
    </>
  );
}
