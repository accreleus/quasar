// The three account section containers (handoff §A.22): one head per section,
// whose h1 is the section name and whose sub-line comes from the page showing
// in the outlet. Same primitives as the admin section containers.

import { Outlet } from "react-router-dom";
import { SectionHeadProvider } from "../../../components/shell/sectionHead";
import { ACCOUNT_TABS } from "./accountNav";

export function AccountSection() {
  return (
    <SectionHeadProvider title="Account" tabs={ACCOUNT_TABS.account}>
      <Outlet />
    </SectionHeadProvider>
  );
}

export function PreferencesSection() {
  return (
    <SectionHeadProvider title="Preferences" tabs={ACCOUNT_TABS.prefs}>
      <Outlet />
    </SectionHeadProvider>
  );
}

export function UsageSection() {
  return (
    <SectionHeadProvider title="Usage" tabs={ACCOUNT_TABS.usage}>
      <Outlet />
    </SectionHeadProvider>
  );
}
