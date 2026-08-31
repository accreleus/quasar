// Identity tab (handoff §A.10): how the app appears, plus where the tile came
// from. Provenance is read-only by contract, not by choice: `origin` and
// `external_*` are absent from AppWrite, and a relabel would lie to the
// discovery reconciler rather than to a person.

import { Link } from "react-router-dom";
import type { AdminApp, AppKind, LibraryProvider } from "../../../../api/types";
import { SelectField, TextField } from "../../../../components/TextField";
import { Fact, Section } from "./primitives";
import type { AppDraft, DraftErrors } from "./appDraft";
import { withLibraryProvider } from "./appDraft";

interface IdentityTabProps {
  draft: AppDraft;
  onChange: (draft: AppDraft) => void;
  errors: DraftErrors;
  /** null while creating: a new app has no provenance yet. */
  app: AdminApp | null;
  /** The provider app a discovered tile hangs off, once loaded. */
  parent: AdminApp | null;
}

export function IdentityTab({ draft, onChange, errors, app, parent }: IdentityTabProps) {
  return (
    <>
      <Section title="Identity" desc="How the app appears in the library.">
        <div>
          <TextField
            label="Display name"
            value={draft.name}
            onChange={(e) => onChange({ ...draft, name: e.target.value })}
            aria-invalid={!!errors.name}
          />
          {errors.name && <p className="form-error">{errors.name}</p>}
        </div>
        <TextField
          label="Description"
          value={draft.description}
          onChange={(e) => onChange({ ...draft, description: e.target.value })}
        />
        <div className="grid g2">
          <SelectField
            label="Kind"
            value={draft.kind}
            onChange={(e) => onChange({ ...draft, kind: e.target.value as AppKind })}
            hint="Presentation only. It never affects scheduling, streaming or admission."
          >
            <option value="game">Game</option>
            <option value="desktop">Desktop</option>
            <option value="launcher">Launcher</option>
          </SelectField>
          <SelectField
            label="Library provider"
            value={draft.libraryProvider}
            onChange={(e) => onChange(withLibraryProvider(draft, e.target.value as LibraryProvider))}
            hint="Marks this app as a library-discovery source. Steam is the only provider today."
          >
            <option value="">None</option>
            <option value="steam">Steam</option>
          </SelectField>
        </div>
        {draft.libraryProvider === "steam" && (
          <div className="note">
            <div>
              Setting a provider is what triggers discovery, not Kind. Titles found installed under
              this app&rsquo;s managed home are published as their own tiles.
            </div>
          </div>
        )}
      </Section>

      {app && (
        <Section
          title="Provenance"
          desc="Where this tile came from. Read-only, because this is identity, not configuration."
        >
          <div className="ae-facts">
            <Fact label="External reference">
              {app.external_source && app.external_id ? (
                <span className="mono">
                  {app.external_source}:{app.external_id}
                </span>
              ) : (
                <span className="muted">Not a provider title</span>
              )}
            </Fact>
            <Fact label="Origin">
              {app.origin === "discovered" ? "Discovered by a library sync" : "Created by hand"}
            </Fact>
            <Fact label="Parent tile">
              {parent ? (
                <Link to={`/admin/library/apps/${parent.id}`}>{parent.name}</Link>
              ) : (
                <span className="muted">None</span>
              )}
            </Fact>
            <Fact label="Slug">
              <span className="mono">{app.id}</span>
            </Fact>
          </div>
          {parent ? (
            <div className="note">
              <div>
                A discovered tile carries no runtime of its own. Image, arguments, environment and
                mounts are merged from <strong>{parent.name}</strong> at launch, so an edit to the
                parent reaches every tile under it with no re-sync.
              </div>
            </div>
          ) : (
            <span className="hint">
              An external reference lets artwork resolve by id instead of by fuzzy title. Nothing
              else reads it.
            </span>
          )}
        </Section>
      )}
    </>
  );
}
