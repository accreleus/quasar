// useResource — the binding only. The state machine's races are covered in
// core.test.ts with no renderer; what is asserted here is the wiring: token in,
// visibility in, subscription out, and teardown on unmount.

import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { render, screen, act } from "@testing-library/react";
import { useResource } from "./react";

let currentToken: string | null = "tok";
vi.mock("../../auth/context", () => ({ useAuth: () => ({ token: currentToken }) }));

function setHidden(hidden: boolean) {
  Object.defineProperty(document, "hidden", { configurable: true, value: hidden });
  act(() => {
    document.dispatchEvent(new Event("visibilitychange"));
  });
}

function Probe({ fetchFn, deps = [] }: { fetchFn: () => Promise<string[]>; deps?: unknown[] }) {
  const res = useResource({ label: "things", fetch: fetchFn, pollMs: 5000 }, deps);
  return (
    <div>
      <span data-testid="status">{res.status}</span>
      <span data-testid="loading">{String(res.loading)}</span>
      <span data-testid="data">{(res.data ?? []).join(",")}</span>
      <span data-testid="error">{res.errorMessage ?? ""}</span>
    </div>
  );
}

beforeEach(() => {
  currentToken = "tok";
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
  setHidden(false);
});

describe("useResource", () => {
  it("loads on mount and renders the resulting state", async () => {
    const fetchFn = vi.fn().mockResolvedValue(["a", "b"]);
    render(<Probe fetchFn={fetchFn} />);

    expect(screen.getByTestId("loading").textContent).toBe("true");
    await act(async () => {});
    expect(screen.getByTestId("status").textContent).toBe("ready");
    expect(screen.getByTestId("data").textContent).toBe("a,b");
    expect(screen.getByTestId("loading").textContent).toBe("false");
  });

  it("polls while visible, pauses while hidden, refetches on resume", async () => {
    const fetchFn = vi.fn().mockResolvedValue(["a"]);
    render(<Probe fetchFn={fetchFn} />);
    await act(async () => {});
    expect(fetchFn).toHaveBeenCalledTimes(1);

    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    expect(fetchFn).toHaveBeenCalledTimes(2);

    setHidden(true);
    await act(async () => {
      vi.advanceTimersByTime(60_000);
    });
    expect(fetchFn).toHaveBeenCalledTimes(2);

    setHidden(false);
    await act(async () => {});
    expect(fetchFn).toHaveBeenCalledTimes(3);
  });

  it("stops polling and ignores late responses after unmount", async () => {
    const fetchFn = vi.fn().mockResolvedValue(["a"]);
    const { unmount } = render(<Probe fetchFn={fetchFn} />);
    await act(async () => {});
    unmount();

    await act(async () => {
      vi.advanceTimersByTime(60_000);
    });
    expect(fetchFn).toHaveBeenCalledTimes(1); // no React "setState after unmount" either
  });

  it("surfaces a load failure without blanking the data", async () => {
    const fetchFn = vi
      .fn()
      .mockResolvedValueOnce(["a"])
      .mockRejectedValueOnce(new Error("boom"));
    render(<Probe fetchFn={fetchFn} />);
    await act(async () => {});

    await act(async () => {
      vi.advanceTimersByTime(5000);
    });
    expect(screen.getByTestId("data").textContent).toBe("a");
    expect(screen.getByTestId("error").textContent).toBe("could not load things");
  });

  it("does not fetch without a token, and loads once one arrives", async () => {
    currentToken = null;
    const fetchFn = vi.fn().mockResolvedValue(["a"]);
    const { rerender } = render(<Probe fetchFn={fetchFn} />);
    await act(async () => {});
    expect(fetchFn).not.toHaveBeenCalled();
    expect(screen.getByTestId("status").textContent).toBe("idle");

    currentToken = "tok";
    await act(async () => {
      rerender(<Probe fetchFn={fetchFn} />);
    });
    expect(fetchFn).toHaveBeenCalledTimes(1);
  });

  it("starts a fresh resource when deps change", async () => {
    const fetchFn = vi.fn().mockResolvedValue(["a"]);
    const { rerender } = render(<Probe fetchFn={fetchFn} deps={["id-1"]} />);
    await act(async () => {});
    expect(fetchFn).toHaveBeenCalledTimes(1);

    await act(async () => {
      rerender(<Probe fetchFn={fetchFn} deps={["id-2"]} />);
    });
    expect(fetchFn).toHaveBeenCalledTimes(2);
  });
});
