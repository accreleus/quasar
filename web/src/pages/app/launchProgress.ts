// Steam's shader progress (amendment 39) as a display hint. One reading for the
// launch loader and the quick-switch screen: it counts only while the app is
// booting, and only for the one step the contract defines.
import type { Session } from "../../api/types";

export function shaderPercent(session: Pick<Session, "state_detail" | "launch_progress">): number | null {
  const p = session.state_detail === "app booting" ? session.launch_progress : null;
  return p?.step === "shaders" ? p.percent : null;
}
