// The Library section container (spec §3.3): one head, one tab row, and the
// active tab's page in the outlet. The head's sub-line and actions come from
// that page via useSectionHead — see components/shell/sectionHead.tsx.

import { Outlet } from "react-router-dom";
import * as adminApi from "../../api/admin";
import { SectionHeadProvider } from "../../components/shell/sectionHead";
import { LIBRARY_TABS } from "../../components/shell/sectionTabs";
import { useResource } from "../../lib/resource/react";
import { LIBRARY_PROVIDERS } from "./libraryProviders";

/** Slow: these are tab-row counts, not a table. The open tab publishes its own
 *  count over the shared one from its own live read. */
const COUNTS_POLL_MS = 60_000;

/** Source rows on the Sources tab: every registry provider plus the Manual apps
 *  row, which is not one. Must agree with the count SourcesTab publishes when
 *  it is open, or the tab flickers between them. */
const SOURCES_COUNT = LIBRARY_PROVIDERS.length + 1;

/** Every tab carries a count in the mock, not just the open one
 *  (design_handoff_v3 assets/pages-library.js `LIB_TABS`), and a page only
 *  knows its own — so the container reads all four here. */
function useLibraryCounts(): Record<string, number> {
  const res = useResource<Record<string, number>>({
    label: "library counts",
    initialData: { sources: SOURCES_COUNT },
    pollMs: COUNTS_POLL_MS,
    fetch: async (ctx) => {
      const [apps, presets, images] = await Promise.all([
        adminApi.listAdminApps(ctx.token),
        adminApi.listRuntimePresets(ctx.token),
        adminApi.listImages(ctx.token),
      ]);
      return {
        apps: apps.items.length,
        presets: presets.items.length,
        images: images.images.length,
        sources: SOURCES_COUNT,
      };
    },
  });
  return res.data ?? {};
}

export function Library() {
  const counts = useLibraryCounts();

  return (
    <SectionHeadProvider title="Library" tabs={LIBRARY_TABS} counts={counts}>
      <Outlet />
    </SectionHeadProvider>
  );
}
