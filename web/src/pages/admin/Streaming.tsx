// The Streaming section container (spec §3.3): one head, one tab row, and the
// active tab's page in the outlet. The head's sub-line and actions come from
// that page via useSectionHead — see components/shell/sectionHead.tsx.

import { Outlet } from "react-router-dom";
import { SectionHeadProvider } from "../../components/shell/sectionHead";
import { STREAMING_TABS } from "../../components/shell/sectionTabs";

export function Streaming() {
  return (
    <SectionHeadProvider title="Streaming" tabs={STREAMING_TABS}>
      <Outlet />
    </SectionHeadProvider>
  );
}
