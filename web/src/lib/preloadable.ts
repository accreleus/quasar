import { lazy, type ComponentType } from "react";

type LazyModule = { default: ComponentType<any> };

/**
 * A lazily loaded module that can be fetched ahead of need (hover, focus, idle,
 * a deep link). Once it has arrived, its components render without suspending:
 * React.lazy reads a thenable that resolves synchronously without committing a
 * fallback, so the page skips React's Suspense reveal throttle (~300 ms). That is
 * React.lazy behaviour, not API: guarded by preloadable.test.tsx.
 */
export function preloadable<M extends object>(load: () => Promise<M>) {
  let mod: M | undefined;
  let pending: Promise<M> | undefined;
  const get = () =>
    (pending ??= load().then(
      (m) => (mod = m),
      (e: unknown) => {
        pending = undefined; // a failed chunk fetch may be retried
        throw e;
      },
    ));
  return {
    /** Start fetching; errors are left to the render that needs the module. */
    preload: () => void get().catch(() => {}),
    component: (name: keyof M) =>
      lazy(() => {
        const m = mod;
        if (m) return { then: (ok: (v: LazyModule) => void) => ok({ default: m[name] as ComponentType<any> }) } as unknown as Promise<LazyModule>;
        return get().then((loaded) => ({ default: loaded[name] as ComponentType<any> }));
      }),
  };
}
