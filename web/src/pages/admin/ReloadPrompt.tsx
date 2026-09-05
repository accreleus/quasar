// Tells an admin their tab is running a bundle the server no longer serves
// (#117). Renders nothing; the toast is the whole UI.
//
// The question is "would a reload fetch a different bundle?", and only the
// hashed bundle name answers it. Comparing the baked SOURCE_REF against the
// control plane's `source_commit` does not: a source-built stack rebuilds
// web/dist and the control-plane image from different commits (a `redeploy.sh
// … web` rebuilds only the SPA), so those differ permanently and the toast
// would return after every reload, forever.

import { useCallback, useEffect, useRef } from "react";
import * as adminApi from "../../api/admin";
import type { PlatformIdentity } from "../../api/types";
import { useToast } from "../../components/Toast";
import { useResource } from "../../lib/resource/react";

/** The entry bundle's hashed name. Twin of deploy/redeploy.sh's served-bundle
 *  check, which greps exactly this out of the served index.html. */
const BUNDLE_RE = /assets\/(index-[A-Za-z0-9_-]+\.js)/;

/** The floor between checks when nothing else prompts one. */
const RECHECK_MS = 5 * 60 * 1000;

/** Dismissals are per bundle and per tab session, so a reload (a new hash)
 *  clears them without anything having to remember to. */
const DISMISSED_PREFIX = "quasar-reload-dismissed:";

/** The bundle this document is running, or null when there is none to read —
 *  a dev server or a test, where there is nothing to compare and so nothing to
 *  say. */
function loadedBundle(): string | null {
  for (const el of Array.from(document.querySelectorAll("script[src]"))) {
    const match = BUNDLE_RE.exec(el.getAttribute("src") ?? "");
    if (match) return match[1];
  }
  return null;
}

async function servedBundle(): Promise<string | null> {
  const resp = await fetch("/index.html", { cache: "no-store" });
  if (!resp.ok) return null;
  return BUNDLE_RE.exec(await resp.text())?.[1] ?? null;
}

function dismissed(bundle: string): boolean {
  try {
    return sessionStorage.getItem(DISMISSED_PREFIX + bundle) === "1";
  } catch {
    return false;
  }
}

export function ReloadPrompt() {
  const { addToast } = useToast();
  // The served bundle this tab has already spoken about.
  const promptedFor = useRef<string | null>(null);

  const { data } = useResource<{ identity: PlatformIdentity }>({
    label: "platform identity",
    fetch: ({ token, signal }) => adminApi.getPlatformIdentity(token, signal),
    pollMs: 30000,
  });
  const served = data?.identity.source_commit ?? "";

  const check = useCallback(async () => {
    const loaded = loadedBundle();
    if (!loaded) return;
    let current: string | null = null;
    try {
      current = await servedBundle();
    } catch {
      return; // the control plane is restarting, which is not news
    }
    if (!current || current === loaded) return;
    if (promptedFor.current === current || dismissed(current)) return;
    promptedFor.current = current;
    addToast({
      variant: "info",
      title: "Quasar was updated",
      body: "This page is running the previous build.",
      duration: null,
      action: { label: "Reload", onClick: () => window.location.reload() },
      onDismiss: () => {
        try {
          sessionStorage.setItem(DISMISSED_PREFIX + current, "1");
        } catch {
          // A tab with no session storage just gets asked again next check.
        }
      },
    });
  }, [addToast]);

  // The served commit moving is the strongest hint the bundle moved with it.
  useEffect(() => {
    void check();
  }, [served, check]);

  // And a floor under that, because a web-only redeploy moves the bundle and
  // leaves the commit alone. A chained timeout, never an interval: a stacked
  // check is unrepresentable this way.
  useEffect(() => {
    let timer = setTimeout(function tick() {
      void check();
      timer = setTimeout(tick, RECHECK_MS);
    }, RECHECK_MS);
    return () => clearTimeout(timer);
  }, [check]);

  return null;
}
