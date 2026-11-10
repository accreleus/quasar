// A Steam game's own compatibility tool (#1076, approved mock #1067 v2). The first
// editable section on a derived tile's otherwise read-only Runtime tab.

import type { CompatTool } from "../../../../api/types";
import { SelectField } from "../../../../components/TextField";
import { Section } from "./primitives";
import { toolGroups, type CompatChoice } from "./compatTool";

export function CompatToolSection({
  tools,
  choice,
  savedToolId,
  onChange,
  error,
}: {
  /** Null while the catalogue is loading or failed to load. */
  tools: CompatTool[] | null;
  choice: CompatChoice;
  /** The tool the server holds, kept in the list while it is not usable. */
  savedToolId: string;
  onChange: (c: CompatChoice) => void;
  error: string | null;
}) {
  const { installed, valve } = toolGroups(tools ?? [], savedToolId);
  return (
    <Section
      title="Compatibility tool"
      desc="Which Proton this game runs on, for every user and every host."
    >
      {error && (
        <p className="form-error" role="alert">
          {error}
        </p>
      )}
      <div className="ae-compat">
        <SelectField
          label="Compatibility tool"
          value={choice.toolId}
          disabled={!tools}
          onChange={(e) =>
            onChange({ toolId: e.target.value, forced: choice.forced })
          }
        >
          <option value="">Steam&rsquo;s default</option>
          {installed.length > 0 && (
            <optgroup label="Installed tools">
              {installed.map((t) => (
                <option key={t.id} value={t.id}>
                  {t.display_name}
                </option>
              ))}
            </optgroup>
          )}
          {valve.length > 0 && (
            <optgroup label="Valve’s builds">
              {valve.map((t) => (
                <option key={t.id} value={t.id}>
                  {t.display_name}
                </option>
              ))}
            </optgroup>
          )}
        </SelectField>
        <label className="check">
          <input
            type="checkbox"
            checked={choice.forced && !!choice.toolId}
            disabled={!choice.toolId}
            aria-describedby="compat-tool-hint"
            onChange={(e) => onChange({ ...choice, forced: e.target.checked })}
          />{" "}
          Force
        </label>
      </div>
      <span className="field-hint" id="compat-tool-hint">
        Choices: Steam&rsquo;s default, the installed tools, then Valve&rsquo;s
        builds. Force overrides the player&rsquo;s own pick in Steam. Without
        it, the tool is set only when the player hasn&rsquo;t picked one.
      </span>
    </Section>
  );
}
