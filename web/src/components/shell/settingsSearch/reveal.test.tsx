// A pending reveal ends with the next navigation, and a target that never
// appears leaves nothing behind to steal focus later.

import { act, render } from "@testing-library/react";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cancelReveal, revealWhenReady, useCancelRevealOnNavigation } from "./reveal";

let nav!: ReturnType<typeof useNavigate>;
let key = "";
function Probe() {
  nav = useNavigate();
  key = useLocation().key;
  useCancelRevealOnNavigation();
  return <input id="target" />;
}

const mount = () =>
  render(
    <MemoryRouter initialEntries={["/a"]}>
      <Probe />
    </MemoryRouter>,
  );
const target = () => document.getElementById("target");
const wait = (ms: number) => act(async () => void (await vi.advanceTimersByTimeAsync(ms)));

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe("revealWhenReady", () => {
  it("focuses the target once it has stopped moving", async () => {
    mount();
    act(() => {
      nav("/b");
      revealWhenReady({ sel: "#target" }, key, true);
    });
    await wait(1000);
    expect(document.activeElement).toBe(target());
  });

  it("is cancelled by Back before it fires", async () => {
    mount();
    const from = key;
    act(() => {
      nav("/b");
      revealWhenReady({ sel: "#target" }, from, true);
    });
    await wait(60);
    act(() => nav(-1));
    await wait(2000);
    expect(document.activeElement).not.toBe(target());
  });

  it("cancelReveal ends the wait", async () => {
    mount();
    act(() => revealWhenReady({ sel: "#target" }, key, false));
    cancelReveal();
    await wait(2000);
    expect(document.activeElement).not.toBe(target());
  });

  it("without a navigation of its own, the first one cancels", async () => {
    mount();
    act(() => revealWhenReady({ sel: "#target" }, key, false));
    act(() => nav("/b"));
    await wait(2000);
    expect(document.activeElement).not.toBe(target());
  });

  it("a target that never appears stops quietly and does not focus a later arrival", async () => {
    mount();
    const el = target()!;
    el.id = "later";
    act(() => revealWhenReady({ sel: "#target" }, key, false));
    await wait(5000);
    el.id = "target";
    await wait(2000);
    expect(document.activeElement).not.toBe(el);
  });

  it("waits out a disabled control", async () => {
    mount();
    const el = target() as HTMLInputElement;
    el.disabled = true;
    act(() => revealWhenReady({ sel: "#target" }, key, false));
    await wait(1000);
    el.disabled = false;
    await wait(1000);
    expect(document.activeElement).toBe(el);
  });
});
