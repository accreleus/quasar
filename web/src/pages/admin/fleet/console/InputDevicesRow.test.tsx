import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { InputDevicesRow } from "./InputDevicesRow";

const LONG = "/dev/input/by-id/usb-Microsoft_Controller_3039363431313732383333303233-event-joystick";

describe("InputDevicesRow (#436)", () => {
  // A by-id path is longer than the card is wide. Table cells never wrap (DESIGN.md
  // Density), so the path truncates in its own cell and the whole path stays one hover away.
  it("truncates a long device path in its cell and keeps the whole path as its tooltip", () => {
    render(
      <InputDevicesRow
        value={[LONG]}
        devices={[{ path: LONG, label: "Microsoft X-Box Series S|X Controller" }]}
        onChange={() => {}}
      />,
    );
    const path = screen.getByText(LONG);
    expect(path.getAttribute("title")).toBe(LONG);
    expect(path.classList.contains("idev-path")).toBe(true);
    expect(path.closest("td")?.classList.contains("idev-path-cell")).toBe(true);
  });
});
