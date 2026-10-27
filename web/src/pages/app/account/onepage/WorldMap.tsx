// The Region section's world map (design_handoff_v3/screens/region-input-v3.html): a dot map on
// which a click picks the zone of the nearest zone.tab point and hovering lights the places that
// share its clock. Pointer only; TimezonePicker's list is the keyboard and screen-reader path.

import { useLayoutEffect, useMemo, useRef, useState } from "react";
import { H, W, cityDots, landPaths, lonLatAt, mapPoints, mapX, mapY, nearest, type MapPoint } from "../../../../lib/region/mapPick";
import { clockIn, formatOffset, type Zone } from "../../../../lib/region/zones";

export interface WorldMapProps {
  zones: readonly Zone[];
  /** The zone in effect (the saved one, or what "Follow my browser" resolves to). */
  selectedTz: string;
  now: Date;
  disabled: boolean;
  onPick: (tz: string) => void;
}

interface Hover {
  point: MapPoint;
  x: number;
  y: number;
}

const LABEL_PAD = 12;
const LABEL_GAP = 24;
const GLYPH_WIDTH = 12.5; // fallback where the browser cannot measure text

export function WorldMap({ zones, selectedTz, now, disabled, onPick }: WorldMapProps) {
  const points = useMemo(() => mapPoints(zones), [zones]);
  const land = useMemo(() => landPaths(points), [points]);
  const dots = useMemo(() => cityDots(points), [points]);
  const [hover, setHover] = useState<Hover | null>(null);
  const svgRef = useRef<SVGSVGElement>(null);
  const textRef = useRef<SVGTextElement>(null);
  const [textWidth, setTextWidth] = useState(0);

  // A saved zone outside the curated list has no point: nothing is marked or lit.
  const sel = points.find((p) => p.zone.tz === selectedTz) ?? null;
  const label = sel ? `${sel.zone.city} · ${clockIn(sel.zone.tz, now) ?? ""}` : "";
  useLayoutEffect(() => {
    setTextWidth(textRef.current?.getComputedTextLength?.() || label.length * GLYPH_WIDTH);
  }, [label]);

  const at = (e: React.PointerEvent | React.MouseEvent) => {
    const r = svgRef.current?.getBoundingClientRect();
    if (!r || r.width === 0 || r.height === 0) return null;
    const { lon, lat } = lonLatAt((e.clientX - r.left) / r.width, (e.clientY - r.top) / r.height);
    return nearest(points, lon, lat);
  };

  const onMove = (e: React.PointerEvent) => {
    const point = at(e);
    const box = e.currentTarget.parentElement?.getBoundingClientRect();
    setHover(point && box ? { point, x: e.clientX - box.left, y: e.clientY - box.top } : null);
  };

  const sx = sel ? mapX(sel.lon) : 0;
  const sy = sel ? mapY(sel.lat) : 0;
  const boxW = textWidth + LABEL_PAD * 2;
  const flip = sx + LABEL_GAP + boxW > W;

  return (
    <div className="tz-map" aria-busy={disabled}>
      <svg
        ref={svgRef}
        viewBox={`0 0 ${W} ${H}`}
        role="img"
        aria-label="World map. Click your area to pick its timezone; the list does the same."
        onPointerMove={onMove}
        onPointerLeave={() => setHover(null)}
        onClick={(e) => {
          const point = disabled ? null : at(e);
          if (point) onPick(point.zone.tz);
        }}
      >
        {[...land].map(([off, d]) => {
          const cls = ["land", hover?.point.zone.offset === off ? "is-hover" : "", sel?.zone.offset === off ? "is-sel" : ""];
          return <path key={off} className={cls.filter(Boolean).join(" ")} d={d} />;
        })}
        {dots.map((p) => (
          <circle key={p.zone.tz} className="city" cx={mapX(p.lon).toFixed(1)} cy={mapY(p.lat).toFixed(1)} r={3} />
        ))}
        {sel && (
          <g className="mk" transform={`translate(${sx.toFixed(1)} ${sy.toFixed(1)})`}>
            <circle className="mk-ring" r={16} />
            <circle className="mk-dot" r={7} />
            <g className="mk-lbl">
              <rect rx={6} height={36} y={-18} width={boxW} x={flip ? -LABEL_GAP - boxW : LABEL_GAP} />
              <text ref={textRef} y={8} x={flip ? -LABEL_GAP - boxW + LABEL_PAD : LABEL_GAP + LABEL_PAD}>{label}</text>
            </g>
          </g>
        )}
      </svg>
      {hover && (
        <div className="tz-tip" style={{ left: hover.x, top: hover.y }}>
          {hover.point.zone.city}, {hover.point.zone.country}
          {hover.point.zone.offset !== null && <span className="num">{formatOffset(hover.point.zone.offset)}</span>}
        </div>
      )}
      <div className="tz-map-cap">
        <span>Click your area on the map, or pick from the list.</span>
        <span>Highlighted: places on the same clock</span>
      </div>
    </div>
  );
}
