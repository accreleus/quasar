// The sections of the new design's one account page, in page order, and the
// paths that land on each. The classic tab paths all stay valid.
import type { IconName } from "../../../../components/icons";
import type { RailSection } from "../../../../components/shell/navTypes";

export type OneSectionId = "profile" | "streaming" | "region" | "input" | "overlay" | "appearance" | "devices" | "password" | "storage" | "sessions";

export const ONE_PAGE_SECTIONS: { id: OneSectionId; label: string; sub: string; to: string; icon: IconName }[] = [
  { id: "profile", label: "Profile", sub: "Who you are signed in as.", to: "/app/account/profile", icon: "profile" },
  { id: "streaming", label: "Stream quality", sub: "What a launch from the library asks for.", to: "/app/account/streaming", icon: "streaming" },
  { id: "region", label: "Region", sub: "The clock, language and formats your games and desktops start with. Follows you to every device and applies from your next launch.", to: "/app/account/region", icon: "globe" },
  { id: "input", label: "Input", sub: "Keyboard, mouse and controller. Follows you to every device and applies from your next launch.", to: "/app/account/input", icon: "input" },
  { id: "overlay", label: "In-session overlay", sub: "The strip over a running session. Follows you to every device.", to: "/app/account/overlay", icon: "overlay" },
  { id: "appearance", label: "Appearance", sub: "Which layout the library, game page and account pages use. Follows you to every device.", to: "/app/account/appearance", icon: "overlay" },
  { id: "devices", label: "Devices", sub: "Everything signed in to your account.", to: "/app/account/devices", icon: "devices" },
  { id: "password", label: "Password", sub: "Changing it signs out every device.", to: "/app/account/security", icon: "lock" },
  { id: "storage", label: "Storage", sub: "Save data kept for your apps.", to: "/app/account/storage", icon: "storage" },
  { id: "sessions", label: "My sessions", sub: "What is running under your account now.", to: "/app/account/sessions", icon: "sessions" },
];

export function sectionForPath(path: string): OneSectionId | null {
  const seg = path.replace(/\/+$/, "").split("/")[3];
  if (!seg) return null;
  return ONE_PAGE_SECTIONS.find((s) => s.to.endsWith(`/${seg}`))?.id ?? null;
}

/** The rail lists the sections themselves: each row scrolls the page to one. */
export function onePageRail(): RailSection[] {
  return [
    {
      items: ONE_PAGE_SECTIONS.map((s) => ({
        id: s.id,
        label: s.label,
        icon: s.icon,
        to: s.to,
        match: (p: string) => sectionForPath(p) === s.id || (s.id === "profile" && sectionForPath(p) === null),
      })),
    },
  ];
}
