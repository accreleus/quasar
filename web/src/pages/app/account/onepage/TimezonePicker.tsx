// The Region section's timezone list: a search box over a listbox whose first row is
// "Follow my browser". Arrow keys move the highlight and Enter picks, so a keyboard user
// does not save (and close an idle Steam) once per arrow press.

import { Fragment, useEffect, useMemo, useRef, useState } from "react";
import { IconSearch } from "../../../../components/icons";
import { formatOffset, searchZones, type Zone } from "../../../../lib/region/zones";

export interface TimezonePickerProps {
  zones: readonly Zone[];
  /** The saved zone; null is "Follow my browser". */
  value: string | null;
  /** The zone "Follow my browser" resolves to now. */
  followed: Zone;
  disabled: boolean;
  onPick: (tz: string | null) => void;
}

const optionId = (z: Zone | null) => (z ? `tz-o-${z.tz.replace(/\W/g, "_")}` : "tz-o-follow");

export function TimezonePicker({ zones, value, followed, disabled, onPick }: TimezonePickerProps) {
  const [term, setTerm] = useState("");
  const [activeIdx, setActiveIdx] = useState<number | null>(null);
  const listRef = useRef<HTMLUListElement>(null);

  const options = useMemo<(Zone | null)[]>(() => [null, ...searchZones(zones, term)], [zones, term]);
  const selectedIdx = options.findIndex((o) => (o ? o.tz === value : value === null));
  const active = Math.min(activeIdx ?? Math.max(selectedIdx, 0), options.length - 1);

  // Open the list on the saved zone; scrollIntoView would move the page too.
  useEffect(() => {
    const list = listRef.current;
    const el = selectedIdx > 0 ? document.getElementById(optionId(options[selectedIdx] ?? null)) : null;
    if (list && el) list.scrollTop = el.offsetTop - list.clientHeight / 2 + el.offsetHeight / 2;
  }, []);

  useEffect(() => {
    if (activeIdx !== null) document.getElementById(optionId(options[active] ?? null))?.scrollIntoView?.({ block: "nearest" });
  }, [activeIdx, active, options]);

  const onKeyDown = (e: React.KeyboardEvent) => {
    const last = options.length - 1;
    const next = { ArrowDown: Math.min(last, active + 1), ArrowUp: Math.max(0, active - 1), Home: 0, End: last }[e.key];
    if (next !== undefined) {
      e.preventDefault();
      setActiveIdx(next);
    } else if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      onPick(options[active]?.tz ?? null);
    }
  };

  const hits = options.length - 1;
  return (
    <div className="tz-side">
      <div className="search">
        <IconSearch />
        <input
          type="search"
          placeholder="Search a city, country or zone"
          aria-label="Search timezones"
          aria-controls="tz-list"
          autoComplete="off"
          value={term}
          onChange={(e) => {
            setTerm(e.target.value);
            setActiveIdx(null);
            if (listRef.current) listRef.current.scrollTop = 0;
          }}
        />
      </div>
      <ul
        id="tz-list"
        ref={listRef}
        className="tz-list"
        role="listbox"
        aria-labelledby="tz-label"
        aria-busy={disabled}
        aria-activedescendant={optionId(options[active] ?? null)}
        tabIndex={0}
        onKeyDown={disabled ? undefined : onKeyDown}
      >
        {options.map((z, i) => {
          const zone = z ?? followed;
          const sub = z ? `${z.country ? `${z.country} · ` : ""}${z.tz}` : `Now ${followed.city} (${followed.tz})`;
          const classes = ["tz-opt", z ? "" : "tz-follow", i === active && activeIdx !== null ? "is-active" : ""];
          return (
            <Fragment key={z?.tz ?? "follow"}>
              <li
                id={optionId(z)}
                role="option"
                aria-selected={z ? z.tz === value : value === null}
                className={classes.filter(Boolean).join(" ")}
                onClick={disabled ? undefined : () => onPick(z?.tz ?? null)}
              >
                <span className="tz-t">
                  <span className="tz-name">{z ? z.city : "Follow my browser"}</span>
                  <span className="tz-sub" title={sub}>{sub}</span>
                </span>
                <span className="tz-off">{formatOffset(zone.offset)}</span>
              </li>
              {!z && <li className="tz-sep" role="presentation" />}
            </Fragment>
          );
        })}
        {hits === 0 && <li className="tz-empty" role="presentation">No timezone matches. Try a city or country name.</li>}
      </ul>
    </div>
  );
}
