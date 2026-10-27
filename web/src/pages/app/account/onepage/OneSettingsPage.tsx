// The new design's one account page. Every /app/account/* path renders this one
// scrolling page and scrolls to its section, so old links and the rail keep
// working; the top bar's search finds any setting on it. Each section edits in
// place. Overlay, appearance, password, storage and sessions reuse the classic
// pages as-is; stream quality and devices are rebuilt here (a labelled select
// that saves on change, devices grouped by use with named actions).

import { useEffect, useRef, useState, type ReactNode } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import * as authApi from "../../../../api/auth";
import type { Device } from "../../../../api/auth";
import * as libraryApi from "../../../../api/library";
import { getMyStorage } from "../../../../api/storage";
import type { MyStorageItem } from "../../../../api/types";
import { ApiError } from "../../../../api/client";
import { useAuth } from "../../../../auth/context";
import { clearSession } from "../../../../auth/storage";
import { Button } from "../../../../components/Button";
import { Modal } from "../../../../components/Modal";
import { PageHeader } from "../../../../components/PageHeader";
import { SelectField, Switch } from "../../../../components/TextField";
import { useToast } from "../../../../components/Toast";
import { useResource } from "../../../../lib/resource/react";
import { bytes } from "../../../../lib/format/bytes";
import { relativeTime } from "../../../../lib/format/relativeTime";
import "../../../../styles/account-onepage.css";
import { AccountAppearance } from "../AccountAppearance";
import { AccountOverlay } from "../AccountOverlay";
import { AccountSessions } from "../AccountSessions";
import { AccountStorage } from "../AccountStorage";
import { SteamDialogsField } from "../SteamDialogsField";
import { LaunchFullscreenField } from "../LaunchFullscreenField";
import { PasswordCard } from "../PasswordCard";
import { InputSection } from "./InputSection";
import { buildCapChips, deviceLabel } from "../DeviceCard";
import { ONE_PAGE_SECTIONS, sectionForPath, type OneSectionId } from "./sections";

const DAY = 864e5;
const UNUSED_DAYS = 90;
const daysSince = (iso: string) => Math.floor((Date.now() - Date.parse(iso)) / DAY);

export function OneSettingsPage() {
  const { pathname, key } = useLocation();

  // A path names a section: scroll to it (the bare page and Profile are the top).
  // `key` re-runs this when the current section's own link is clicked after scrolling away.
  const target = sectionForPath(pathname);
  useEffect(() => {
    const el = target && target !== "profile" ? document.getElementById(`set-${target}`) : null;
    if (el) el.scrollIntoView({ block: "start" });
    else document.querySelector(".main")?.scrollTo({ top: 0 });
    // Phones: bring the lit chip into the chip row's view.
    document.querySelector(".os-link.active")?.scrollIntoView({ block: "nearest", inline: "center" });
  }, [target, key]);

  const body: Record<OneSectionId, ReactNode> = {
    profile: <ProfileStrip />,
    streaming: <StreamSection />,
    input: <InputSection />,
    overlay: <AccountOverlay />,
    appearance: <AccountAppearance />,
    devices: <DevicesSection />,
    password: <PasswordCard />,
    storage: <AccountStorage />,
    sessions: <AccountSessions />,
  };

  return (
    <>
      <PageHeader title="Account" sub="All your settings on one page. Pick a section, or search for any setting from the top bar." />

      <div className="os-bar">
        <nav className="os-links" aria-label="Settings sections">
          {ONE_PAGE_SECTIONS.map((s) => (
            <Link key={s.id} to={s.to} className={s.id === target ? "os-link active" : "os-link"} aria-current={s.id === target ? "location" : undefined}>
              {s.label}
            </Link>
          ))}
        </nav>
      </div>

      {ONE_PAGE_SECTIONS.map((s) => (
        <section key={s.id} id={`set-${s.id}`} className={`os-sec os-sec-${s.id}`} aria-labelledby={`set-${s.id}-h`}>
          <div className="os-head">
            <h2 id={`set-${s.id}-h`}>{s.label}</h2>
            <p className="sub">{s.sub}</p>
          </div>
          {body[s.id]}
        </section>
      ))}
    </>
  );
}

/** Who you are, with the two counts the baseline Profile led with. */
function ProfileStrip() {
  const { user } = useAuth();
  const devices = useResource<Device[]>({
    label: "your devices",
    fetch: async ({ token, signal }) => (await authApi.listDevices(token, signal)).devices,
  });
  const storage = useResource<MyStorageItem[]>({
    label: "your storage",
    fetch: async ({ token, signal }) => (await getMyStorage(token, signal)).items,
  });
  if (!user) return null;
  const home = (storage.data ?? []).reduce((s, i) => s + i.bytes_used, 0);
  return (
    <div className="card sec-card os-id">
      <span className="u-ava os-ava" aria-hidden="true">{user.username.slice(0, 2).toUpperCase()}</span>
      <div className="grow os-who">
        <div className="os-name">{user.username}</div>
        <div className="sub">{user.email} · {user.role === "admin" ? "Admin" : "User"}</div>
      </div>
      <dl className="os-facts">
        <div><dt>Devices</dt><dd className="num">{devices.data ? devices.data.length : "—"}</dd></div>
        <div><dt>Stored</dt><dd className="num">{storage.data ? bytes(home) : "—"}</dd></div>
      </dl>
    </div>
  );
}

interface StreamPrefs {
  profiles: { id: string; name: string }[];
  defaultId: string;
  recommendedName: string | null;
  adminDefaultName: string | null;
  overridesAllowed: boolean;
}

function StreamSection() {
  const res = useResource<StreamPrefs>({
    label: "your stream default",
    fetch: async ({ token }) => {
      const [p, pref] = await Promise.all([libraryApi.getProfiles(token), libraryApi.getProfilePreferences(token)]);
      const profiles = p.profiles.map((x) => ({ id: x.id, name: x.display_name }));
      const recId = p.recommended_id ?? pref.global_default_profile_id;
      return {
        profiles,
        defaultId: pref.default_profile_id ?? "",
        recommendedName: profiles.find((x) => x.id === recId)?.name ?? null,
        adminDefaultName: profiles.find((x) => x.id === pref.global_default_profile_id)?.name ?? null,
        overridesAllowed: pref.user_overrides_allowed,
      };
    },
  });
  const [status, setStatus] = useState<{ ok: boolean; text: string } | null>(null);
  // The pick, shown at once and held until the save settles; it also holds the select shut, so one save runs at a time.
  const [picked, setPicked] = useState<string | null>(null);
  const hadFocus = useRef(false);
  const prefs = res.data;
  const recommended = `Recommended (${prefs?.recommendedName ?? "for this device"})`;
  // With overrides off the stored default is not what a launch starts (#721): say the admin's default.
  const current =
    prefs?.defaultId && prefs.overridesAllowed
      ? (prefs.profiles.find((p) => p.id === prefs.defaultId)?.name ?? prefs.defaultId)
      : (prefs?.adminDefaultName ?? recommended);

  // Saves on change: the select is labelled and the result is said back at once.
  const save = async (id: string) => {
    if (picked !== null) return;
    hadFocus.current = document.activeElement?.id === "stream-default";
    setPicked(id);
    setStatus(null);
    try {
      await res.mutate(
        ({ token }) => libraryApi.updateProfilePreferences(token, id || null),
        (d, r) => ({ ...d, defaultId: r.default_profile_id ?? "" }),
      );
      const name = prefs?.profiles.find((p) => p.id === id)?.name;
      setStatus({ ok: true, text: name ? `Saved. Your next launch asks for ${name}.` : "Saved. Launches use the recommendation again." });
    } catch (e) {
      setStatus({ ok: false, text: e instanceof ApiError ? e.message : "Could not save the stream default." });
    } finally {
      setPicked(null);
    }
  };
  // A disabled select drops focus; give it back once the save has settled.
  useEffect(() => {
    if (picked !== null || !hadFocus.current) return;
    hadFocus.current = false;
    document.getElementById("stream-default")?.focus();
  }, [picked]);

  return (
    <div className="card sec-card">
      <div className="os-field">
        <SelectField
          label="Stream default"
          value={picked ?? prefs?.defaultId ?? ""}
          disabled={!prefs || !prefs.overridesAllowed || picked !== null}
          onChange={(e) => void save(e.target.value)}
        >
          <option value="">{recommended}</option>
          {(prefs?.profiles ?? []).map((p) => (
            <option key={p.id} value={p.id}>{p.name}</option>
          ))}
        </SelectField>
      </div>
      {prefs && !prefs.overridesAllowed && <p className="muted mt3">An admin has turned off personal stream defaults.</p>}
      <p className={status && !status.ok ? "form-error mt3" : "muted mt3"} role="status">
        {status?.text ?? (prefs ? `Now: ${current}. Saved as you pick. A launch never exceeds what your device can decode.` : "Loading…")}
      </p>
      <div className="os-field mt5">
        <SteamDialogsField />
      </div>
      <div className="os-field mt5">
        <LaunchFullscreenField />
      </div>
    </div>
  );
}

function DevicesSection() {
  const { token, logout } = useAuth();
  const navigate = useNavigate();
  const { addToast } = useToast();
  const [target, setTarget] = useState<Device | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const res = useResource<Device[]>({
    label: "your devices",
    fetch: async ({ token: t, signal }) => (await authApi.listDevices(t, signal)).devices,
    initialData: [],
  });
  const list = [...(res.data ?? [])].sort(
    (a, b) => Number(b.current) - Number(a.current) || Date.parse(b.last_seen_at) - Date.parse(a.last_seen_at),
  );
  const unused = list.filter((d) => !d.current && daysSince(d.last_seen_at) >= UNUSED_DAYS);
  const inUse = list.filter((d) => !unused.includes(d));

  async function update(d: Device, patch: { name?: string; trusted?: boolean }) {
    if (!token) return;
    setBusy(d.id);
    try {
      const { device } = await authApi.updateDevice(token, d.id, patch);
      res.setData((l) => l.map((x) => (x.id === d.id ? device : x)));
    } catch (e) {
      addToast({ variant: "danger", title: "Could not update device", body: e instanceof ApiError ? e.message : undefined });
    } finally {
      setBusy(null);
    }
  }

  async function revoke(d: Device) {
    if (!token) return;
    setBusy(d.id);
    try {
      await authApi.revokeDevice(token, d.id);
      setTarget(null);
      addToast({ variant: "success", title: `${deviceLabel(d)} revoked` });
      if (d.current) {
        clearSession();
        await logout();
        navigate("/login", { replace: true });
        return;
      }
      void res.refresh();
    } catch (e) {
      addToast({ variant: "danger", title: "Could not revoke device", body: e instanceof ApiError ? e.message : undefined });
    } finally {
      setBusy(null);
    }
  }

  const group = (title: string, items: Device[], warn = false) =>
    items.length > 0 && (
      <div className="os-group" role="group" aria-label={title}>
        <h3 className={warn ? "os-group-h tone-warning" : "os-group-h"}>
          {title} <span className="num muted">{items.length}</span>
        </h3>
        <ul className="os-devs">
          {items.map((d) => (
            <DeviceRow key={d.id} d={d} busy={busy === d.id} stale={warn} onUpdate={update} onRevoke={setTarget} />
          ))}
        </ul>
      </div>
    );

  return (
    <div className="card sec-card">
      {res.errorMessage && <p className="form-error" role="alert">{res.errorMessage}</p>}
      {group("In use", inUse)}
      {group(`Not used in ${UNUSED_DAYS} days`, unused, true)}
      <p className="muted mt4 os-small">
        Revoking signs that device out at once and ends any session it runs. Capabilities are measured at sign-in.
      </p>

      {target && (
        <Modal
          open
          onClose={() => setTarget(null)}
          title={`Revoke ${deviceLabel(target)}?`}
          footer={
            <>
              <Button variant="ghost" onClick={() => setTarget(null)}>Cancel</Button>
              <Button variant="danger" disabled={busy === target.id} onClick={() => void revoke(target)}>
                {busy === target.id ? "Revoking…" : `Revoke ${deviceLabel(target)}`}
              </Button>
            </>
          }
        >
          <p className="sec">
            This signs <strong>{deviceLabel(target)}</strong> out immediately.
            {target.current && <> It is <strong>this device</strong>, so you will be signed out now.</>}
            {target.active_session_id && <> Its live session will end.</>}
          </p>
        </Modal>
      )}
    </div>
  );
}

function DeviceRow({
  d, busy, stale, onUpdate, onRevoke,
}: {
  d: Device; busy: boolean; stale: boolean;
  onUpdate: (d: Device, patch: { name?: string; trusted?: boolean }) => Promise<void>;
  onRevoke: (d: Device) => void;
}) {
  const name = deviceLabel(d);
  const [draft, setDraft] = useState<string | null>(null);
  const cap = d.capabilities ? buildCapChips(d.capabilities)[0]?.label : undefined;
  const commit = () => {
    const t = draft?.trim();
    setDraft(null);
    if (t && t !== name) void onUpdate(d, { name: t });
  };
  const days = daysSince(d.last_seen_at);
  return (
    <li className="os-dev" id={`os-dev-${d.id}`}>
      <div className="grow os-dev-text">
        {draft === null ? (
          <div className="os-dev-name">{name}</div>
        ) : (
          <input
            className="input"
            autoFocus
            aria-label={`New name for ${name}`}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            onBlur={commit}
            onKeyDown={(e) => {
              if (e.key === "Enter") commit();
              if (e.key === "Escape") setDraft(null);
            }}
          />
        )}
        <div className={stale ? "os-small tone-warning" : "os-small muted"}>
          {d.current ? "This device" : stale ? `Last used ${days} days ago` : `Last used ${relativeTime(d.last_seen_at)}`}
          {cap ? ` · ${cap}` : ""}
          {d.active_session_id ? " · streaming now" : ""}
        </div>
      </div>
      <div className="os-dev-acts">
        <span className="os-trust">
          <span className="os-small muted" aria-hidden="true">Trusted</span>
          <Switch checked={d.trusted} disabled={busy} aria-label={`Trust ${name}`} id={`os-trust-${d.id}`} onChange={(v) => void onUpdate(d, { trusted: v })} />
        </span>
        <Button variant="ghost" size="sm" disabled={busy} aria-label={`Rename ${name}`} onClick={() => setDraft(name)}>
          Rename
        </Button>
        <Button variant={stale ? "danger" : "ghost"} size="sm" disabled={busy} aria-label={`Revoke ${name}`} onClick={() => onRevoke(d)}>
          Revoke
        </Button>
      </div>
    </li>
  );
}
