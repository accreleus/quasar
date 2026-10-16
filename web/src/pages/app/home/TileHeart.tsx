// A resting favourite heart on a grid tile. `tabIndex={-1}` for the same reason
// the tile's Play has it: a second tab stop per tile doubles the cost of walking
// 1,500 tiles. `F` on a focused tile is its keyboard route (useHomeKeys).
// The glyph is a masked ::before and the title is the tile's ::after
// (tileHeart.css), so it adds one element per tile.

import "./tileHeart.css";

export function TileHeart({ name, on, onToggle }: { name: string; on: boolean; onToggle: () => void }) {
  return (
    <button
      type="button"
      className="tile-heart"
      tabIndex={-1}
      aria-pressed={on}
      aria-label={on ? `Remove ${name} from favourites` : `Add ${name} to favourites`}
      title={on ? "Remove from favourites (F)" : "Add to favourites (F)"}
      onClick={onToggle}
    />
  );
}
