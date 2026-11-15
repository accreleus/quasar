/** Calls `onTick` every `ms` until the returned stop is called. */
export type Ticker = (ms: number, onTick: () => void) => () => void;

/** Driven by a Worker, so a throttled background tab still pings (pingWorker.ts). Falls back to
 *  a page timer where there is no Worker, the page may not start one, or its script fails to load
 *  (a 404, a wrong MIME type, a policy: those arrive as an async `error`, not a throw).
 *  shortcut: the fallback timer is throttled in a hidden tab like any page timer; upgrade with #1154-style follow-up. */
export const workerTicker: Ticker = (ms, onTick) => {
  let stop = () => {};
  const pageTimer = () => {
    const id = setInterval(onTick, ms);
    stop = () => clearInterval(id);
  };
  try {
    if (typeof Worker !== "undefined") {
      const worker = new Worker(new URL("./pingWorker.ts", import.meta.url), { type: "module" });
      let stopped = false;
      worker.onmessage = onTick;
      worker.onerror = () => {
        worker.terminate();
        if (!stopped) pageTimer();
      };
      worker.postMessage(ms);
      stop = () => {
        stopped = true;
        worker.terminate();
      };
      return () => stop();
    }
  } catch {
    /* the page may not start a Worker */
  }
  pageTimer();
  return () => stop();
};
