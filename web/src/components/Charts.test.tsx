import { describe, it, expect } from "vitest";
import { render } from "@testing-library/react";
import { Sparkline, LineChart2 } from "./Charts";
import type { LineSeries2 } from "./Charts";

// jsdom does not implement ResizeObserver — provide a stub
class MockResizeObserver {
  observe() {}
  disconnect() {}
}
globalThis.ResizeObserver = MockResizeObserver as unknown as typeof ResizeObserver;

describe("Sparkline", () => {
  it("renders an svg element", () => {
    const { container } = render(<Sparkline points={[1, 2, 3, 4, 5]} />);
    expect(container.querySelector("svg")).toBeTruthy();
  });

  it("renders nothing meaningful for empty points (< 2)", () => {
    const { container } = render(<Sparkline points={[]} />);
    const svg = container.querySelector("svg");
    expect(svg).toBeTruthy();
    // With no points, no path should be rendered
    expect(container.querySelector("path")).toBeNull();
  });

  it("renders a line path for valid points", () => {
    const { container } = render(
      <Sparkline points={[10, 20, 15, 30, 25]} color="#00c0ff" />,
    );
    const paths = container.querySelectorAll("path");
    // fill path + line path = 2 (fill=true by default)
    expect(paths.length).toBe(2);
  });

  it("renders only a line path when fill=false", () => {
    const { container } = render(
      <Sparkline points={[10, 20, 15]} fill={false} />,
    );
    const paths = container.querySelectorAll("path");
    expect(paths.length).toBe(1);
  });

  it("draws a steady series flat when the scale starts at a baseline", () => {
    const flat = (baseline?: number) => {
      const { container } = render(
        <Sparkline points={[60, 60.5, 60]} height={20} fill={false} baseline={baseline} />,
      );
      const ys = (container.querySelector("path")?.getAttribute("d") ?? "")
        .split(" ")
        .map((seg) => Number(seg.split(",")[1]));
      return Math.max(...ys) - Math.min(...ys);
    };
    // Without one, half a frame of jitter fills the whole box.
    expect(flat()).toBeGreaterThan(10);
    expect(flat(0)).toBeLessThan(1);
  });

  it("respects custom height prop", () => {
    const { container } = render(<Sparkline points={[1, 2, 3]} height={50} />);
    const svg = container.querySelector("svg");
    expect(svg?.getAttribute("height")).toBe("50");
  });
});

describe("LineChart2", () => {
  const series: LineSeries2[] = [
    {
      label: "fps",
      color: "var(--info)",
      points: [
        { x: 0, y: 45 },
        { x: 1000, y: 50 },
        { x: 2000, y: 48 },
      ],
    },
  ];

  it("renders an svg element", () => {
    const { container } = render(<LineChart2 series={series} />);
    expect(container.querySelector("svg")).toBeTruthy();
  });

  it("renders a path for each series", () => {
    const { container } = render(<LineChart2 series={series} />);
    // At least one path for the data line
    expect(container.querySelectorAll("path").length).toBeGreaterThan(0);
  });

  it("renders series label for single series", () => {
    const { getByText } = render(<LineChart2 series={series} />);
    expect(getByText("fps")).toBeTruthy();
  });

  it("renders legend for multi-series", () => {
    const multiSeries: LineSeries2[] = [
      { label: "encode_ms", color: "var(--info)", points: [{ x: 0, y: 2 }] },
      { label: "rtt_ms",    color: "var(--success)", points: [{ x: 0, y: 5 }] },
    ];
    const { getByText } = render(<LineChart2 series={multiSeries} />);
    expect(getByText("encode_ms")).toBeTruthy();
    expect(getByText("rtt_ms")).toBeTruthy();
  });

  it("renders empty without crashing for empty series", () => {
    const { container } = render(<LineChart2 series={[]} />);
    expect(container.querySelector("svg")).toBeTruthy();
  });

  it("respects custom height prop", () => {
    const { container } = render(<LineChart2 series={series} height={200} />);
    const svg = container.querySelector("svg");
    expect(svg?.getAttribute("height")).toBe("200");
  });

  it("renders unit label when provided", () => {
    const { container } = render(<LineChart2 series={series} unit="ms" />);
    // unit appears as a <text> element in the SVG
    const texts = Array.from(container.querySelectorAll("text"));
    expect(texts.some((t) => t.textContent === "ms")).toBe(true);
  });
});
