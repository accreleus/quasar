// useSourcesData — the read side of the Sources tab as one
// composite: settings, library status, the app list, the Steam provider's
// unpublished items and runtime preset, and secrets. One hook so SourcesTab
// and ScanHealth share the same fetches instead of each wiring their own.

import * as adminApi from "../../../api/admin";
import type {
  AdminApp,
  LibraryStatus,
  LibraryUnpublishedItem,
  RuntimePreset,
  SecretStatus,
  SecretsResponse,
} from "../../../api/types";
import { useResource } from "../../../lib/resource/react";
import { LIBRARY_PROVIDERS } from "../libraryProviders";
import { useSettings, type UseSettingsResult } from "../settings/useSettings";
import { manualAppCount, steamCounts, type SteamCounts } from "./sourcesDerived";

const STEAM_PROVIDER = LIBRARY_PROVIDERS.find((p) => p.kind === "steam");
export const STEAM_PROVIDER_KIND = STEAM_PROVIDER?.kind ?? "steam";
export const STEAM_PROVIDER_LABEL = STEAM_PROVIDER?.label ?? "Steam";

export interface UseSourcesDataResult {
  /** Gates the Content sources card: settings, library status and the app list. */
  loading: boolean;
  error: string | null | undefined;
  settings: UseSettingsResult;
  status: LibraryStatus | null;
  refreshStatus: () => void;
  steamApp: AdminApp | null;
  counts: SteamCounts;
  manualCount: number;
  /** From the unpublished-items read, kept separate from `loading`/`error`
   *  above: a failed read must show an error on the Steam row rather than
   *  present a plausible but wrong "0 discovered". */
  pendingCount: number;
  unpublishedLoading: boolean;
  unpublishedError: string | null | undefined;
  preset: RuntimePreset | null;
  presetLoading: boolean;
  secretsLoading: boolean;
  secretsError: string | null | undefined;
  secretsData: SecretsResponse | null;
  refreshSecrets: () => void;
  artworkSecret: SecretStatus | null;
}

export function useSourcesData(artworkSecretName: string): UseSourcesDataResult {
  const settings = useSettings();

  const status = useResource<LibraryStatus | null>(
    { label: "library status", initialData: null, fetch: (ctx) => adminApi.getLibraryStatus(ctx.token) },
    [],
  );

  const apps = useResource<AdminApp[]>(
    {
      label: "apps",
      initialData: [],
      fetch: async (ctx) => (await adminApi.listAdminApps(ctx.token)).items,
    },
    [],
  );
  const appsData = apps.data ?? [];
  const steamApp =
    appsData.find((a) => a.library_provider === STEAM_PROVIDER_KIND && !a.parent_app_id) ?? null;
  const steamAppId = steamApp?.id ?? null;

  const unpublished = useResource<LibraryUnpublishedItem[]>(
    {
      label: "unpublished library items",
      initialData: [],
      fetch: async (ctx) => {
        if (!steamAppId) return [];
        return (await adminApi.listUnpublishedLibraryItems(ctx.token, steamAppId)).items;
      },
    },
    [steamAppId],
  );
  const unpublishedData = unpublished.data ?? [];

  const presetId = steamApp?.runtime_preset_id ?? null;
  const preset = useResource<RuntimePreset | null>(
    {
      label: "steam provider runtime preset",
      initialData: null,
      fetch: async (ctx) => {
        if (!presetId) return null;
        return (await adminApi.getRuntimePreset(ctx.token, presetId)).runtime_preset;
      },
    },
    [presetId],
  );

  const secrets = useResource<SecretsResponse | null>(
    { label: "secrets", initialData: null, fetch: (ctx) => adminApi.listSecrets(ctx.token, ctx.signal) },
    [],
  );
  const artworkSecret = secrets.data?.secrets.find((s) => s.name === artworkSecretName) ?? null;

  return {
    loading: settings.loading || status.loading || apps.loading,
    error: settings.error ?? status.errorMessage ?? apps.errorMessage,
    settings,
    status: status.data ?? null,
    refreshStatus: status.refresh,
    steamApp,
    counts: steamCounts(appsData, unpublishedData, steamAppId),
    manualCount: manualAppCount(appsData),
    pendingCount: unpublishedData.length,
    unpublishedLoading: unpublished.loading,
    unpublishedError: unpublished.errorMessage,
    preset: preset.data ?? null,
    presetLoading: preset.loading,
    secretsLoading: secrets.loading,
    secretsError: secrets.errorMessage,
    secretsData: secrets.data ?? null,
    refreshSecrets: secrets.refresh,
    artworkSecret,
  };
}
