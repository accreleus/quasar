// The band's hero art and the palette sampled from it. Sampling runs on load
// and on a cached image that is already complete, since `onLoad` never fires
// for one served from cache.

import { useCallback, useEffect, useRef, useState } from "react";
import type { CSSProperties, RefObject } from "react";
import type { App } from "../../../api/types";
import { artFor } from "../../../lib/appArtwork";
import { DEFAULT_HERO_PALETTE, samplePalette, type HeroPalette } from "../../../lib/heroPalette";

export interface HeroArt {
  /** The hero image URL, or null when the app has none and the glyph shows. */
  art: ReturnType<typeof artFor>;
  imgRef: RefObject<HTMLImageElement | null>;
  onLoad: () => void;
  /** The band's scrim and accent. The interior ink stays pinned light in both
   *  themes (home.css); only these two follow the art. */
  style: CSSProperties;
}

export function useHeroPalette(app: App): HeroArt {
  const art = artFor(app, "hero");
  const imgRef = useRef<HTMLImageElement>(null);
  const [palette, setPalette] = useState<HeroPalette>(DEFAULT_HERO_PALETTE);

  useEffect(() => {
    if (!art) {
      setPalette(DEFAULT_HERO_PALETTE);
      return;
    }
    const img = imgRef.current;
    if (img && img.complete && img.naturalWidth > 0) setPalette(samplePalette(img));
  }, [art]);

  const onLoad = useCallback(() => {
    if (imgRef.current) setPalette(samplePalette(imgRef.current));
  }, []);

  return {
    art,
    imgRef,
    onLoad,
    style: {
      "--scrim-rgb": palette.scrimRgb,
      "--accent-rgb": palette.accentRgb,
    } as CSSProperties,
  };
}
