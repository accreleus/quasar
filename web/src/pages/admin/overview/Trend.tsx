/**
 * The Overview's sparkline: the mock's 74x20 `.spark`, over whatever
 * `./history.ts` has collected.
 *
 * One sample draws no path — a single poll is a level, and a flat line would
 * claim a minute the page never watched. The empty box holds the geometry.
 */

import { Sparkline } from "../../../components/Charts";

/** Matches `.spark` in primitives.css, which sizes the box this fills. */
const SPARK_HEIGHT = 20;

export function Trend({ points, color }: { points: readonly number[]; color: string }) {
  return (
    <div className="spark">
      {points.length >= 2 && (
        <Sparkline points={[...points]} color={color} height={SPARK_HEIGHT} fill={false} baseline={0} />
      )}
    </div>
  );
}
