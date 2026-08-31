// Stream profiles tab (v3 handoff §A.17, mockup pageStreamProfiles()): the
// encode rungs, grouped by codec — one codec per rung, chained by launch
// profiles. Authorization is server-enforced; UX only (invariant #6).

import { useState } from "react";
import * as adminApi from "../../../api/admin";
import type { CatalogCodec, StreamProfile } from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { ActionsMenu, type ActionsMenuItem } from "../../../components/ActionsMenu";
import { Button } from "../../../components/Button";
import { Chip } from "../../../components/Chip";
import { IconPlus } from "../../../components/icons";
import { Modal } from "../../../components/Modal";
import { ResourceStates } from "../../../components/ResourceStates";
import { Table, type TableColumn } from "../../../components/Table";
import { useToast } from "../../../components/Toast";
import { useAdminAction } from "../../../lib/resource/action";
import { useResource } from "../../../lib/resource/react";
import { formatMbps } from "./launchProfileHelpers";
import { CODEC_GROUPS, CODEC_GROUP_LABEL, CODEC_GROUP_NOTE, codecFallbackLabel, deleteDisabledTitle, groupByCodec, isStreamProfileInUse } from "./streamProfileHelpers";
import { StreamProfileDrawer } from "./StreamProfileDrawer";
import { useSectionHead } from "../../../components/shell/sectionHead";

function browserChip(support: StreamProfile["browser_client"]) {
  if (support === "recommended") return <Chip variant="success">Recommended</Chip>;
  if (support === "risky") return <Chip variant="warning">Risky</Chip>;
  return <Chip variant="neutral">Supported</Chip>;
}

function CodecCard({
  codec,
  profiles,
  onEdit,
  onDeleteClick,
}: {
  codec: CatalogCodec;
  profiles: StreamProfile[];
  onEdit: (p: StreamProfile) => void;
  onDeleteClick: (p: StreamProfile) => void;
}) {
  const columns: TableColumn<StreamProfile>[] = [
    { key: "name", header: "Profile", render: (p) => <span className="mono primary">{p.display_name}</span> },
    { key: "res", header: "Resolution", render: (p) => <span className="mono">{p.width}×{p.height}</span> },
    { key: "fps", header: "FPS", align: "right", render: (p) => <span className="mono">{p.fps}</span> },
    {
      key: "br",
      header: "Bitrate",
      align: "right",
      render: (p) => <span className="mono">{formatMbps(p.nominal_bitrate_kbps)} Mb/s</span>,
    },
    {
      key: "floor",
      header: "ABR floor",
      align: "right",
      render: (p) => <span className="mono" style={{ color: "var(--text-3)" }}>{formatMbps(p.abr_floor_kbps)} Mb/s</span>,
    },
    {
      key: "enc",
      header: "Encoder",
      render: (p) => (p.hardware_encoder_required ? "Hardware encoder" : "Universal"),
    },
    { key: "browser", header: "Browser", render: (p) => browserChip(p.browser_client) },
    {
      key: "used",
      header: "Used by",
      align: "right",
      render: (p) => {
        const n = p.used_by?.length ?? 0;
        const sessions = p.session_count ?? 0;
        return `${n} profile${n === 1 ? "" : "s"}${sessions > 0 ? ` · ${sessions} session${sessions === 1 ? "" : "s"}` : ""}`;
      },
    },
    {
      key: "menu",
      header: "",
      render: (p) => {
        const inUse = isStreamProfileInUse(p);
        const items: ActionsMenuItem[] = [
          { key: "edit", label: "Edit", onClick: () => onEdit(p) },
          {
            key: "delete",
            label: "Delete",
            variant: "danger",
            onClick: () => onDeleteClick(p),
            disabled: inUse,
            title: inUse ? deleteDisabledTitle(p.session_count ?? 0) : undefined,
          },
        ];
        return <ActionsMenu label={`Actions for ${p.display_name}`} items={items} />;
      },
    },
  ];

  return (
    <div className="card">
      <div className="panel-head">
        <span className="panel-title">{CODEC_GROUP_LABEL[codec]}</span>
        <div className="acts">
          <Chip variant={codec === "h264" ? undefined : "accent"}>{codecFallbackLabel(codec)}</Chip>
          <Chip>{profiles.length} rung{profiles.length === 1 ? "" : "s"}</Chip>
        </div>
      </div>
      <p className="hint" style={{ padding: "0 var(--card-pad)", marginTop: "var(--s3)" }}>
        {CODEC_GROUP_NOTE[codec]}
      </p>
      <div style={{ padding: "var(--s3) 0 0" }}>
        <Table columns={columns} rows={profiles} rowKey={(p) => p.id} />
      </div>
    </div>
  );
}

export function ProfilesTab() {
  const { token } = useAuth();
  const { addToast } = useToast();

  const resource = useResource({
    label: "stream profiles",
    initialData: [] as StreamProfile[],
    fetch: async (ctx) => (await adminApi.listStreamProfiles(ctx.token)).items,
  });
  const rows = resource.data ?? [];

  // null = closed, "new" = empty drawer, a StreamProfile = editing that row.
  const [editorTarget, setEditorTarget] = useState<StreamProfile | "new" | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<StreamProfile | null>(null);

  const del = useAdminAction<[StreamProfile], void>(
    (profile) =>
      resource.mutate(
        (ctx) => adminApi.deleteStreamProfile(ctx.token, profile.id),
        (items) => items.filter((p) => p.id !== profile.id),
      ),
    {
      success: (_result, profile) => `"${profile.display_name}" deleted`,
      failure: "could not delete stream profile",
      onSuccess: (_result, profile) => {
        setDeleteTarget(null);
        if (editorTarget !== "new" && editorTarget?.id === profile.id) setEditorTarget(null);
      },
    },
  );

  const groups = groupByCodec(rows);

  // The head is the Streaming section's (../Streaming.tsx); this tab fills it in.
  useSectionHead({
    sub: "The encode rungs themselves, grouped by codec",
    actions: (
      <Button variant="primary" onClick={() => setEditorTarget("new")}>
        <IconPlus />
        New stream profile
      </Button>
    ),
    counts: { profiles: rows.length },
  });

  return (
    <section className="page">
      <ResourceStates
        loading={resource.loading}
        error={resource.errorMessage}
        isEmpty={rows.length === 0}
        empty="No stream profiles yet. Create one to start building a launch profile."
      />

      {!resource.loading && CODEC_GROUPS.map((codec) => {
        const group = groups.get(codec) ?? [];
        if (group.length === 0) return null;
        return (
          <CodecCard
            key={codec}
            codec={codec}
            profiles={group}
            onEdit={(p) => setEditorTarget(p)}
            onDeleteClick={(p) => setDeleteTarget(p)}
          />
        );
      })}

      {/* Delete confirmation */}
      {deleteTarget && (
        <Modal
          open
          onClose={() => setDeleteTarget(null)}
          title="Delete stream profile"
          footer={
            <>
              <Button variant="ghost" onClick={() => setDeleteTarget(null)}>Cancel</Button>
              <Button variant="danger" disabled={del.pending != null} onClick={() => void del.run(deleteTarget)}>
                {del.pending ? "Deleting…" : "Delete profile"}
              </Button>
            </>
          }
        >
          <p className="sec">
            This permanently removes <strong>{deleteTarget.display_name}</strong>. This cannot be
            undone. Launch profiles still listing it must drop it first. The server refuses the
            delete while any do.
          </p>
        </Modal>
      )}

      {/* Editor drawer */}
      {editorTarget && (
        <StreamProfileDrawer
          profile={editorTarget === "new" ? null : editorTarget}
          token={token!}
          onClose={() => setEditorTarget(null)}
          onSaved={(saved) => {
            resource.setData((prev) => {
              const exists = prev.some((p) => p.id === saved.id);
              // Carry used_by/session_count forward: the write response is the
              // bare object, not the list projection — a straight replace
              // blanked both and re-enabled Delete on an undeletable rung.
              // Neither can change by editing the rung itself.
              return exists
                ? prev.map((p) =>
                    p.id === saved.id
                      ? { ...saved, used_by: p.used_by, session_count: p.session_count }
                      : p,
                  )
                : [...prev, saved];
            });
            setEditorTarget(null);
            addToast({ variant: "success", title: `"${saved.display_name}" saved` });
          }}
          onRequestDelete={(p) => setDeleteTarget(p)}
        />
      )}
    </section>
  );
}
