// /app/account/profile — who you are, and the password form (handoff §A.22).
//
// Two of the four facts are counts this page does not otherwise need, so they
// are read here: the device list gives both "Devices" and the last sign-in, and
// /v1/me/storage gives the managed-home total.
//
// Last sign-in is the current device's `first_seen_at` — when this device bound
// its token. Not `last_seen_at`: the fetch on this very page refreshes it, so it
// is always "just now" and says nothing.

import { useAuth } from "../../../auth/context";
import { useResource } from "../../../lib/resource/react";
import { listDevices, type Device } from "../../../api/auth";
import { getMyStorage } from "../../../api/storage";
import type { MyStorageItem } from "../../../api/types";
import { bytes } from "../../../lib/format/bytes";
import { relativeTime } from "../../../lib/format/relativeTime";
import { fmtDate } from "../../../lib/formatLegacy";
import { useSectionHead } from "../../../components/shell/sectionHead";
import { PasswordCard } from "./PasswordCard";

function Fact({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="ae-fact">
      <span>{label}</span>
      <span>{value}</span>
    </div>
  );
}

export function AccountProfile() {
  const { user } = useAuth();
  useSectionHead({ sub: "How your account appears across Quasar." });

  // Both reads are facts on this page only; a failure leaves the fact blank
  // rather than blocking the identity card or the password form.
  const devices = useResource<Device[]>({
    label: "your devices",
    fetch: async ({ token, signal }) => (await listDevices(token, signal)).devices,
  });
  const storage = useResource<MyStorageItem[]>({
    label: "your storage",
    fetch: async ({ token, signal }) => (await getMyStorage(token, signal)).items,
  });

  if (!user) return null;
  const initials = user.username.slice(0, 2).toUpperCase();
  const deviceList = devices.data ?? [];
  const thisDevice = deviceList.find((d) => d.current);
  const homeBytes = (storage.data ?? []).reduce((sum, i) => sum + i.bytes_used, 0);

  return (
    <>
      <div className="card sec-card">
        <div className="row gap5 ac-id">
          <span className="u-ava ac-ava">{initials}</span>
          <div className="grow">
            <div className="row gap3 center">
              <h2 className="ac-name">{user.username}</h2>
              <span className="hint">
                {user.role === "admin" ? "Admin" : "User"} · active
              </span>
            </div>
            <div className="sub mono mt2">
              {user.email}
              {user.created_at ? ` · joined ${fmtDate(user.created_at)}` : ""}
            </div>
            <div className="ae-facts mt5 ac-facts">
              <Fact label="Role" value={user.role === "admin" ? "Admin" : "User"} />
              <Fact
                label="Last sign-in"
                value={thisDevice ? relativeTime(thisDevice.first_seen_at) : "—"}
              />
              <Fact label="Devices" value={deviceList.length} />
              <Fact
                label="Managed home"
                value={storage.data ? bytes(homeBytes) : "—"}
              />
            </div>
          </div>
        </div>
      </div>

      <PasswordCard />
    </>
  );
}
