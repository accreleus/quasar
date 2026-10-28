import { Suspense } from "react";
import { act, render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { preloadable } from "./preloadable";

const Page = () => <p>page</p>;

describe("preloadable", () => {
  it("renders a preloaded module without ever committing the fallback", async () => {
    const p = preloadable(() => Promise.resolve({ Page }));
    p.preload();
    await act(async () => {});
    const Lazy = p.component("Page");
    render(<Suspense fallback={<p>fallback</p>}><Lazy /></Suspense>);
    expect(screen.getByText("page")).toBeInTheDocument();
    expect(screen.queryByText("fallback")).toBeNull();
  });

  it("suspends until a module that was not preloaded arrives", async () => {
    const Lazy = preloadable(() => Promise.resolve({ Page })).component("Page");
    render(<Suspense fallback={<p>fallback</p>}><Lazy /></Suspense>);
    expect(screen.getByText("fallback")).toBeInTheDocument();
    expect(await screen.findByText("page")).toBeInTheDocument();
  });

  it("retries a load that failed", async () => {
    let calls = 0;
    const p = preloadable(() => (++calls === 1 ? Promise.reject(new Error("chunk")) : Promise.resolve({ Page })));
    p.preload();
    await act(async () => {});
    p.preload();
    await act(async () => {});
    expect(calls).toBe(2);
    const Lazy = p.component("Page");
    render(<Suspense fallback={<p>fallback</p>}><Lazy /></Suspense>);
    expect(screen.getByText("page")).toBeInTheDocument();
  });
});
