// useAdminAction — pending state and the toast contract that replaces the
// hand-written try/catch/finally around every admin write.

import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, act } from "@testing-library/react";
import { useAdminAction } from "./action";
import { ApiError } from "../../api/client";

const addToast = vi.fn();
vi.mock("../../components/Toast", () => ({ useToast: () => ({ addToast, removeToast: vi.fn() }) }));

interface Row {
  id: string;
}

function Probe({
  fn,
  opts,
  rows = [{ id: "r1" }, { id: "r2" }],
}: {
  fn: (row: Row) => Promise<string>;
  opts: Parameters<typeof useAdminAction<[Row], string>>[1];
  rows?: Row[];
}) {
  const action = useAdminAction<[Row], string>(fn, opts);
  return (
    <div>
      <span data-testid="pending">{action.pending ? action.pending[0].id : "idle"}</span>
      {rows.map((row) => (
        <button
          key={row.id}
          data-testid={`run-${row.id}`}
          disabled={action.pending?.[0].id === row.id}
          onClick={() => void action.run(row)}
        >
          {row.id}
        </button>
      ))}
    </div>
  );
}

beforeEach(() => addToast.mockClear());

describe("useAdminAction", () => {
  it("tracks the pending args and clears them when the run settles", async () => {
    let release!: (v: string) => void;
    const fn = vi.fn(() => new Promise<string>((res) => (release = res)));
    render(<Probe fn={fn} opts={{ failure: "could not do it" }} />);

    await act(async () => {
      screen.getByTestId("run-r1").click();
    });
    expect(screen.getByTestId("pending").textContent).toBe("r1");
    expect(screen.getByTestId("run-r1")).toBeDisabled();
    expect(screen.getByTestId("run-r2")).not.toBeDisabled(); // per-row gating

    await act(async () => {
      release("ok");
    });
    expect(screen.getByTestId("pending").textContent).toBe("idle");
  });

  it("runs onSuccess before the success toast, with interpolation", async () => {
    const order: string[] = [];
    addToast.mockImplementation(() => order.push("toast"));
    render(
      <Probe
        fn={() => Promise.resolve("saved")}
        opts={{
          success: (result, row) => `${row.id} ${result}`,
          failure: "nope",
          onSuccess: () => order.push("onSuccess"),
        }}
      />,
    );

    await act(async () => {
      screen.getByTestId("run-r1").click();
    });
    expect(order).toEqual(["onSuccess", "toast"]);
    expect(addToast).toHaveBeenCalledWith({ variant: "success", title: "r1 saved" });
  });

  it("omits the success toast when no `success` copy is given", async () => {
    render(<Probe fn={() => Promise.resolve("ok")} opts={{ failure: "nope" }} />);
    await act(async () => {
      screen.getByTestId("run-r1").click();
    });
    expect(addToast).not.toHaveBeenCalled();
  });

  it("prefers an ApiError's own message over the fallback copy", async () => {
    render(
      <Probe
        fn={() => Promise.reject(new ApiError(409, "preset_in_use", "preset is in use"))}
        opts={{ failure: "could not delete preset" }}
      />,
    );
    await act(async () => {
      screen.getByTestId("run-r1").click();
    });
    expect(addToast).toHaveBeenCalledWith({ variant: "danger", title: "preset is in use" });
  });

  it("falls back to the page's copy for a non-ApiError", async () => {
    render(
      <Probe fn={() => Promise.reject(new TypeError("network"))} opts={{ failure: "could not delete preset" }} />,
    );
    await act(async () => {
      screen.getByTestId("run-r1").click();
    });
    expect(addToast).toHaveBeenCalledWith({ variant: "danger", title: "could not delete preset" });
  });

  it("lets the failure function branch on the error code", async () => {
    render(
      <Probe
        fn={() => Promise.reject(new ApiError(409, "home_in_use", "home is in use"))}
        opts={{
          failure: (e) =>
            e instanceof ApiError && e.code === "home_in_use"
              ? "Stop the running session first"
              : "could not delete home",
        }}
      />,
    );
    await act(async () => {
      screen.getByTestId("run-r1").click();
    });
    expect(addToast).toHaveBeenCalledWith({
      variant: "danger",
      title: "Stop the running session first",
    });
  });

  it("carries a toast body when the copy has two lines", async () => {
    render(
      <Probe
        fn={() => Promise.resolve("ok")}
        opts={{
          success: () => ({ title: "Artwork sweep queued", body: "starts within 30s" }),
          failure: () => ({ title: "Could not run job now", body: "another run is in flight" }),
        }}
      />,
    );
    await act(async () => {
      screen.getByTestId("run-r1").click();
    });
    expect(addToast).toHaveBeenCalledWith({
      variant: "success",
      title: "Artwork sweep queued",
      body: "starts within 30s",
    });
  });

  it("never throws — run() resolves false on failure", async () => {
    let outcome: boolean | undefined;
    function Once() {
      const action = useAdminAction<[Row], string>(() => Promise.reject(new Error("x")), {
        failure: "nope",
      });
      return (
        <button
          data-testid="go"
          onClick={() => {
            void action.run({ id: "r1" }).then((ok) => (outcome = ok));
          }}
        >
          go
        </button>
      );
    }
    render(<Once />);
    await act(async () => {
      screen.getByTestId("go").click();
    });
    expect(outcome).toBe(false);
  });

  it("keeps pending until the last of several concurrent runs settles", async () => {
    const releases: ((v: string) => void)[] = [];
    const fn = vi.fn(() => new Promise<string>((res) => releases.push(res)));
    render(<Probe fn={fn} opts={{ failure: "nope" }} />);

    await act(async () => {
      screen.getByTestId("run-r1").click();
      screen.getByTestId("run-r2").click();
    });
    expect(screen.getByTestId("pending").textContent).toBe("r2");

    await act(async () => {
      releases[0]("ok");
    });
    expect(screen.getByTestId("pending").textContent).toBe("r2"); // one still open

    await act(async () => {
      releases[1]("ok");
    });
    expect(screen.getByTestId("pending").textContent).toBe("idle");
  });
});
