/**
 * useResource — the entire React surface of the resource machine (./core.ts).
 *
 * Everything interesting lives in the core; this binds one resource to a
 * component's lifetime, feeds it the auth token and the page's visibility, and
 * subscribes to its state. Keeping it this thin is the point: the races are
 * tested in core.test.ts with no renderer at all.
 */

import { useEffect, useMemo, useSyncExternalStore } from "react";
import { useAuth } from "../../auth/context";
import { createResource, type Resource, type ResourceSpec, type ResourceState } from "./core";

export interface UseResourceResult<T> extends ResourceState<T> {
  /** True only before the first data arrives — the drop-in for the `loading`
   *  flag every admin page declared by hand. A refresh or a poll tick never
   *  sets it, so saving does not flash a spinner over the table. */
  loading: boolean;
  refresh: Resource<T>["refresh"];
  mutate: Resource<T>["mutate"];
  setData: Resource<T>["setData"];
  /** Operator-controlled polling (an "Auto-refresh" switch). Stable for the
   *  life of the resource, so an effect may depend on them. */
  pause: Resource<T>["pause"];
  resume: Resource<T>["resume"];
}

/**
 * Bind a resource to this component.
 *
 * `spec` is captured when `deps` change — inline object literals are expected,
 * and their identity is ignored, exactly like a `useEffect` dependency array.
 * Pass anything the spec closes over (a route `:id`); the token is handled here
 * and must NOT be listed. A deps change discards the old resource and starts a
 * fresh one, so no data crosses between, say, two session ids.
 *
 * Known hazard, unmitigated: a forgotten `deps` entry is a stale-closure bug,
 * and `web/` runs no ESLint (there is no config and no lint script), so nothing
 * checks the array against the spec's closure. `react-hooks/exhaustive-deps`
 * with this hook in `additionalHooks` would cover it if linting is ever added.
 * Until then it is review discipline: list every value the spec closes over.
 */
export function useResource<T>(
  spec: ResourceSpec<T>,
  deps: readonly unknown[] = [],
): UseResourceResult<T> {
  const { token } = useAuth();

  const resource = useMemo(() => createResource(spec), deps);

  // Ordered before start()'s effect, so the first load always has the token.
  useEffect(() => {
    resource.setToken(token ?? null);
  }, [resource, token]);

  useEffect(() => {
    const onVisibility = () => resource.setVisible(!document.hidden);
    resource.setVisible(!document.hidden);
    document.addEventListener("visibilitychange", onVisibility);
    resource.start();
    return () => {
      document.removeEventListener("visibilitychange", onVisibility);
      resource.stop();
    };
  }, [resource]);

  const state = useSyncExternalStore(resource.subscribe, resource.getState, resource.getState);

  return {
    ...state,
    loading: state.status === "loading",
    refresh: resource.refresh,
    mutate: resource.mutate,
    setData: resource.setData,
    pause: resource.pause,
    resume: resource.resume,
  };
}
