/**
 * The rail is the operator's "where am I" and "what needs me" surface, so the
 * two things worth pinning are: exactly one row lights per route, and a badge
 * appears only when it has something to say.
 *
 * The collapse toggle writes a body-level attribute shared by /admin and
 * /app/account, which is why both rails carry it: without it, an admin who
 * collapsed from /admin and then opened their account lands on a collapsed
 * rail with no way back.
 */
import { act, render, screen, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { MemoryRouter } from "react-router-dom";

import { Rail } from "./Rail";
import { ADMIN_NAV } from "../../pages/admin/adminNav";
import { buildAccountSections } from "../../pages/app/account/accountNav";
import { ThemeProvider } from "../../settings/ThemeContext";

function renderRail(
  path = "/admin",
  props: Partial<React.ComponentProps<typeof Rail>> = {},
) {
  return render(
    <ThemeProvider>
      <MemoryRouter initialEntries={[path]}>
        <Rail sections={[{ items: ADMIN_NAV }]} label="Admin sections" {...props} />
      </MemoryRouter>
    </ThemeProvider>,
  );
}

beforeEach(() => {
  localStorage.clear();
  document.body.removeAttribute("data-rail");
});

describe("Rail rows", () => {
  it("renders the eight admin destinations, in order, each with a glyph", () => {
    renderRail();
    const nav = screen.getByRole("navigation", { name: "Admin sections" });
    const links = within(nav).getAllByRole("link");
    expect(links.map((a) => a.textContent)).toEqual([
      "Overview",
      "Sessions",
      "Fleet",
      "Library",
      "Streaming",
      "People",
      "Audit log",
      "Settings",
    ]);
    for (const link of links) {
      expect(link.querySelector("svg")).not.toBeNull();
      // The tooltip is the only label a collapsed row has.
      expect(link).toHaveAttribute("title", link.textContent);
    }
  });

  // A glyph drawn at a different weight or viewBox reads as a different icon
  // set, and a collapsed rail is nothing but glyphs. This keeps a newly added
  // row from arriving at 20x20 or stroke 2 and quietly breaking the column.
  it("draws every glyph in the one shared stroke vocabulary", () => {
    renderRail("/app/account/storage", {
      sections: buildAccountSections(),
      label: "Account sections",
    });
    const svgs = [...screen.getByRole("navigation", { name: "Account sections" }).querySelectorAll("svg")];
    expect(svgs.length).toBe(buildAccountSections().flatMap((s) => s.items).length + 1); // + Collapse
    for (const svg of svgs) {
      expect(svg).toHaveAttribute("viewBox", "0 0 16 16");
      expect(svg).toHaveAttribute("fill", "none");
      expect(svg).toHaveAttribute("stroke", "currentColor");
      expect(svg).toHaveAttribute("stroke-width", "1.5");
      expect(svg).toHaveAttribute("aria-hidden", "true");
    }
  });

  it("lights exactly the matching row, with aria-current, on a drill-down", () => {
    renderRail("/admin/fleet/hosts/abc/settings");
    const current = screen.getAllByRole("link").filter((a) => a.getAttribute("aria-current") === "page");
    expect(current.map((a) => a.textContent)).toEqual(["Fleet"]);
    expect(current[0]).toHaveClass("active");
    // Overview prefixes every admin route; it must not also light.
    expect(screen.getByRole("link", { name: "Overview" })).not.toHaveClass("active");
  });

  it("renders no badge without counts", () => {
    renderRail();
    expect(document.querySelectorAll(".mk")).toHaveLength(0);
  });

  it("renders a badge per kind, and only above zero", () => {
    renderRail("/admin", { badges: { live: 5, fault: 0 } });
    const sessions = screen.getByRole("link", { name: /Sessions/ });
    expect(within(sessions).getByLabelText("5 sessions running now")).toHaveClass("mk-live");
    // A permanent zero is what trains an operator to ignore the badge.
    expect(screen.getByRole("link", { name: "Fleet" }).querySelector(".mk")).toBeNull();
  });

  it("singularises the marker labels", () => {
    renderRail("/admin", { badges: { live: 1, fault: 1 } });
    expect(screen.getByLabelText("1 session running now")).toBeInTheDocument();
    expect(screen.getByLabelText("1 host needs attention")).toBeInTheDocument();
  });

  it("draws the account rail as three untitled rows, lit by section", () => {
    renderRail("/app/account/storage", {
      sections: buildAccountSections(),
      label: "Account sections",
    });
    const nav = screen.getByRole("navigation", { name: "Account sections" });
    expect(nav.querySelectorAll(".rail-sec")).toHaveLength(0);
    expect([...nav.querySelectorAll(".rail-item .lbl")].map((e) => e.textContent)).toEqual([
      "Account",
      "Preferences",
      "Usage",
      "Collapse",
    ]);
    // /storage is a Usage page, so Usage lights even though its `to` is not
    // the current path.
    expect(within(nav).getByRole("link", { name: "Usage" })).toHaveClass("active");
  });

  it("closes a narrow-screen drawer when a row is followed", async () => {
    const onNavigate = vi.fn();
    renderRail("/admin", { onNavigate });
    await act(async () => {
      screen.getByRole("link", { name: "Fleet" }).click();
    });
    expect(onNavigate).toHaveBeenCalled();
  });
});

describe("Rail collapse", () => {
  it("starts expanded and offers the toggle", () => {
    renderRail();
    expect(document.body.getAttribute("data-rail")).toBe("expanded");
    const toggle = screen.getByRole("button", { name: "Collapse" });
    expect(toggle).toHaveAttribute("aria-expanded", "true");
    expect(toggle).toHaveAttribute("title", "Collapse sidebar");
  });

  it("writes the shared body attribute and localStorage, and back again", async () => {
    renderRail();
    // One static "Collapse" label throughout; state rides on aria-expanded,
    // the title and the glyph rotation, not on swapping the text.
    const toggle = screen.getByRole("button", { name: "Collapse" });

    await act(async () => {
      toggle.click();
    });
    expect(document.body.getAttribute("data-rail")).toBe("collapsed");
    expect(localStorage.getItem("quasar-rail")).toBe("collapsed");
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveAttribute("title", "Expand sidebar");

    await act(async () => {
      toggle.click();
    });
    expect(document.body.getAttribute("data-rail")).toBe("expanded");
    expect(localStorage.getItem("quasar-rail")).toBe("expanded");
    expect(toggle).toHaveAttribute("title", "Collapse sidebar");
  });

  it("keeps every row named and glyphed once collapsed", async () => {
    renderRail();
    await act(async () => {
      screen.getByRole("button", { name: "Collapse" }).click();
    });
    for (const item of ADMIN_NAV) {
      // jsdom applies no external sheet, so `.lbl { display: none }` is a
      // visual fact verified against the mock; what matters here is that the
      // row is still a named link with a glyph, not a blank clickable box.
      const link = screen.getByRole("link", { name: item.label });
      expect(link).toHaveAttribute("title", item.label);
      expect(link.querySelector("svg")).not.toBeNull();
    }
  });
});
