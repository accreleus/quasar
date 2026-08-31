/**
 * Avatar / MonoTile — user avatar or app icon tile.
 * Matches the `.mono-tile` pattern from the design system.
 */

interface AvatarProps {
  /** Display name — used to derive initials */
  name: string;
  /** Optional image URL */
  src?: string;
  size?: "sm" | "md" | "lg";
}

function initials(name: string): string {
  return name
    .split(/\s+/)
    .slice(0, 2)
    .map((w) => w[0]?.toUpperCase() ?? "")
    .join("");
}

const SIZE_PX: Record<string, number> = { sm: 28, md: 36, lg: 48 };

export function Avatar({ name, src, size = "md" }: AvatarProps) {
  const px = SIZE_PX[size] ?? 36;
  return (
    <div
      className="mono-tile"
      title={name}
      aria-label={name}
      style={{ width: px, height: px, fontSize: px * 0.36 }}
    >
      {src ? (
        <img src={src} alt={name} style={{ width: "100%", height: "100%", objectFit: "cover", borderRadius: "inherit" }} />
      ) : (
        initials(name)
      )}
    </div>
  );
}
