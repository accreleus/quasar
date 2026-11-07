/** Calls `onTick` every `ms` until the returned stop is called. */
export type Ticker = (ms: number, onTick: () => void) => () => void;

/** Driven by a Worker, so a throttled background tab still pings (pingWorker.ts). Falls back to
 *  a page timer where there is no Worker, or the page may not start one. */
export const workerTicker: Ticker = (ms, onTick) => {
  try {
    if (typeof Worker !== "undefined") {
      const worker = new Worker(new URL("./pingWorker.ts", import.meta.url), { type: "module" });
      worker.onmessage = onTick;
      worker.postMessage(ms);
      return () => worker.terminate();
    }
  } catch {
    /* the page may not start a Worker */
  }
  const id = setInterval(onTick, ms);
  return () => clearInterval(id);
};
