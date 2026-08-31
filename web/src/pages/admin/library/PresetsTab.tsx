// Runtime presets tab — v3 handoff §A.11 (pagePresets()): a reusable
// container configuration (image, env, storage) that apps inherit instead of
// repeating. Distinct from a launch/stream profile (the quality/encode chain).
//
// The mock's Preset table has a GPU column and a "GPU: any/required/optional"
// toolbar select, and the row menu offers "Duplicate preset". RuntimePreset
// (openapi.yaml) carries no gpu flag and no duplicate endpoint exists — both
// are omitted rather than wired to nothing.

import { useCallback, useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import * as adminApi from "../../../api/admin";
import type { RuntimePreset } from "../../../api/types";
import { useAuth } from "../../../auth/context";
import { ActionsMenu, type ActionsMenuItem, type ActionsMenuSeparator } from "../../../components/ActionsMenu";
import { Button } from "../../../components/Button";
import { IconPlus } from "../../../components/icons";
import { Modal } from "../../../components/Modal";
import { ResourceStates } from "../../../components/ResourceStates";
import { SearchInput } from "../../../components/TextField";
import { SegmentedControl } from "../../../components/SegmentedControl";
import { Table, type TableColumn } from "../../../components/Table";
import { useToast } from "../../../components/Toast";
import { useAdminAction } from "../../../lib/resource/action";
import { useResource } from "../../../lib/resource/react";
import { isPresetInUse } from "./presetHelpers";
import { RuntimePresetDrawer } from "./RuntimePresetDrawer";
import { useSectionHead } from "../../../components/shell/sectionHead";

type Segment = "all" | "inUse" | "unused";

export function PresetsTab() {
  const { token } = useAuth();
  const { addToast } = useToast();

  const presets = useResource({
    label: "runtime presets",
    initialData: [] as RuntimePreset[],
    fetch: async (ctx) => (await adminApi.listRuntimePresets(ctx.token)).items,
  });
  const rows = presets.data ?? [];

  const [segment, setSegment] = useState<Segment>("all");
  const [query, setQuery] = useState("");
  const [imageFilter, setImageFilter] = useState("");

  // null = closed, "new" = empty drawer, a RuntimePreset = editing that row.
  const [editorTarget, setEditorTarget] = useState<RuntimePreset | "new" | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<RuntimePreset | null>(null);

  // `?preset=<id>` opens that row's drawer — the app editor's rail and its
  // Runtime tab both link here. The URL IS the open state until the drawer is
  // closed, which is why closing has to clear the param: leaving it set would
  // re-open the drawer on the next render.
  const [searchParams, setSearchParams] = useSearchParams();
  const presetParam = searchParams.get("preset");
  const closeEditor = useCallback(() => {
    setEditorTarget(null);
    if (!presetParam) return;
    const next = new URLSearchParams(searchParams);
    next.delete("preset");
    setSearchParams(next, { replace: true });
  }, [presetParam, searchParams, setSearchParams]);
  const openTarget =
    editorTarget ?? (presetParam ? rows.find((p) => p.id === presetParam) ?? null : null);

  const imageOptions = useMemo(
    () => [...new Set(rows.map((p) => p.image).filter(Boolean))].sort(),
    [rows],
  );

  const inUseCount = useMemo(() => rows.filter((p) => isPresetInUse(p.used_by)).length, [rows]);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    return rows.filter((p) => {
      if (segment === "inUse" && !isPresetInUse(p.used_by)) return false;
      if (segment === "unused" && isPresetInUse(p.used_by)) return false;
      if (imageFilter && p.image !== imageFilter) return false;
      if (q && !p.name.toLowerCase().includes(q)) return false;
      return true;
    });
  }, [rows, segment, imageFilter, query]);

  // The disabled Delete menu item is a UX affordance only; the server's 409
  // while the preset is still in use is the real enforcement (CLAUDE.md
  // invariant #6), and its message is what the danger toast shows.
  const del = useAdminAction<[RuntimePreset], void>(
    (preset) =>
      presets.mutate(
        (ctx) => adminApi.deleteRuntimePreset(ctx.token, preset.id),
        (items) => items.filter((p) => p.id !== preset.id),
      ),
    {
      success: (_result, preset) => `"${preset.name}" deleted`,
      failure: "could not delete runtime preset",
      onSuccess: (_result, preset) => {
        setDeleteTarget(null);
        if (openTarget !== "new" && openTarget?.id === preset.id) closeEditor();
      },
    },
  );

  const columns: TableColumn<RuntimePreset>[] = [
    { key: "name", header: "Preset", render: (p) => <span className="primary">{p.name}</span> },
    { key: "image", header: "Image", render: (p) => <span className="cell-id">{p.image || "—"}</span> },
    {
      key: "env",
      header: "Environment",
      align: "right",
      render: (p) => `${Object.keys(p.env).length} keys`,
    },
    { key: "mounts", header: "Mounts", align: "right", render: (p) => p.mounts.length },
    {
      key: "used",
      header: "Used by",
      align: "right",
      render: (p) => (
        <Link
          to={`/admin/library/apps?preset=${p.id}`}
          onClick={(e) => e.stopPropagation()}
        >
          {p.used_by.length} app{p.used_by.length === 1 ? "" : "s"}
        </Link>
      ),
    },
    {
      key: "menu",
      header: "",
      render: (p) => {
        const inUse = isPresetInUse(p.used_by);
        const items: (ActionsMenuItem | ActionsMenuSeparator)[] = [
          { key: "edit", label: "Edit preset", onClick: () => setEditorTarget(p) },
          { key: "sep", separator: true },
          {
            key: "delete",
            label: "Delete preset",
            variant: "danger",
            disabled: inUse,
            title: inUse ? "In use. Remove it from every app first." : undefined,
            onClick: () => setDeleteTarget(p),
          },
        ];
        return <ActionsMenu label={`Actions for ${p.name}`} items={items} />;
      },
    },
  ];

  // The head is the Library section's (../Library.tsx); this tab fills it in.
  useSectionHead({
    sub: "Shared container configuration an app inherits rather than repeating",
    actions: (
      <Button variant="primary" onClick={() => setEditorTarget("new")}>
        <IconPlus />
        New preset
      </Button>
    ),
    counts: { presets: rows.length },
  });

  return (
    <section className="page">
      <ResourceStates
        loading={presets.loading}
        error={presets.errorMessage}
        isEmpty={rows.length === 0}
        empty="No runtime presets yet. Create one to share container settings across apps."
      />

      {!presets.loading && rows.length > 0 && (
        <>
          <div className="toolbar">
            <SegmentedControl<Segment>
              options={[
                { value: "all", label: <>All <span className="num" style={{ opacity: 0.7 }}>{rows.length}</span></> },
                { value: "inUse", label: <>In use <span className="num" style={{ opacity: 0.7 }}>{inUseCount}</span></> },
                { value: "unused", label: "Unused" },
              ]}
              value={segment}
              onChange={setSegment}
              aria-label="Filter presets"
            />
            <SearchInput
              placeholder="Filter presets"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              aria-label="Filter presets by name"
            />
            <div className="right">
              <select
                className="select"
                value={imageFilter}
                onChange={(e) => setImageFilter(e.target.value)}
                aria-label="Filter by image"
              >
                <option value="">All images</option>
                {imageOptions.map((img) => (
                  <option key={img} value={img}>{img}</option>
                ))}
              </select>
            </div>
          </div>

          <Table
            columns={columns}
            rows={filtered}
            rowKey={(p) => p.id}
            empty={query ? `No presets matching "${query}"` : "No presets match this filter."}
            onRowClick={(p) => setEditorTarget(p)}
          />
        </>
      )}

      {/* Delete confirmation */}
      {deleteTarget && (
        <Modal
          open
          onClose={() => setDeleteTarget(null)}
          title="Delete runtime preset"
          footer={
            <>
              <Button variant="ghost" onClick={() => setDeleteTarget(null)}>Cancel</Button>
              <Button
                variant="danger"
                disabled={del.pending != null}
                onClick={() => void del.run(deleteTarget)}
              >
                {del.pending ? "Deleting…" : "Delete preset"}
              </Button>
            </>
          }
        >
          <p className="sec">
            This permanently removes <strong>{deleteTarget.name}</strong>. This cannot be
            undone. Apps still referencing it must be repointed first. The server refuses
            the delete while any app uses it.
          </p>
        </Modal>
      )}

      {/* Editor drawer */}
      {openTarget && (
        <RuntimePresetDrawer
          preset={openTarget === "new" ? null : openTarget}
          token={token!}
          onClose={closeEditor}
          onSaved={(saved) => {
            // setData also discards any GET already in flight, so a list read
            // that predates this save cannot stomp it back to the old value.
            presets.setData((prev) => {
              const exists = prev.some((p) => p.id === saved.id);
              return exists ? prev.map((p) => (p.id === saved.id ? saved : p)) : [...prev, saved];
            });
            closeEditor();
            addToast({ variant: "success", title: `"${saved.name}" saved` });
          }}
          onRequestDelete={(preset) => setDeleteTarget(preset)}
        />
      )}
    </section>
  );
}
