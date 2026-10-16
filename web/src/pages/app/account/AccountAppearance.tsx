// /app/account/appearance — which layout the player-side pages use.
//
// One switch, saved immediately like the overlay page (a presentation toggle,
// no destructive outcome). "Classic layout" is stored as `classic_layout` in
// the user's ui-preferences, so off (the default) means the new design.

import { Switch } from "../../../components/TextField";
import { useToast } from "../../../components/Toast";
import { useSectionHead } from "../../../components/shell/sectionHead";
import { useOverlayPreferences } from "../../../settings/OverlayPreferencesContext";

export function AccountAppearance() {
  const { loaded, classicLayout, setClassicLayout, classicSaving } = useOverlayPreferences();
  const { addToast } = useToast();
  useSectionHead({ sub: "How the library, game page and account pages look. This follows your account to every device." });

  const toggle = (next: boolean) =>
    setClassicLayout(next).catch((e: unknown) =>
      addToast({
        variant: "danger",
        title: "Could not save layout",
        body: e instanceof Error ? e.message : undefined,
      }),
    );

  return (
    <div className="card sec-card">
      <div className="ac-sw">
        <Switch
          label="Classic layout"
          id="classic-layout"
          checked={classicLayout}
          disabled={!loaded || classicSaving}
          aria-describedby="classic-layout-hint"
          onChange={(v) => void toggle(v)}
        />
      </div>
      <p className="hint mt3" id="classic-layout-hint">Use the previous library, game page and account layout.</p>
    </div>
  );
}
