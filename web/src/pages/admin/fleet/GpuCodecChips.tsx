/**
 * One GPU's codec set as chips (#296/#302), shared by the fleet host
 * expansion and the host-detail capacity card so the two surfaces cannot
 * drift. Styled after the setup wizard's codec section
 * (`pages/setup/StepHosts.tsx` `CodecSection`) — no mock covers a per-GPU
 * chip, so that is the idiom this reuses rather than a new one.
 *
 * `codecs === null` means this GPU inherits a host that has never reported a
 * codec set either (`GPUAvailability.codecs`, amendment 12) — distinct from
 * an empty list, and rendered as a muted chip rather than nothing.
 */
import { Chip } from "../../../components/Chip";
import { codecDisplayName } from "../../../lib/codecDisplay";
import type { GPUAvailability } from "../../../api/types";

type Codec = NonNullable<GPUAvailability["codecs"]>[number];

/** Fixed display order; an unrecognised value (open wire vocabulary) sorts last. */
const CODEC_ORDER: readonly Codec[] = ["h264", "h265", "av1"];

function codecRank(codec: Codec): number {
  const i = CODEC_ORDER.indexOf(codec);
  return i === -1 ? CODEC_ORDER.length : i;
}

export function GpuCodecChips({ codecs }: { codecs: GPUAvailability["codecs"] }) {
  if (codecs == null || codecs.length === 0) {
    return (
      <Chip variant="neutral" title="Neither this GPU nor its host has reported a codec set yet.">
        Not reported
      </Chip>
    );
  }

  const ordered = [...codecs].sort((a, b) => codecRank(a) - codecRank(b));
  return (
    <>
      {ordered.map((c) => (
        <Chip key={c} variant="success">
          {codecDisplayName(c) ?? c}
        </Chip>
      ))}
    </>
  );
}
