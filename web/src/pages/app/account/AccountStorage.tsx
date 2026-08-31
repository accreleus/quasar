// /app/account/storage — the caller's managed home sizes (handoff §A.22).
//
// No Clear action: there is no self-serve tombstone endpoint, so the mock's
// per-row button and its warning note are omitted (spec §9). App names link to
// the library rather than the admin editor, which a user cannot open.

import { Link } from "react-router-dom";
import { getMyStorage } from "../../../api/storage";
import type { MyStorageItem } from "../../../api/types";
import { Bar } from "../../../components/Bar";
import { ResourceStates } from "../../../components/ResourceStates";
import { Table } from "../../../components/Table";
import type { TableColumn } from "../../../components/Table";
import { useResource } from "../../../lib/resource/react";
import { bytes } from "../../../lib/format/bytes";
import { relativeTime } from "../../../lib/format/relativeTime";
import { useSectionHead } from "../../../components/shell/sectionHead";

export function AccountStorage() {
  useSectionHead({
    sub: "Managed save-data for your apps, on whichever host ran the session.",
  });

  const res = useResource<MyStorageItem[]>({
    label: "your storage",
    fetch: async ({ token, signal }) => (await getMyStorage(token, signal)).items,
    initialData: [],
  });
  const items = res.data ?? [];

  const totalBytes = items.reduce((sum, i) => sum + i.bytes_used, 0);
  const maxBytes = items.length > 0 ? Math.max(...items.map((i) => i.bytes_used)) : 0;

  const columns: TableColumn<MyStorageItem>[] = [
    {
      key: "app",
      header: "App",
      render: (i) => (
        <Link to={`/app/library?q=${encodeURIComponent(i.app_name)}`}>{i.app_name}</Link>
      ),
    },
    {
      key: "used",
      header: "Storage used",
      width: "220px",
      // Bar renders its own `.bar-row` grid, and its default variant is the
      // teal capacity colour the handoff reserves for exactly this.
      render: (i) => (
        <Bar
          percent={maxBytes > 0 ? (i.bytes_used / maxBytes) * 100 : 0}
          label={<span className="num ac-size">{bytes(i.bytes_used)}</span>}
        />
      ),
    },
    {
      key: "last_used",
      header: "Last used",
      width: "150px",
      render: (i) => (
        <span className="sub mono">{i.last_used_at ? relativeTime(i.last_used_at) : "—"}</span>
      ),
    },
  ];

  return (
    <div className="card sec-card">
      <ResourceStates loading={res.loading} error={res.errorMessage} />

      {!res.loading && !res.errorMessage && (
        <>
          <div className="grid g3 mb5">
            <div>
              <div className="eyebrow">Apps with storage</div>
              <div className="num ac-stat">{items.length}</div>
            </div>
            <div>
              <div className="eyebrow">Total used</div>
              <div className="num ac-stat">{bytes(totalBytes)}</div>
            </div>
            <div>
              <div className="eyebrow">Largest app</div>
              <div className="num ac-stat">{bytes(maxBytes)}</div>
            </div>
          </div>

          <Table
            columns={columns}
            rows={items}
            rowKey={(i) => i.app_id}
            empty="No managed storage yet. A home is provisioned the first time you launch an app that keeps save data."
          />
        </>
      )}
    </div>
  );
}
