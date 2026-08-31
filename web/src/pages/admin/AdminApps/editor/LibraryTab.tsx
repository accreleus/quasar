// Library tab (handoff §A.10, spec §8.2/§8.4). Shown only on a provider app:
// the routes are keyed on the provider's app id, not a tile's, and a derived
// tile can never be a provider (apps_derived_shape_ck).
//
// Not a review queue (spec §8.2): discovery is fully correct for an operator
// who never opens this, and the copy must not imply "pending approval".
//
// The list is loaded by the page, because the tab bar carries its count.

import * as adminApi from "../../../../api/admin";
import type { LibrarySuppressedBy, LibraryUnpublishedItem } from "../../../../api/types";
import { Button } from "../../../../components/Button";
import { ResourceStates } from "../../../../components/ResourceStates";
import { useAdminAction } from "../../../../lib/resource/action";
import { fmtDate } from "../../../../lib/formatLegacy";
import { Section } from "./primitives";

const SUPPRESSED_BY_LABEL: Record<LibrarySuppressedBy, string> = {
  rule_ignore: "Ignored by an admin",
  builtin_appid: "Built-in denylist (this appid)",
  builtin_prefix: "Built-in denylist (name looks like a Steam runtime/tool)",
  appdetails: "The third-party appdetails lookup says this isn't a game",
  // As uncertain as the backend states it (spec §8.2: "Don't invent a more
  // confident label") — the database cannot say why no live tile exists.
  other: "Not published, reason unclear (most likely an admin disabled the tile by hand)",
};

interface LibraryTabProps {
  appId: string;
  token: string;
  items: LibraryUnpublishedItem[];
  loading: boolean;
  error: string | null;
  reload: () => void;
}

export function LibraryTab({ appId, token, items, loading, error, reload }: LibraryTabProps) {
  const unIgnore = useAdminAction(
    async (item: LibraryUnpublishedItem) => {
      await adminApi.setLibraryRule(token, appId, item.external_id, {
        rule: "allow",
        external_source: item.external_source,
      });
      // §8.2: `allow` writes only the rule row. The reconciler publishes the
      // tile on the next scan, so the item stays in this list until then.
      reload();
    },
    {
      success: (_r, item) => `"${item.name || item.external_id}" will publish on the next scan`,
      failure: "could not write the allow rule",
    },
  );

  return (
    <Section
      title="Library"
      desc="Steam appids this instance has seen installed for at least one user that have no live library tile. A read and a button, not a review queue, and discovery works correctly whether or not it is ever opened."
    >
      <ResourceStates loading={loading} error={error} />
      {!loading && !error && (
        <>
          <div className="ae-list" style={{ maxWidth: 640 }}>
            {items.length === 0 ? (
              <span className="hint">
                Nothing suppressed right now. Every appid this instance has observed is either
                published or has never been scanned.
              </span>
            ) : (
              items.map((item) => (
                <div key={`${item.external_source}:${item.external_id}`} className="ae-item">
                  <div>
                    <div className="ae-item-t">{item.name || `Appid ${item.external_id}`}</div>
                    <div className="ae-item-m mono">
                      {item.external_id} · {SUPPRESSED_BY_LABEL[item.suppressed_by]} · {item.users}{" "}
                      user{item.users === 1 ? "" : "s"} · last seen {fmtDate(item.last_seen_at)}
                      {item.has_tile && " · a disabled tile already exists"}
                    </div>
                  </div>
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={unIgnore.pending?.[0].external_id === item.external_id}
                    onClick={() => void unIgnore.run(item)}
                  >
                    {unIgnore.pending?.[0].external_id === item.external_id
                      ? "Un-ignoring…"
                      : "Un-ignore"}
                  </Button>
                </div>
              ))
            )}
          </div>
          <div className="note">
            <div>
              Suppression is a hide, never a delete. An ignored appid keeps its tile, its artwork
              and every user&rsquo;s favourite of it. Un-ignoring republishes it to every user who
              has it installed.
            </div>
          </div>
        </>
      )}
    </Section>
  );
}
