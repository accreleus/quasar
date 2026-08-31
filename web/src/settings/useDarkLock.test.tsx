/**
 * The dark lock and the streaming hint are two different claims. Locking dark
 * says "this surface renders dark whatever the theme"; data-streaming says
 * "live video is on screen, cheapen the compositing". The pre-auth card makes
 * only the first claim, so the hint must be opt-out — otherwise every sign-in
 * page tells the whole app's CSS a stream is playing.
 */

import { describe, expect, it } from "vitest";
import { render } from "@testing-library/react";
import { ThemeProvider, useDarkLock } from "./ThemeContext";

function Locker({ streaming }: { streaming?: boolean }) {
  useDarkLock(streaming === undefined ? undefined : { streaming });
  return <div>locked</div>;
}

const flag = () => document.documentElement.getAttribute("data-streaming");

describe("useDarkLock", () => {
  it("sets the streaming hint by default, and clears it on unmount", () => {
    const { unmount } = render(
      <ThemeProvider>
        <Locker />
      </ThemeProvider>,
    );
    expect(flag()).toBe("true");

    unmount();
    expect(flag()).toBeNull();
  });

  it("locks dark without the streaming hint when asked", () => {
    render(
      <ThemeProvider>
        <Locker streaming={false} />
      </ThemeProvider>,
    );
    expect(document.documentElement).toHaveAttribute("data-theme", "dark");
    expect(flag()).toBeNull();
  });
});
