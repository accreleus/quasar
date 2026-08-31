BEGIN;

ALTER TABLE hosts
    DROP COLUMN IF EXISTS codec_pixel_rates;

COMMIT;
