// The host-settings page's state machine: load, edit-as-overrides, save, and
// the restart confirm/poll dance. Split out so HostSettings.tsx is
// composition (JSX) and this is behaviour, matching
// HostConsole.tsx's useConsoleLoad split.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import * as adminApi from "../../../../api/admin";
import { ApiError } from "../../../../api/client";
import type { ConfigKnob, GPUAvailability, Host } from "../../../../api/types";
import { useAuth } from "../../../../auth/context";
import { useFleetContext } from "../../../../lib/fleet/FleetContext";
import { primaryGpuLabel } from "../../../../lib/gpu";
import { coerceEffective, groupFor, shallowEqualOverrides, type OverrideValue, type SettingValue } from "./knobs";

// Restart polling: after triggering a restart, the agent reconnects with the
// new config asynchronously — poll until `pending_restart` clears (or give up).
const RESTART_POLL_MS = 3000;
const RESTART_POLL_MAX_ATTEMPTS = 10; // ~30s

export function useHostSettings(id: string | undefined) {
  const { token } = useAuth();
  const { sessions } = useFleetContext();

  const [host, setHost] = useState<Host | null>(null);
  const [knobs, setKnobs] = useState<ConfigKnob[]>([]);
  const [gpus, setGpus] = useState<GPUAvailability[]>([]);
  const [resolved, setResolved] = useState<Record<string, SettingValue>>({});
  const [effective, setEffective] = useState<Record<string, string> | null>(null);
  const [overrides, setOverrides] = useState<Record<string, OverrideValue>>({});
  // Last-persisted overrides map (server GET/PATCH), restored by discard() and
  // compared against `overrides` to derive `dirty` — never `{}`, since a host can
  // load with overrides already set.
  const [savedOverrides, setSavedOverrides] = useState<Record<string, OverrideValue>>({});
  const [pendingRestart, setPendingRestart] = useState(false);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [confirmRestart, setConfirmRestart] = useState(false);
  const [restarting, setRestarting] = useState(false);
  const [restartConfirmPending, setRestartConfirmPending] = useState(false);
  const [restartLiveSessions, setRestartLiveSessions] = useState<number | undefined>(undefined);
  const [showAdvanced, setShowAdvanced] = useState(false);
  const restartPollRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  // The restart poll's in-flight fetch can resolve after unmount (e.g. admin
  // navigates away mid-poll) — guard setState on it rather than on the
  // one-shot initial-load effect's own `cancelled` flag, since this ref is
  // shared across every poll tick, not just one effect run.
  const mountedRef = useRef(true);
  useEffect(() => {
    return () => { mountedRef.current = false; };
  }, []);

  /** Re-fetches settings and returns the freshly-reported `pending_restart`
   *  (used by the restart poll loop, which needs the value synchronously rather
   *  than through a stale closure over state). */
  const loadSettings = useCallback(async (): Promise<boolean> => {
    if (!token || !id) return false;
    const st = await adminApi.getHostSettings(token, id);
    if (!mountedRef.current) return st.pending_restart;
    setResolved(st.resolved);
    setEffective(st.effective ?? null);
    setOverrides(st.overrides);
    setSavedOverrides(st.overrides);
    setPendingRestart(st.pending_restart);
    return st.pending_restart;
  }, [id, token]);

  useEffect(() => {
    if (!token || !id) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    void (async () => {
      try {
        const [hostRes, cat, st, gpuRes] = await Promise.all([
          adminApi.getHost(token, id),
          adminApi.getConfigCatalog(token),
          adminApi.getHostSettings(token, id),
          adminApi.getHostGPUs(token, id).catch(() => ({ items: [] as GPUAvailability[] })),
        ]);
        if (cancelled) return;
        setHost(hostRes.host);
        setKnobs(cat.knobs);
        setGpus(gpuRes.items);
        setResolved(st.resolved);
        setEffective(st.effective ?? null);
        setOverrides(st.overrides);
        setSavedOverrides(st.overrides);
        setPendingRestart(st.pending_restart);
      } catch (e: unknown) {
        if (!cancelled) setError(e instanceof ApiError ? e.message : "Could not load host settings.");
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => { cancelled = true; };
  }, [id, token]);

  // Stop any in-flight restart poll on unmount.
  useEffect(() => {
    return () => {
      if (restartPollRef.current) clearTimeout(restartPollRef.current);
    };
  }, []);

  const changedCount = Object.keys(overrides).length;
  const dirty = !shallowEqualOverrides(overrides, savedOverrides);
  const liveSessionsCount = useMemo(
    () => sessions.filter((s) => s.host_id === id).length,
    [sessions, id],
  );

  const grouped = useMemo(() => {
    const result = {
      runtime: [] as ConfigKnob[],
      encoder: [] as ConfigKnob[],
      adaptation: [] as ConfigKnob[],
      advanced: [] as ConfigKnob[],
    };
    for (const knob of knobs) result[groupFor(knob)].push(knob);
    return result;
  }, [knobs]);

  // render_node becomes a picker: "software" + one option per GPU with a
  // reported by-path device, so admins pick a stable device instead of typing
  // a free-text path. If the current value isn't among these (a custom path,
  // or a pre-amendment host with no GPU-reported render_node), it's added as
  // an extra option so the control round-trips without clobbering it.
  const renderNodeOptions = useMemo(() => {
    const opts: { value: string; label: string }[] = [
      { value: "software", label: "Software rendering" },
    ];
    for (const g of gpus) {
      if (g.render_node) {
        opts.push({ value: g.render_node, label: `${primaryGpuLabel(g.vendor, g.model)} · ${g.render_node}` });
      }
    }
    return opts;
  }, [gpus]);

  const effectiveValueOf = useCallback((k: ConfigKnob): SettingValue | undefined =>
    effective && k.key in effective ? coerceEffective(k, effective[k.key]) : undefined,
  [effective]);

  // Input initialization: an explicit override wins; otherwise prefer the
  // agent's true running value over the catalog-resolved display (which can't
  // see agent env and would show e.g. render_node "software" while the agent
  // runs a real device).
  const valueOf = useCallback((k: ConfigKnob): SettingValue | undefined => {
    if (k.key in overrides) {
      const ov = overrides[k.key];
      return ov === null ? resolved[k.key] : ov;
    }
    const eff = effectiveValueOf(k);
    return eff !== undefined ? eff : resolved[k.key];
  }, [overrides, resolved, effectiveValueOf]);

  const setValue = (key: string, v: SettingValue) => {
    setConfirmRestart(false);
    setOverrides((prev) => ({ ...prev, [key]: v }));
  };

  const resetKey = (key: string) => {
    setConfirmRestart(false);
    setOverrides((prev) => ({ ...prev, [key]: null }));
  };

  const discard = () => {
    setOverrides(savedOverrides);
    setConfirmRestart(false);
    setRestartLiveSessions(undefined);
  };

  const cancelSaveConfirm = () => {
    setConfirmRestart(false);
    setRestartLiveSessions(undefined);
  };

  const cancelRestartConfirm = () => {
    setRestartConfirmPending(false);
    setRestartLiveSessions(undefined);
  };

  // render_node picker: has this specific knob's config target (override, or
  // resolved when un-overridden) not yet reached the agent? Restricted to
  // "there's an override waiting to apply" so it rarely fires — a knob whose
  // resolved default simply disagrees with a permanent env-set value on the
  // agent is not a pending-restart situation, just how the box is configured.
  const isStaleEffective = useCallback((k: ConfigKnob): boolean => {
    if (k.class !== "restart") return false;
    if (!(k.key in overrides)) return false;
    const ov = overrides[k.key];
    const target = ov === null ? resolved[k.key] : ov;
    const eff = effectiveValueOf(k);
    return eff !== undefined && eff !== target;
  }, [overrides, resolved, effectiveValueOf]);

  const hasStaleEffectiveKnob = knobs.some(isStaleEffective);
  const dirtyRestartKnobs = knobs.filter((k) => k.class === "restart" && k.key in overrides);
  const hasDirtyRestart = dirtyRestartKnobs.length > 0;
  // pending_restart is host-wide; hasStaleEffectiveKnob is the per-knob signal
  // that a saved change hasn't reached the agent yet — either means there's a
  // genuine restart to offer standalone (distinct from the proactive "save
  // dirty restart-class edits" confirm flow below).
  const showRestartButton = (pendingRestart || hasStaleEffectiveKnob) && !confirmRestart && !restartConfirmPending;

  const startRestartPoll = useCallback(() => {
    if (restartPollRef.current) clearTimeout(restartPollRef.current);
    let attempts = 0;
    const tick = () => {
      restartPollRef.current = setTimeout(() => {
        attempts += 1;
        void loadSettings()
          .then((stillPending) => {
            if (stillPending && attempts < RESTART_POLL_MAX_ATTEMPTS) {
              tick();
            } else {
              restartPollRef.current = null;
            }
          })
          .catch(() => {
            // Transient poll error — keep trying until attempts run out.
            if (attempts < RESTART_POLL_MAX_ATTEMPTS) tick();
            else restartPollRef.current = null;
          });
      }, RESTART_POLL_MS);
    };
    tick();
  }, [loadSettings]);

  const save = async (restartConfirm = false) => {
    if (!token || !id) return;
    setSaving(true);
    setError(null);
    try {
      const res = await adminApi.updateHostSettings(token, id, overrides, restartConfirm);
      setResolved(res.resolved);
      setEffective(res.effective ?? null);
      setOverrides(res.overrides);
      setSavedOverrides(res.overrides);
      setConfirmRestart(false);
      setRestartLiveSessions(undefined);
      // restart_triggered=false does not mean no restart is pending — it just
      // means this particular save didn't trigger one; leave pendingRestart as
      // last reported by GET rather than stomping it to false.
      if (res.restart_triggered) {
        setPendingRestart(true);
        startRestartPoll();
      }
    } catch (e: unknown) {
      if (e instanceof ApiError && e.code === "restart_required") {
        setConfirmRestart(true);
        setRestartLiveSessions(e.liveSessions);
      } else {
        setError(e instanceof ApiError ? e.message : "Save failed.");
      }
    } finally {
      setSaving(false);
    }
  };

  // The primary button's click: dirty restart-class knobs need a second click
  // to confirm before the PATCH actually restarts the agent.
  const saveChanges = () => {
    if (hasDirtyRestart && !confirmRestart) {
      setConfirmRestart(true);
      return;
    }
    void save(hasDirtyRestart || confirmRestart);
  };

  const handleRestart = async (confirm = false) => {
    if (!token || !id) return;
    setRestarting(true);
    setError(null);
    try {
      const res = await adminApi.restartHost(token, id, confirm);
      setRestartConfirmPending(false);
      setRestartLiveSessions(undefined);
      if (res.restart_triggered) {
        setPendingRestart(true);
        startRestartPoll();
      }
    } catch (e: unknown) {
      if (e instanceof ApiError && e.code === "restart_required") {
        setRestartConfirmPending(true);
        setRestartLiveSessions(e.liveSessions);
      } else {
        setError(e instanceof ApiError ? e.message : "Restart failed.");
      }
    } finally {
      setRestarting(false);
    }
  };

  return {
    host, knobs, resolved, effective, overrides,
    pendingRestart, loading, saving, error, setError,
    confirmRestart, restarting, restartConfirmPending,
    restartLiveSessions, showAdvanced, setShowAdvanced,
    changedCount, dirty, liveSessionsCount, grouped, renderNodeOptions,
    effectiveValueOf, valueOf, setValue, resetKey, discard, isStaleEffective,
    hasStaleEffectiveKnob, dirtyRestartKnobs, hasDirtyRestart, showRestartButton,
    save, saveChanges, handleRestart, cancelSaveConfirm, cancelRestartConfirm,
  };
}
