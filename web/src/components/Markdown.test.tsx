// Release notes are a third party's text rendered in an admin's session, so
// the sanitising path is pinned here rather than assumed.

import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { Markdown } from "./Markdown";

describe("Markdown", () => {
  it("renders a script tag as text and never as a script element", () => {
    const { container } = render(
      <Markdown>{`### Fixed\n\n<script>window.__pwned = true</script>\n\n- a thing\n`}</Markdown>,
    );

    expect(container.querySelector("script")).toBeNull();
    expect((window as unknown as { __pwned?: boolean }).__pwned).toBeUndefined();
    // The heading and list still render, so sanitising did not eat the notes.
    expect(screen.getByRole("heading", { name: "Fixed" })).toBeInTheDocument();
    expect(screen.getByText("a thing")).toBeInTheDocument();
  });

  it("strips an inline event handler and a javascript: link", () => {
    const { container } = render(
      <Markdown>{`<img src=x onerror="window.__pwned = true">\n\n[click](javascript:alert(1))\n`}</Markdown>,
    );

    expect(container.querySelector("img[onerror]")).toBeNull();
    const link = container.querySelector("a");
    expect(link?.getAttribute("href") ?? "").not.toContain("javascript:");
  });

  it("opens an upstream link away from the console without a window handle", () => {
    const { container } = render(<Markdown>{`[release](https://github.com/x/y)`}</Markdown>);
    const link = container.querySelector("a");
    expect(link).toHaveAttribute("target", "_blank");
    expect(link?.getAttribute("rel") ?? "").toContain("noopener");
  });
});
