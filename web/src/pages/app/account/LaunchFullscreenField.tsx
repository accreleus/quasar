// "Launch in full screen and capture input" (#647). Device-local, like the
// scaling mode it is stored beside: a phone and a desktop want different things,
// so it does not follow the account. Saved on change.

import { useState } from "react";
import { Switch } from "../../../components/TextField";
import { launchImmersive, saveDisplayPreferences } from "../../../settings/displayPreferences";

export function LaunchFullscreenField() {
  const [on, setOn] = useState(launchImmersive);
  return (
    <div>
      <div className="ac-sw">
        <Switch
          label="Launch in full screen and capture input"
          id="launch-fullscreen"
          checked={on}
          aria-describedby="launch-fullscreen-hint"
          onChange={(next) => {
            saveDisplayPreferences({ launchImmersive: next });
            setOn(next);
          }}
        />
      </div>
      <p className="hint mt3" id="launch-fullscreen-hint">
        Play goes straight into the game. Applies to this device only.
      </p>
    </div>
  );
}
