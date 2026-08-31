/**
 * UI-03 Shared Primitives I — render tests.
 * Covers: Button, SegmentedControl, TextField/SelectField/TextareaField,
 *         Switch, Checkbox, SearchInput, Chip, LiveDot, TierBadge,
 *         Card, Panel, Stat, StatGrid, Avatar.
 */
import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";

import { Button } from "./Button";
import { SegmentedControl } from "./SegmentedControl";
import { TextField, SelectField, TextareaField, Switch, Checkbox, SearchInput } from "./TextField";
import { Chip, LiveDot } from "./Chip";
import { TierBadge } from "./TierBadge";
import { Card, Panel } from "./Card";
import { Stat, StatGrid } from "./Stat";
import { Avatar } from "./Avatar";

/* ------------------------------------------------------------------ */
/* Button                                                               */
/* ------------------------------------------------------------------ */
describe("Button", () => {
  it("renders with secondary variant by default", () => {
    render(<Button>Click me</Button>);
    const btn = screen.getByRole("button", { name: "Click me" });
    expect(btn).toBeInTheDocument();
    expect(btn.className).toContain("btn");
    // secondary has no extra variant class beyond "btn"
    expect(btn.className).not.toContain("btn-primary");
  });

  it("renders primary variant", () => {
    render(<Button variant="primary">Primary</Button>);
    expect(screen.getByRole("button")).toHaveClass("btn-primary");
  });

  it("renders ghost variant", () => {
    render(<Button variant="ghost">Ghost</Button>);
    expect(screen.getByRole("button")).toHaveClass("btn-ghost");
  });

  it("renders danger variant", () => {
    render(<Button variant="danger">Danger</Button>);
    expect(screen.getByRole("button")).toHaveClass("btn-danger");
  });

  it("renders small size", () => {
    render(<Button size="sm">Small</Button>);
    expect(screen.getByRole("button")).toHaveClass("btn-sm");
  });

  it("renders large size", () => {
    render(<Button size="lg">Large</Button>);
    expect(screen.getByRole("button")).toHaveClass("btn-lg");
  });

  it("renders icon-only with btn-icon class", () => {
    render(<Button iconOnly aria-label="More">•</Button>);
    expect(screen.getByRole("button")).toHaveClass("btn-icon");
  });

  it("fires onClick handler", () => {
    const onClick = vi.fn();
    render(<Button onClick={onClick}>Click</Button>);
    fireEvent.click(screen.getByRole("button"));
    expect(onClick).toHaveBeenCalledTimes(1);
  });

  it("is disabled when disabled prop is set", () => {
    render(<Button disabled>Disabled</Button>);
    expect(screen.getByRole("button")).toBeDisabled();
  });

  it("does not fire onClick when disabled", () => {
    const onClick = vi.fn();
    render(<Button disabled onClick={onClick}>No click</Button>);
    fireEvent.click(screen.getByRole("button"));
    expect(onClick).not.toHaveBeenCalled();
  });
});

/* ------------------------------------------------------------------ */
/* SegmentedControl                                                     */
/* ------------------------------------------------------------------ */
describe("SegmentedControl", () => {
  const options = [
    { value: "a" as const, label: "A" },
    { value: "b" as const, label: "B" },
    { value: "c" as const, label: "C" },
  ];

  it("renders all options", () => {
    render(<SegmentedControl options={options} value="a" onChange={vi.fn()} />);
    expect(screen.getByRole("tab", { name: "A" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "B" })).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "C" })).toBeInTheDocument();
  });

  it("marks the active option as selected", () => {
    render(<SegmentedControl options={options} value="b" onChange={vi.fn()} />);
    expect(screen.getByRole("tab", { name: "B" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("tab", { name: "A" })).toHaveAttribute("aria-selected", "false");
  });

  it("calls onChange with the clicked value", () => {
    const onChange = vi.fn();
    render(<SegmentedControl options={options} value="a" onChange={onChange} />);
    fireEvent.click(screen.getByRole("tab", { name: "C" }));
    expect(onChange).toHaveBeenCalledWith("c");
  });

  /* A tablist is ONE tab stop. Before 2026-08-06 every segment was its own, so
     the five-segment library filter cost five stops between the featured rail
     and the first tile (UX assessment §2.5). */
  it("is a single tab stop — only the selected segment is reachable by Tab", () => {
    const { rerender } = render(
      <SegmentedControl options={options} value="b" onChange={vi.fn()} />,
    );
    const stops = () =>
      screen.getAllByRole("tab").filter((t) => t.getAttribute("tabindex") === "0");
    expect(stops().map((t) => t.textContent)).toEqual(["B"]);

    rerender(<SegmentedControl options={options} value="c" onChange={vi.fn()} />);
    expect(stops().map((t) => t.textContent)).toEqual(["C"]);
  });

  it("keeps a tab stop even when the value matches no segment", () => {
    render(<SegmentedControl options={options} value={"zzz" as "a"} onChange={vi.fn()} />);
    const stops = screen.getAllByRole("tab").filter((t) => t.getAttribute("tabindex") === "0");
    expect(stops.map((t) => t.textContent)).toEqual(["A"]);
  });

  it("moves and selects with the arrow keys, wrapping at both ends", () => {
    const onChange = vi.fn();
    render(<SegmentedControl options={options} value="a" onChange={onChange} />);
    const a = screen.getByRole("tab", { name: "A" });

    fireEvent.keyDown(a, { key: "ArrowRight" });
    expect(onChange).toHaveBeenLastCalledWith("b");
    expect(document.activeElement).toBe(screen.getByRole("tab", { name: "B" }));

    // Wraps backwards off the first segment rather than dead-ending.
    fireEvent.keyDown(a, { key: "ArrowLeft" });
    expect(onChange).toHaveBeenLastCalledWith("c");
    expect(document.activeElement).toBe(screen.getByRole("tab", { name: "C" }));
  });

  it("jumps to the ends with Home and End", () => {
    const onChange = vi.fn();
    render(<SegmentedControl options={options} value="b" onChange={onChange} />);
    const b = screen.getByRole("tab", { name: "B" });

    fireEvent.keyDown(b, { key: "End" });
    expect(onChange).toHaveBeenLastCalledWith("c");

    fireEvent.keyDown(b, { key: "Home" });
    expect(onChange).toHaveBeenLastCalledWith("a");
  });

  /* Manual activation (people/InvitesTab's registration-mode switch): a
     fetch-backed onChange (PUT /v1/admin/settings) must
     never fire from arrow-key navigation alone — only automatic groups do
     that. `activation="manual"` opts a consumer out; default is unchanged,
     covered by the automatic-mode cases above. */
  describe("manual activation", () => {
    it("moves focus without calling onChange on arrow keys", () => {
      const onChange = vi.fn();
      render(
        <SegmentedControl options={options} value="a" onChange={onChange} activation="manual" />,
      );
      const a = screen.getByRole("tab", { name: "A" });

      fireEvent.keyDown(a, { key: "ArrowRight" });
      expect(document.activeElement).toBe(screen.getByRole("tab", { name: "B" }));
      expect(onChange).not.toHaveBeenCalled();

      fireEvent.keyDown(screen.getByRole("tab", { name: "B" }), { key: "ArrowLeft" });
      expect(document.activeElement).toBe(a);
      expect(onChange).not.toHaveBeenCalled();
    });

    it("moves focus without calling onChange on Home/End", () => {
      const onChange = vi.fn();
      render(
        <SegmentedControl options={options} value="b" onChange={onChange} activation="manual" />,
      );
      const b = screen.getByRole("tab", { name: "B" });

      fireEvent.keyDown(b, { key: "End" });
      expect(document.activeElement).toBe(screen.getByRole("tab", { name: "C" }));
      expect(onChange).not.toHaveBeenCalled();

      fireEvent.keyDown(screen.getByRole("tab", { name: "C" }), { key: "Home" });
      expect(document.activeElement).toBe(screen.getByRole("tab", { name: "A" }));
      expect(onChange).not.toHaveBeenCalled();
    });

    it("fires onChange exactly once on Enter for the focused segment", () => {
      const onChange = vi.fn();
      render(
        <SegmentedControl options={options} value="a" onChange={onChange} activation="manual" />,
      );
      const a = screen.getByRole("tab", { name: "A" });

      fireEvent.keyDown(a, { key: "ArrowRight" });
      const b = screen.getByRole("tab", { name: "B" });
      expect(onChange).not.toHaveBeenCalled();

      fireEvent.keyDown(b, { key: "Enter" });
      expect(onChange).toHaveBeenCalledTimes(1);
      expect(onChange).toHaveBeenCalledWith("b");
    });

    it("fires onChange exactly once on Space for the focused segment", () => {
      const onChange = vi.fn();
      render(
        <SegmentedControl options={options} value="a" onChange={onChange} activation="manual" />,
      );
      fireEvent.keyDown(screen.getByRole("tab", { name: "A" }), { key: "ArrowRight" });
      fireEvent.keyDown(screen.getByRole("tab", { name: "B" }), { key: " " });

      expect(onChange).toHaveBeenCalledTimes(1);
      expect(onChange).toHaveBeenCalledWith("b");
    });

    it("still activates on click", () => {
      const onChange = vi.fn();
      render(
        <SegmentedControl options={options} value="a" onChange={onChange} activation="manual" />,
      );
      fireEvent.click(screen.getByRole("tab", { name: "C" }));
      expect(onChange).toHaveBeenCalledWith("c");
    });

    it("keeps aria-selected on the value-selected tab while focus moves elsewhere", () => {
      const onChange = vi.fn();
      render(
        <SegmentedControl options={options} value="a" onChange={onChange} activation="manual" />,
      );
      fireEvent.keyDown(screen.getByRole("tab", { name: "A" }), { key: "ArrowRight" });

      expect(screen.getByRole("tab", { name: "A" })).toHaveAttribute("aria-selected", "true");
      expect(screen.getByRole("tab", { name: "B" })).toHaveAttribute("aria-selected", "false");
      expect(onChange).not.toHaveBeenCalled();
    });

    it("roving tabindex follows focus rather than the selected value", () => {
      const onChange = vi.fn();
      render(
        <SegmentedControl options={options} value="a" onChange={onChange} activation="manual" />,
      );
      fireEvent.keyDown(screen.getByRole("tab", { name: "A" }), { key: "ArrowRight" });

      expect(screen.getByRole("tab", { name: "A" })).toHaveAttribute("tabindex", "-1");
      expect(screen.getByRole("tab", { name: "B" })).toHaveAttribute("tabindex", "0");
    });
  });
});

/* ------------------------------------------------------------------ */
/* TextField / SelectField / TextareaField                              */
/* ------------------------------------------------------------------ */
describe("TextField", () => {
  it("renders label and input", () => {
    render(<TextField label="Username" />);
    expect(screen.getByText("Username")).toBeInTheDocument();
    expect(screen.getByRole("textbox")).toBeInTheDocument();
  });

  it("applies mono class when mono=true", () => {
    render(<TextField label="VRAM" mono />);
    expect(screen.getByRole("textbox")).toHaveClass("mono");
  });

  it("shows hint text", () => {
    render(<TextField label="Name" hint="Required field" />);
    expect(screen.getByText("Required field")).toBeInTheDocument();
  });

  it("passes through disabled", () => {
    render(<TextField label="Name" disabled />);
    expect(screen.getByRole("textbox")).toBeDisabled();
  });
});

describe("SelectField", () => {
  it("renders label and options", () => {
    render(
      <SelectField label="GPU tier">
        <option value="a">Option A</option>
        <option value="b">Option B</option>
      </SelectField>
    );
    expect(screen.getByText("GPU tier")).toBeInTheDocument();
    expect(screen.getByRole("combobox")).toBeInTheDocument();
    expect(screen.getByText("Option A")).toBeInTheDocument();
  });
});

describe("TextareaField", () => {
  it("renders label and textarea", () => {
    render(<TextareaField label="Notes" />);
    expect(screen.getByText("Notes")).toBeInTheDocument();
    expect(screen.getByRole("textbox")).toBeInTheDocument();
  });
});

/* ------------------------------------------------------------------ */
/* Switch                                                               */
/* ------------------------------------------------------------------ */
describe("Switch", () => {
  it("renders checked state", () => {
    render(<Switch checked={true} onChange={vi.fn()} label="Enable" />);
    const checkbox = screen.getByRole("checkbox");
    expect(checkbox).toBeChecked();
  });

  it("renders unchecked state", () => {
    render(<Switch checked={false} onChange={vi.fn()} label="Enable" />);
    expect(screen.getByRole("checkbox")).not.toBeChecked();
  });

  it("calls onChange when toggled", () => {
    const onChange = vi.fn();
    render(<Switch checked={false} onChange={onChange} label="Toggle" />);
    fireEvent.click(screen.getByRole("checkbox"));
    expect(onChange).toHaveBeenCalledWith(true);
  });

  it("is disabled when disabled prop is set", () => {
    render(<Switch checked={false} onChange={vi.fn()} label="Off" disabled />);
    expect(screen.getByRole("checkbox")).toBeDisabled();
  });
});

/* ------------------------------------------------------------------ */
/* Checkbox                                                             */
/* ------------------------------------------------------------------ */
describe("Checkbox", () => {
  it("renders label", () => {
    render(<Checkbox checked={false} onChange={vi.fn()} label="Capture input" />);
    expect(screen.getByText("Capture input")).toBeInTheDocument();
  });

  it("calls onChange when clicked", () => {
    const onChange = vi.fn();
    render(<Checkbox checked={false} onChange={onChange} label="Option" />);
    fireEvent.click(screen.getByRole("checkbox"));
    expect(onChange).toHaveBeenCalledWith(true);
  });
});

/* ------------------------------------------------------------------ */
/* SearchInput                                                          */
/* ------------------------------------------------------------------ */
describe("SearchInput", () => {
  it("renders with default placeholder", () => {
    render(<SearchInput />);
    expect(screen.getByPlaceholderText("Search…")).toBeInTheDocument();
  });

  it("renders with custom placeholder", () => {
    render(<SearchInput placeholder="Search games…" />);
    expect(screen.getByPlaceholderText("Search games…")).toBeInTheDocument();
  });

  it("fires onChange", () => {
    const onChange = vi.fn();
    render(<SearchInput onChange={onChange} />);
    fireEvent.change(screen.getByPlaceholderText("Search…"), { target: { value: "doom" } });
    expect(onChange).toHaveBeenCalled();
  });
});

/* ------------------------------------------------------------------ */
/* Chip                                                                 */
/* ------------------------------------------------------------------ */
describe("Chip", () => {
  it("renders children", () => {
    render(<Chip>Running</Chip>);
    expect(screen.getByText("Running")).toBeInTheDocument();
  });

  it("applies variant classes", () => {
    const { rerender } = render(<Chip variant="success">OK</Chip>);
    expect(screen.getByText("OK").closest("span")).toHaveClass("chip-success");
    rerender(<Chip variant="danger">Fail</Chip>);
    expect(screen.getByText("Fail").closest("span")).toHaveClass("chip-danger");
    rerender(<Chip variant="warning">Warn</Chip>);
    expect(screen.getByText("Warn").closest("span")).toHaveClass("chip-warning");
    rerender(<Chip variant="info">Info</Chip>);
    expect(screen.getByText("Info").closest("span")).toHaveClass("chip-info");
    rerender(<Chip variant="accent">Admin</Chip>);
    expect(screen.getByText("Admin").closest("span")).toHaveClass("chip-accent");
    rerender(<Chip variant="neutral">User</Chip>);
    expect(screen.getByText("User").closest("span")).toHaveClass("chip-neutral");
  });

  it("renders dot when dot=true", () => {
    const { container } = render(<Chip variant="success" dot>Online</Chip>);
    expect(container.querySelector(".dot")).toBeInTheDocument();
  });

  it("does not render dot by default", () => {
    const { container } = render(<Chip variant="success">Running</Chip>);
    expect(container.querySelector(".dot")).not.toBeInTheDocument();
  });
});

/* ------------------------------------------------------------------ */
/* LiveDot                                                              */
/* ------------------------------------------------------------------ */
describe("LiveDot", () => {
  it("renders with default label", () => {
    render(<LiveDot />);
    expect(screen.getByText("Live")).toBeInTheDocument();
  });

  it("renders with custom label", () => {
    render(<LiveDot label="Streaming" />);
    expect(screen.getByText("Streaming")).toBeInTheDocument();
  });
});

/* ------------------------------------------------------------------ */
/* TierBadge                                                            */
/* ------------------------------------------------------------------ */
describe("TierBadge", () => {
  it("renders text", () => {
    render(<TierBadge>1080p · 60</TierBadge>);
    expect(screen.getByText("1080p · 60")).toBeInTheDocument();
  });

  it("applies tier-hi class for hi level", () => {
    render(<TierBadge level="hi">1080p · 60</TierBadge>);
    expect(screen.getByText("1080p · 60")).toHaveClass("tier-hi");
  });

  it("applies tier-low class for low level", () => {
    render(<TierBadge level="low">720p · 30</TierBadge>);
    expect(screen.getByText("720p · 30")).toHaveClass("tier-low");
  });

  it("has no extra class for mid (default)", () => {
    render(<TierBadge>900p · 60</TierBadge>);
    const el = screen.getByText("900p · 60");
    expect(el).toHaveClass("tier");
    expect(el).not.toHaveClass("tier-hi");
    expect(el).not.toHaveClass("tier-low");
  });
});

/* ------------------------------------------------------------------ */
/* Card / Panel                                                         */
/* ------------------------------------------------------------------ */
describe("Card", () => {
  it("renders children with card class", () => {
    const { container } = render(<Card>Card content</Card>);
    expect(container.firstChild).toHaveClass("card");
    expect(screen.getByText("Card content")).toBeInTheDocument();
  });

  it("merges extra classNames", () => {
    const { container } = render(<Card className="card-pad">Content</Card>);
    expect(container.firstChild).toHaveClass("card", "card-pad");
  });
});

describe("Panel", () => {
  it("renders children with panel class", () => {
    const { container } = render(<Panel>Panel content</Panel>);
    expect(container.firstChild).toHaveClass("panel");
    expect(screen.getByText("Panel content")).toBeInTheDocument();
  });
});

/* ------------------------------------------------------------------ */
/* Stat / StatGrid                                                      */
/* ------------------------------------------------------------------ */
describe("Stat", () => {
  it("renders label and value", () => {
    render(<Stat label="FPS" value="59.8" />);
    expect(screen.getByText("FPS")).toBeInTheDocument();
    expect(screen.getByText("59.8")).toBeInTheDocument();
  });

  it("renders unit as <small>", () => {
    const { container } = render(<Stat label="RTT" value="4" unit=" ms" />);
    expect(container.querySelector("small")).toHaveTextContent("ms");
  });

  it("renders meta text", () => {
    render(<Stat label="Bitrate" value="6347" meta="target 8000" />);
    expect(screen.getByText("target 8000")).toBeInTheDocument();
  });

  it("uses .k for label and .v for value", () => {
    const { container } = render(<Stat label="FPS" value="60" />);
    expect(container.querySelector(".k")).toHaveTextContent("FPS");
    expect(container.querySelector(".v")).toHaveTextContent("60");
  });
});

describe("StatGrid", () => {
  it("renders children inside .stat-grid", () => {
    const { container } = render(
      <StatGrid>
        <Stat label="FPS" value="60" />
        <Stat label="RTT" value="4" />
      </StatGrid>
    );
    expect(container.querySelector(".stat-grid")).toBeInTheDocument();
    expect(container.querySelectorAll(".stat")).toHaveLength(2);
  });
});

/* ------------------------------------------------------------------ */
/* Avatar                                                               */
/* ------------------------------------------------------------------ */
describe("Avatar", () => {
  it("renders initials from name", () => {
    render(<Avatar name="Admin User" />);
    expect(screen.getByText("AU")).toBeInTheDocument();
  });

  it("renders single initial for single-word name", () => {
    render(<Avatar name="Quasar" />);
    expect(screen.getByText("Q")).toBeInTheDocument();
  });

  it("renders img when src is provided", () => {
    render(<Avatar name="Test" src="https://example.com/avatar.png" />);
    expect(screen.getByRole("img")).toHaveAttribute("src", "https://example.com/avatar.png");
  });

  it("has aria-label with the name", () => {
    const { container } = render(<Avatar name="Michael" />);
    expect(container.firstChild).toHaveAttribute("aria-label", "Michael");
  });
});
