-- 0068_host_codec_pixel_rates.up.sql — per-codec encoder throughput hint (#506).
--
-- `hosts.codecs` (migration 0031) says WHICH codecs a host can produce; it says
-- nothing about how fast. The two are not interchangeable, and the gap is not a
-- rounding error: measured on an RTX 5090, `vulkanh265enc` sustains ~395 Mpix/s
-- against ~1400 for `vulkanh264enc` and ~1215 for `vulkanav1enc`. 1440p120 needs
-- 442 and 2160p60 needs 498, so an h265 session at either tier runs BELOW TIER —
-- and silently, because the encoder back-pressures the compositor instead of
-- dropping (a live 2560x1440@120 h265 session delivered fps(recv)=96 with zero
-- drops and zero freezes).
--
-- One additive nullable column, holding the agent's `capacity.codec_throughput`
-- map verbatim (agent-api.md): `{"h265": {"max_pixel_rate_mpix_s": 395}, ...}`.
--
-- STORED AS THE AGENT SENT IT, not flattened to codec→number. The wire shape is
-- an object per codec precisely so a future measured probe can add a measurement
-- timestamp or a confidence without a contract change; flattening here would
-- discard those on arrival and make the amendment pointless.
--
-- NULL is the pre-amendment state and means UNKNOWN, which gates nothing —
-- exactly like a NULL `codecs` reading back as h264-only rather than as "no
-- codecs". There is deliberately no backfill and no default: a host that has not
-- reported has not reported, and inventing a rate for it would gate real launches
-- on a number nobody measured.
BEGIN;

ALTER TABLE hosts
    ADD COLUMN codec_pixel_rates JSONB;

COMMIT;
