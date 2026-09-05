/**
 * useContainerWidth — measure an element's width and track it across resizes.
 *
 * Shared by every hand-rolled SVG chart (Charts.tsx, TraceViewer.tsx). It
 * returns a CALLBACK ref, deliberately:
 *
 * The obvious `useRef` + `useEffect(..., [])` version is subtly broken for any
 * caller that renders the measured element behind an early return. On the first
 * render the ref is null, the effect bails, and — with empty deps — it never
 * runs again, so the observer is never attached and the width stays at the
 * fallback for the life of the component. #124: TraceViewer always mounts in its
 * loading state, so its lanes were pinned to `600 - 80 = 520` px on every screen,
 * which also fixed the x-axis onto a 6-interval (non-round) tick grid.
 *
 * A callback ref cannot have that bug: React calls it with the node whenever the
 * node appears, changes, or goes away, however many renders later that is.
 */
import { useCallback, useEffect, useRef, useState } from "react";

export type ContainerWidthRef = (el: HTMLElement | null) => void;

export function useContainerWidth(fallback: number): [ContainerWidthRef, number] {
  const [width, setWidth] = useState(fallback);
  const observerRef = useRef<ResizeObserver | null>(null);

  const ref = useCallback<ContainerWidthRef>((el) => {
    observerRef.current?.disconnect();
    observerRef.current = null;
    if (!el) return;

    const ro = new ResizeObserver((entries) => {
      const w = entries[0]?.contentRect.width;
      if (w && w > 0) setWidth(w);
    });
    ro.observe(el);
    observerRef.current = ro;

    // ResizeObserver delivers its first entry asynchronously, so seed from the
    // live box as well: without this the chart paints one frame at the fallback
    // width. jsdom reports 0 here, which is why the tests still see `fallback`.
    const w = el.getBoundingClientRect().width;
    if (w > 0) setWidth(w);
  }, []);

  useEffect(() => () => observerRef.current?.disconnect(), []);

  return [ref, width];
}
