// The line under a booting launch or quick switch. Steam's own shader figure is
// shown only when it is above 0: on a long replay it is real, but Steam often
// logs a single "0/N" line and starts the game while it replays in the background.
// Otherwise a short retro-game nod rotates, picked at random.
import { useEffect, useState } from "react";

export const GETTING_READY_PHRASES = [
  "Blowing on the cartridge…",
  "Inserting coin…",
  "Warping to world 8…",
  "Pressing start…",
  "Saving the princess…",
  "Collecting rings…",
  "Rolling a barrel…",
  "Finding the secret level…",
  "Loading the next stage…",
  "Charging the buster…",
  "Stomping goombas…",
  "Eating power pellets…",
  "Dodging space invaders…",
  "Warming up the arcade…",
  "Rewinding the tape…",
  "Checking the high scores…",
  "Collecting 100 coins…",
  "Taking the warp pipe…",
  "Finding the last star…",
  "Stacking the falling blocks…",
] as const;

export const GETTING_READY_MS = 3_000;

const pick = (not?: string) => {
  const pool = GETTING_READY_PHRASES.filter((p) => p !== not);
  return pool[Math.floor(Math.random() * pool.length)];
};

/** The line for a booting app: "" when not booting. Not a live region: callers
 *  mark it aria-live="off" (the phrase also aria-hidden). */
export function useBootLine(booting: boolean, shaderPercent: number | null): { text: string; decorative: boolean } {
  const [phrase, setPhrase] = useState<string>(() => pick());
  useEffect(() => {
    if (!booting) return;
    const t = setInterval(() => setPhrase((p) => pick(p)), GETTING_READY_MS);
    return () => clearInterval(t);
  }, [booting]);
  if (!booting) return { text: "", decorative: false };
  if (shaderPercent !== null && shaderPercent > 0) {
    return { text: `Processing shaders — ${shaderPercent}%`, decorative: false };
  }
  return { text: phrase, decorative: true };
}
