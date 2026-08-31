/**
 * The head a section container renders, and the hook its tab pages fill in.
 *
 * In v3 a section (Fleet, Library, Streaming, People) owns one head: the h1 is
 * the section name, and the sub-line, the actions and the tab counts belong to
 * whichever tab is showing (handoff §A.4/§A.9/§A.16/§A.18). The container is
 * above the page in the tree, so the page publishes them upward through this
 * context rather than rendering a second head of its own.
 *
 * The provider is a separate component from the container so that publishing
 * cannot loop: `children` arrives as a prop, so re-rendering on a publish
 * leaves the page's element identical and React skips the subtree — the page
 * does not re-render, so it does not publish again. Publishing runs in a
 * Layout effect so a control that lives in the head (a search box) repaints in
 * the same frame as the keystroke that changed it.
 */

import {
  createContext,
  useContext,
  useLayoutEffect,
  useState,
  type ReactNode,
} from "react";
import { Link, useLocation } from "react-router-dom";
import { PageHeader } from "../PageHeader";
import { activeTab, type SectionTab } from "./sectionTabs";

export interface SectionHead {
  /** The tab's sub-line, under the section title. */
  sub?: ReactNode;
  /** Right-aligned action controls for this tab. */
  actions?: ReactNode;
  /** Counts by tab id. A tab with no count renders no `.cnt`. */
  counts?: Record<string, number>;
}

const PublishContext = createContext<((head: SectionHead) => void) | null>(null);

/** Publish this page's sub-line, actions and tab counts to its section head. */
export function useSectionHead(head: SectionHead): void {
  const publish = useContext(PublishContext);
  const { sub, actions, counts } = head;
  useLayoutEffect(() => {
    if (!publish) {
      // A page publishing outside a section container loses its head silently;
      // make the mistake visible in development.
      if (import.meta.env.DEV && !import.meta.env.TEST) console.warn("useSectionHead: no SectionHeadProvider above this page");
      return;
    }
    publish({ sub, actions, counts });
    // A page that unmounts must not leave its actions on the next tab's head.
    return () => publish({});
  }, [publish, sub, actions, counts]);
}

export function SectionHeadProvider({
  title,
  tabs,
  counts: baseCounts,
  children,
}: {
  title: string;
  tabs: SectionTab[];
  /** Counts for tabs whose page is not open. The open page's own count wins:
   *  it is the live one. */
  counts?: Record<string, number>;
  children: ReactNode;
}) {
  const [head, setHead] = useState<SectionHead>({});
  const { pathname } = useLocation();
  const active = activeTab(tabs, pathname);

  return (
    <>
      <PageHeader title={title} sub={head.sub} actions={head.actions} />
      <nav className="tabs" role="tablist" aria-label={`${title} sections`}>
        {tabs.map((tab) => {
          const selected = tab.id === active;
          const count = head.counts?.[tab.id] ?? baseCounts?.[tab.id];
          return (
            <Link
              key={tab.id}
              to={tab.to}
              role="tab"
              aria-selected={selected}
              className={selected ? "tab active" : "tab"}
            >
              {tab.label}
              {count !== undefined && <span className="cnt">{count}</span>}
            </Link>
          );
        })}
      </nav>
      <PublishContext.Provider value={setHead}>{children}</PublishContext.Provider>
    </>
  );
}
