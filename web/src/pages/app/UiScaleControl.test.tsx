/** Interface-size control — the wp_fractional_scale_v1 preferred_scale hint. */

import { describe, expect, it, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { UiScaleControl, UI_SCALE_STEPS } from "./UiScaleControl";

describe("UiScaleControl", () => {
  it("renders all seven steps", () => {
    render(<UiScaleControl value={1} onChange={vi.fn()} />);
    expect(screen.getAllByRole("option")).toHaveLength(7);
    expect(UI_SCALE_STEPS).toEqual([1, 1.25, 1.5, 1.75, 2, 2.5, 3]);
  });

  it("labels the steps as percentages", () => {
    render(<UiScaleControl value={1} onChange={vi.fn()} />);
    for (const label of ["100%", "125%", "150%", "175%", "200%", "250%", "300%"]) {
      expect(screen.getByRole("option", { name: label })).toBeInTheDocument();
    }
  });

  it("selects the current value", () => {
    render(<UiScaleControl value={1.75} onChange={vi.fn()} />);
    expect((screen.getByRole("combobox") as HTMLSelectElement).value).toBe("1.75");
  });

  it("calls onChange with the picked step", () => {
    const onChange = vi.fn();
    render(<UiScaleControl value={1} onChange={onChange} />);
    fireEvent.change(screen.getByRole("combobox", { name: /interface size/i }), {
      target: { value: "1.50" },
    });
    expect(onChange).toHaveBeenCalledOnce();
    expect(onChange).toHaveBeenCalledWith(1.5);
  });

  it("ignores a value that is not a step", () => {
    const onChange = vi.fn();
    render(<UiScaleControl value={1} onChange={onChange} />);
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "4.00" } });
    expect(onChange).not.toHaveBeenCalled();
  });

  it("disables the select while a change is in flight", () => {
    render(<UiScaleControl value={1} onChange={vi.fn()} busy />);
    expect(screen.getByRole("combobox")).toBeDisabled();
  });

  it("round-trips every step", () => {
    for (const step of UI_SCALE_STEPS) {
      const onChange = vi.fn();
      const { unmount } = render(<UiScaleControl value={step} onChange={onChange} />);
      const select = screen.getByRole("combobox") as HTMLSelectElement;
      expect(parseFloat(select.value)).toBe(step);
      unmount();
    }
  });
});
