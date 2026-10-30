// /app/account/appearance — which layout the player-side pages use.
//
// One switch, saved immediately like the overlay page (a presentation toggle,
// no destructive outcome). "Classic layout" is stored as `classic_layout` in
// the user's ui-preferences, so off (the default) means the new design.

import { useCallback, useEffect, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { Switch } from "../../../components/TextField";
import { useToast } from "../../../components/Toast";
import { useSectionHead } from "../../../components/shell/sectionHead";
import { revealWhenReady } from "../../../components/shell/settingsSearch/reveal";
import { useOverlayPreferences } from "../../../settings/OverlayPreferencesContext";

const PATH = "/app/account/appearance";
const SWITCH = { sel: "#classic-layout" };

export function AccountAppearance() {
  const navigate = useNavigate();
  const { pathname, key } = useLocation();
  const { loaded, classicLayout, setClassicLayout, classicSaving } = useOverlayPreferences();
  const { addToast } = useToast();
  useSectionHead({ sub: "How the library, game page and account pages look. This follows your account to every device." });

  // The flip swaps the page tree under this switch, so the user is kept at the
  // Appearance path (the bare path and /security redirect to Profile in classic)
  // and the switch is focused again once the save has settled, success or revert.
  // Router navigations are transitions: the flip waits for the path to land, or
  // the classic outlet would render once at the old path and redirect.
  const [pendingFlip, setPendingFlip] = useState<boolean | null>(null);
  const flip = useCallback(
    (next: boolean) =>
      setClassicLayout(next).catch((e: unknown) =>
        addToast({
          variant: "danger",
          title: "Could not save layout",
          body: e instanceof Error ? e.message : undefined,
        }),
      ),
    [setClassicLayout, addToast],
  );
  useEffect(() => {
    if (pendingFlip === null || pathname !== PATH) return;
    setPendingFlip(null);
    void flip(pendingFlip);
  }, [pendingFlip, pathname, flip]);

  const toggle = (next: boolean) => {
    const moves = pathname !== PATH;
    revealWhenReady(SWITCH, key, moves);
    if (moves) {
      setPendingFlip(next);
      navigate(PATH, { replace: true });
    } else void flip(next);
  };

  return (
    <div className="card sec-card">
      <div className="ac-sw">
        <Switch
          label="Classic layout"
          id="classic-layout"
          checked={classicLayout}
          disabled={!loaded || classicSaving || pendingFlip !== null}
          aria-describedby="classic-layout-hint"
          onChange={toggle}
        />
      </div>
      <p className="hint mt3" id="classic-layout-hint">Use the previous library, game page and account layout. Classic layout will be removed in a later release.</p>
    </div>
  );
}
