// The Home page's structure comes from the server; this file is the client's
// half of the rules. semantics: control-api.md §Home layout
import { getHomeLayout, type HomeLayout, type HomeSection } from "../../../api/home";

export type HomeKind = "highlights" | "jump_back_in" | "favourites";

export const HOME_KIND_LABEL: Record<HomeKind, string> = {
  highlights: "Highlights",
  jump_back_in: "Jump back in",
  favourites: "Favourites",
};

const isKnown = (kind: string): kind is HomeKind => Object.hasOwn(HOME_KIND_LABEL, kind);

/** What an older server (404) implies: today's order. Same list as the server's built-in. */
export const BUILTIN_HOME: HomeLayout = {
  source: "builtin",
  sections: (Object.keys(HOME_KIND_LABEL) as HomeKind[]).map((kind) => ({
    id: kind,
    kind,
    title: HOME_KIND_LABEL[kind],
    enabled: true,
    params: {},
  })),
};

/** The caller's layout, or the built-in one on 404 (an older server) or any other failure. */
export async function fetchHomeLayout(token: string, signal: AbortSignal): Promise<HomeLayout> {
  try {
    const layout = await getHomeLayout(token, "/me/home", signal);
    return Array.isArray(layout?.sections) ? layout : BUILTIN_HOME;
  } catch (e) {
    if (signal.aborted) throw e;
    return BUILTIN_HOME;
  }
}

/** The sections to render, in order: enabled ones of a kind this client knows. */
export function shownSections(layout: HomeLayout): (HomeSection & { kind: HomeKind })[] {
  return layout.sections.filter((s): s is HomeSection & { kind: HomeKind } => s.enabled && isKnown(s.kind));
}
