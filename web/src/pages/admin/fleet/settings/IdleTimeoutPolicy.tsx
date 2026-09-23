import { useEffect, useRef, useState } from "react";
import * as adminApi from "../../../../api/admin";
import { ApiError } from "../../../../api/client";
import { useAuth } from "../../../../auth/context";
import { Button } from "../../../../components/Button";
import { KnobPanel } from "./KnobPanel";

/** The first RH05 typed setting. The rest of the existing editor continues to
 * use its compatibility endpoint until their policy groups are implemented. */
export function IdleTimeoutPolicy({ hostId, onAvailable }: { hostId: string | undefined; onAvailable: (available: boolean) => void }) {
  const { token } = useAuth();
  const [view, setView] = useState<adminApi.HostPolicyView | null>(null);
  const [source, setSource] = useState<"deployment" | "explicit">("deployment");
  const [value, setValue] = useState("120");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [message, setMessage] = useState<string | null>(null);
  const draftTouched = useRef(false);

  useEffect(() => {
    if (!token || !hostId || typeof adminApi.getHostPolicy !== "function") return;
    let cancelled = false;
    const load = async () => {
      try {
        const next = await adminApi.getHostPolicy(token, hostId);
        if (cancelled) return;
        setView(next);
        if (!draftTouched.current) {
          setSource(next.choices.idle_timeout_secs?.source === "explicit" ? "explicit" : "deployment");
          const saved = next.choices.idle_timeout_secs?.value;
          if (typeof saved === "number") setValue(String(saved));
        }
        onAvailable(Boolean(next.groups.idle_timeout_secs && next.groups.idle_timeout_secs.status !== "upgrade_required"));
      } catch (e) {
        if (cancelled) return;
        if (e instanceof ApiError && e.status === 404) onAvailable(false);
        else setError(e instanceof ApiError ? e.message : "Could not load idle timeout policy.");
      }
    };
    void load();
    const poll = window.setInterval(() => { if (!cancelled) void load(); }, 5000);
    return () => { cancelled = true; window.clearInterval(poll); };
  }, [token, hostId, onAvailable]);

  if (!view || !view.groups.idle_timeout_secs) return null;
  const group = view.groups.idle_timeout_secs;
  const currentSource = view.choices.idle_timeout_secs?.source ?? "deployment";
  const currentValue = view.choices.idle_timeout_secs?.value;
  const changed = source !== currentSource || (source === "explicit" && Number(value) !== currentValue);

  const save = async () => {
    if (!token || !hostId || !changed) return;
    if (source === "explicit" && (!/^\d+$/.test(value) || !Number.isSafeInteger(Number(value)))) {
      setError("Enter a whole number of seconds, zero or greater.");
      return;
    }
    setSaving(true); setError(null); setMessage(null);
    try {
      const choice = source === "explicit" ? { source, value: Number(value) } : { source };
      const saved = await adminApi.updateHostPolicy(token, hostId, view.revision, { idle_timeout_secs: choice });
      setView(saved);
      draftTouched.current = false;
      setMessage("Saved intent. Check host status to confirm application.");
    } catch (e) {
      if (e instanceof ApiError && e.code === "stale_revision") {
        const current = await adminApi.getHostPolicy(token, hostId);
        setView(current);
        setError("This setting changed elsewhere. Review the current value and save again.");
      } else setError(e instanceof ApiError ? e.message : "Could not save idle timeout policy.");
    } finally { setSaving(false); }
  };

  return <KnobPanel title="Idle timeout" hint="Applies to new sessions when the host uses this setting. Existing sessions keep their launch value.">
    <div className="cset">
      <div>
        <h3>Session idle timeout</h3>
        <p className="hint">Source: {currentSource}. Application: {group.status.replaceAll("_", " ")}.</p>
        {group.remedy && <p className="hint">{group.remedy}</p>}
      </div>
      {group.status !== "upgrade_required" && <div>
        <label className="hint" htmlFor="idle-source">Source</label>
        <select className="select" style={{ width: 260 }} id="idle-source" value={source} onChange={(e) => { draftTouched.current = true; setSource(e.target.value as "deployment" | "explicit"); }}>
          <option value="deployment">Deployment setting</option><option value="explicit">Explicit value</option>
        </select>
        {source === "explicit" && <><label className="hint" htmlFor="idle-value">Seconds</label><input className="input num" style={{ width: 110 }} id="idle-value" type="number" min="0" step="1" value={value} onChange={(e) => { draftTouched.current = true; setValue(e.target.value); }} /></>}
        <Button variant="primary" disabled={!changed || saving} onClick={() => void save()}>{saving ? "Saving…" : "Save idle timeout"}</Button>
        {message && <p role="status" className="hint">{message}</p>}
        {error && <p role="alert" className="form-error">{error}</p>}
      </div>}
    </div>
  </KnobPanel>;
}
