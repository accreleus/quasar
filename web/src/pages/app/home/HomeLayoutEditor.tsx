// Edits one Home layout: the caller's own (account page) or the instance default
// (admin Settings). No mock covers it; it is the rung editor's ordered-row pattern
// (`.rung`, its move controls) with the house Switch, input and Button.
// semantics: control-api.md §Home layout

import { useState } from "react";
import { clearHomeLayout, getHomeLayout, putHomeLayout, type HomeLayout, type HomeScope, type HomeSection } from "../../../api/home";
import { useAuth } from "../../../auth/context";
import { Button } from "../../../components/Button";
import { ResourceStates } from "../../../components/ResourceStates";
import { Switch } from "../../../components/TextField";
import { useAdminAction } from "../../../lib/resource/action";
import { useResource } from "../../../lib/resource/react";
import { IconArrowDown, IconArrowUp, moveRung } from "../../admin/streaming/RungEditor";
import { HOME_KIND_LABEL, type HomeKind } from "./homeLayout";

const SOURCE_LINE: Record<HomeScope, Record<HomeLayout["source"], string>> = {
  "/me/home": {
    user: "You are using your own layout.",
    admin: "You are using the default your admin set.",
    builtin: "You are using the standard layout.",
  },
  "/admin/home": {
    user: "",
    admin: "Everyone without their own layout sees this.",
    builtin: "Everyone without their own layout sees the standard layout.",
  },
};

const kindLabel = (kind: string) => HOME_KIND_LABEL[kind as HomeKind] ?? kind;
const titleOk = (t: string) => t.trim().length > 0;

export function HomeLayoutEditor({ scope }: { scope: HomeScope }) {
  const { token } = useAuth();
  const layout = useResource<HomeLayout>(
    { label: "the Home layout", fetch: ({ token, signal }) => getHomeLayout(token, scope, signal) },
    [scope],
  );
  // null = untouched: the rows follow the server.
  const [draft, setDraft] = useState<HomeSection[] | null>(null);
  const sections = draft ?? layout.data?.sections ?? [];

  const applied = (next: HomeLayout) => {
    layout.setData(() => next);
    setDraft(null);
  };
  const save = useAdminAction(() => putHomeLayout(token ?? "", scope, sections), {
    success: "Home layout saved",
    failure: "Could not save the Home layout",
    onSuccess: applied,
  });
  const clear = useAdminAction(() => clearHomeLayout(token ?? "", scope), {
    success: scope === "/me/home" ? "Back on the default layout" : "Back on the standard layout",
    failure: "Could not reset the Home layout",
    onSuccess: applied,
  });
  const busy = save.pending !== null || clear.pending !== null;
  const update = (i: number, patch: Partial<HomeSection>) =>
    setDraft(sections.map((s, j) => (j === i ? { ...s, ...patch } : s)));

  const source = layout.data?.source;
  const own = scope === "/me/home" ? source === "user" : source === "admin";

  return (
    <div>
      <ResourceStates loading={layout.loading} error={layout.errorMessage} />
      {layout.data && (
        <>
          {sections.map((s, i) => {
            const name = kindLabel(s.kind);
            return (
              <div className="rung" key={s.id}>
                <span className="rank" aria-hidden="true">{i + 1}</span>
                <Switch
                  checked={s.enabled}
                  aria-label={`Show ${name}`}
                  disabled={busy}
                  onChange={(enabled) => update(i, { enabled })}
                />
                <input
                  className="input grow"
                  aria-label={`${name} title`}
                  value={s.title}
                  maxLength={64}
                  aria-invalid={!titleOk(s.title)}
                  disabled={busy}
                  onChange={(e) => update(i, { title: e.target.value })}
                />
                <span className="br">{name}</span>
                <span className="ctl">
                  <button
                    type="button"
                    title="Move up"
                    aria-label={`Move ${name} up`}
                    disabled={busy || i === 0}
                    onClick={() => setDraft(moveRung(sections, i, i - 1))}
                  >
                    <IconArrowUp />
                  </button>
                  <button
                    type="button"
                    title="Move down"
                    aria-label={`Move ${name} down`}
                    disabled={busy || i === sections.length - 1}
                    onClick={() => setDraft(moveRung(sections, i, i + 1))}
                  >
                    <IconArrowDown />
                  </button>
                </span>
              </div>
            );
          })}
          <div className="row between wrap gap3 mt3">
            <p className="hint m0" role="status">
              {source && SOURCE_LINE[scope][source]}
            </p>
            <div className="row gap2">
              {own && (
                <Button variant="ghost" size="sm" disabled={busy} onClick={() => void clear.run()}>
                  {scope === "/me/home" ? "Use the default" : "Use the standard layout"}
                </Button>
              )}
              <Button
                variant="primary"
                size="sm"
                disabled={busy || draft === null || !sections.every((s) => titleOk(s.title))}
                onClick={() => void save.run()}
              >
                Save
              </Button>
            </div>
          </div>
        </>
      )}
    </div>
  );
}
