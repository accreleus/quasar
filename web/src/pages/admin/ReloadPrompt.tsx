// Tells an admin their tab is running the previous build after the control
// plane updated under it (#117). Renders nothing; the toast is the whole UI.

import { useEffect, useRef } from "react";
import * as adminApi from "../../api/admin";
import type { PlatformIdentity } from "../../api/types";
import { useToast } from "../../components/Toast";
import { SOURCE_REF } from "../../lib/buildInfo";
import { useResource } from "../../lib/resource/react";
import { commitsMatch } from "./fleet/releasesCopy";

const COMMIT_RE = /^[0-9a-f]{7,40}$/i;

export function ReloadPrompt() {
  const { addToast } = useToast();
  // A release build bakes a TAG into SOURCE_REF, and a tag never equals a
  // commit, so the first commit this component observes is the baseline
  // instead. Comparing a tag would prompt on every load, forever.
  const baseline = useRef<string | null>(COMMIT_RE.test(SOURCE_REF) ? SOURCE_REF : null);
  const prompted = useRef(false);

  const { data } = useResource<{ identity: PlatformIdentity }>({
    label: "platform identity",
    fetch: ({ token, signal }) => adminApi.getPlatformIdentity(token, signal),
    pollMs: 30000,
  });

  const served = data?.identity.source_commit ?? "";

  useEffect(() => {
    if (!served) return;
    if (baseline.current === null) {
      baseline.current = served;
      return;
    }
    if (commitsMatch(baseline.current, served) || prompted.current) return;
    prompted.current = true;
    addToast({
      variant: "info",
      title: "Quasar was updated",
      body: "This page is running the previous build.",
      duration: null,
      action: { label: "Reload", onClick: () => window.location.reload() },
    });
  }, [served, addToast]);

  return null;
}
