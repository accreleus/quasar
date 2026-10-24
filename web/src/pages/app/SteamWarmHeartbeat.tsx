// The library heartbeat, mounted once in AppLayout (library and game page). A
// null child, so a beat's state change re-renders nothing else.
//
// A launch in flight pauses it: a beat between two capacity retries would start
// a parked Steam for the retry to stop again. useLaunch reports through
// LaunchActivityContext; without a provider the report is a no-op.

import { createContext, useContext, useEffect, useRef } from "react";
import { useSteamWarmHeartbeat } from "./useSteamWarmHeartbeat";

export const LaunchActivityContext = createContext<(launching: boolean) => void>(() => {});

/** Tell the layout this page's launch is (not) in flight. */
export function useReportLaunching(launching: boolean): void {
  const report = useContext(LaunchActivityContext);
  useEffect(() => {
    report(launching);
    return () => report(false);
  }, [launching, report]);
}

export function SteamWarmHeartbeat({ paused }: { paused: boolean }) {
  const { pause, resume } = useSteamWarmHeartbeat();
  const wasPaused = useRef(false);
  useEffect(() => {
    if (paused) pause();
    else if (wasPaused.current) resume();
    wasPaused.current = paused;
  }, [paused, pause, resume]);
  return null;
}
