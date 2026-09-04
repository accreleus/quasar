/**
 * #101: the popover used to be `position: absolute` inside `.row-menu`,
 * which a scrolling `.table-wrap` ancestor clips past the row. These assert
 * the fixed-position replacement: anchored off the trigger button's real
 * rect, flipped above when there's no room below, and torn down on scroll
 * (any ancestor, not just window) and resize.
 */
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";

import { ActionsMenu, type ActionsMenuEntry } from "./ActionsMenu";

beforeAll(() => {
  // .row-menu-pop's `position: fixed` lives in the stylesheet, not inline —
  // load it so getComputedStyle sees it, the same as UserMenu.test.tsx does
  // for `.user-pop`.
  const style = document.createElement("style");
  style.textContent = readFileSync(resolve(__dirname, "../styles/primitives.css"), "utf8");
  document.head.appendChild(style);
});

const items: ActionsMenuEntry[] = [
  { key: "one", label: "First", onClick: vi.fn() },
  { key: "two", label: "Second", onClick: vi.fn() },
];

/** jsdom's layout is always zero; stub the button's rect so positioning has
 *  something real to compute from. */
function stubRect(el: HTMLElement, rect: Partial<DOMRect>) {
  el.getBoundingClientRect = () =>
    ({ top: 0, left: 0, right: 0, bottom: 0, width: 0, height: 0, x: 0, y: 0, ...rect }) as DOMRect;
}

function openMenu(buttonRect: Partial<DOMRect>) {
  render(<ActionsMenu items={items} />);
  const trigger = screen.getByRole("button", { name: "Actions" });
  stubRect(trigger, buttonRect);
  fireEvent.click(trigger);
  return { trigger, pop: screen.getByRole("menu") };
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("ActionsMenu positioning", () => {
  it("opens fixed, anchored below and right-aligned to the button's rect", () => {
    vi.stubGlobal("innerWidth", 1000);
    vi.stubGlobal("innerHeight", 800);
    const { pop } = openMenu({ top: 100, bottom: 128, right: 300 });

    expect(getComputedStyle(pop).position).toBe("fixed");
    expect(pop.style.top).toBe("134px"); // bottom (128) + GAP (6)
    expect(pop.style.right).toBe("700px"); // innerWidth (1000) - rect.right (300)
    expect(pop.style.bottom).toBe("");
  });

  it("flips above the button when there is no room below it", () => {
    vi.stubGlobal("innerWidth", 1000);
    vi.stubGlobal("innerHeight", 400);
    const { pop } = openMenu({ top: 380, bottom: 408, right: 300 });

    expect(getComputedStyle(pop).position).toBe("fixed");
    expect(pop.style.bottom).toBe("26px"); // innerHeight (400) - rect.top (380) + GAP (6)
    expect(pop.style.top).toBe("");
  });

  it("closes on a scroll anywhere in the document (capture, not just window)", () => {
    const { pop } = openMenu({ top: 100, bottom: 128, right: 300 });
    expect(pop).toBeInTheDocument();

    const scrollable = document.createElement("div");
    document.body.appendChild(scrollable);
    fireEvent.scroll(scrollable);

    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  it("closes on window resize", () => {
    openMenu({ top: 100, bottom: 128, right: 300 });
    fireEvent(window, new Event("resize"));
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  it("still closes on outside click", () => {
    const { trigger } = openMenu({ top: 100, bottom: 128, right: 300 });
    void trigger;
    fireEvent.pointerDown(document.body);
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  it("still closes on Escape", () => {
    openMenu({ top: 100, bottom: 128, right: 300 });
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });

  it("still renders every item and fires its onClick", () => {
    const onClick = vi.fn();
    render(
      <ActionsMenu
        items={[{ key: "go", label: "Go", onClick }]}
      />,
    );
    const trigger = screen.getByRole("button", { name: "Actions" });
    stubRect(trigger, { top: 0, bottom: 20, right: 100 });
    fireEvent.click(trigger);
    fireEvent.click(screen.getByRole("menuitem", { name: "Go" }));
    expect(onClick).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  });
});
