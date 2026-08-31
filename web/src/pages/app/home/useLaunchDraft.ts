// The detail band's launch selection: the committed one the band reads back and
// the draft the overlay works on, plus everything derived from either.
//
// Draft/commit is what makes Cancel free — opening re-seeds the draft from the
// committed selection and nothing reads the draft once the overlay closes.

import { useCallback, useEffect, useMemo, useState } from "react";
import type { App } from "../../../api/types";
import {
  resolveSelection,
  type LaunchDraft,
  type LaunchOptionsModel,
  type ResolvedSelection,
} from "../launchOptions";
import {
  formatBitrate,
  heal,
  launchSpec,
  optionColumns,
  verdict,
  type EditedColumn,
  type LaunchSpec,
  type OptionColumns,
  type Verdict,
} from "./launchOptionRules";

export interface LaunchDraftState {
  committed: LaunchDraft | null;
  draft: LaunchDraft | null;
  /** null until the profiles resolve. */
  columns: OptionColumns | null;
  /** The draft's verdict, for the overlay's foot. */
  draftVerdict: Verdict;
  /** What the committed selection launches, or null when it resolves to nothing. */
  resolved: ResolvedSelection | null;
  /** The band's strip reads the committed selection, the overlay's head the draft. */
  spec: LaunchSpec;
  draftSpec: LaunchSpec;
  edit: (patch: Partial<LaunchDraft>, column: EditedColumn) => void;
  commit: (next: LaunchDraft) => void;
}

export function useLaunchDraft(
  model: LaunchOptionsModel | null,
  app: App,
  optionsOpen: boolean,
): LaunchDraftState {
  const [committed, setCommitted] = useState<LaunchDraft | null>(null);
  const [draft, setDraft] = useState<LaunchDraft | null>(null);

  // Seed once, on first resolution — a retry re-fetch must not reset a
  // user-adjusted selection. The draft is seeded too, so the overlay is mounted
  // (and `visibility: hidden`, so it holds no tab stops) before it is opened:
  // mounting it and adding `.show` in one frame has nothing to transition from.
  useEffect(() => {
    if (!model) return;
    setCommitted((prev) => prev ?? model.seed);
    setDraft((prev) => prev ?? model.seed);
  }, [model]);

  useEffect(() => {
    if (optionsOpen && committed) setDraft(committed);
  }, [optionsOpen, committed]);

  const edit = useCallback(
    (patch: Partial<LaunchDraft>, column: EditedColumn) => {
      if (!model) return;
      setDraft((d) => (d ? heal(model.space, { ...d, ...patch }, column) : d));
    },
    [model],
  );

  const columns = useMemo(
    () =>
      model && draft
        ? optionColumns(model.space, draft, { pinned: model.pinnedProfileId !== null })
        : null,
    [model, draft],
  );
  const draftVerdict = useMemo<Verdict>(
    () =>
      model && draft
        ? verdict(model.space, draft, { confidence: model.confidence })
        : { tone: "ok", text: "" },
    [model, draft],
  );
  const resolved = useMemo(
    () => (model && committed ? resolveSelection(model.space, committed) : null),
    [model, committed],
  );
  const spec = useMemo(
    () =>
      model && committed
        ? launchSpec(model.space, committed, app.display_stream)
        : {
            resolution: `${app.default_width}×${app.default_height}`,
            fps: `${app.default_fps} fps`,
            bitrate: formatBitrate(app.default_bitrate_kbps),
            codec: "Auto",
          },
    [model, committed, app],
  );
  const draftSpec = useMemo(
    () => (model && draft ? launchSpec(model.space, draft, app.display_stream) : spec),
    [model, draft, app.display_stream, spec],
  );

  return {
    committed,
    draft,
    columns,
    draftVerdict,
    resolved,
    spec,
    draftSpec,
    edit,
    commit: setCommitted,
  };
}
