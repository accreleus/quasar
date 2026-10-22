-- 0106 — repair apps.default_encode_slots <= 0 and forbid it (#784).
--
-- Rows created before the create path stopped decoding an omitted field to 0
-- hold default_encode_slots = 0 (crud/store.go createApp). Console self-heal
-- (#782) reads "reserves no encode slot" as "console session", so a streamed
-- session of such an app is mistaken for one. The API already rejects <= 0; this
-- makes the column agree.
--
-- Existing rows: every app with default_encode_slots <= 0 becomes 1. Nothing else
-- changes (the column is NOT NULL, so there is no NULL case). A derived tile's
-- own value is never read (admission resolves from the parent), so repairing it
-- changes nothing a session does.
BEGIN;

UPDATE apps SET default_encode_slots = 1 WHERE default_encode_slots <= 0;

ALTER TABLE apps
	ADD CONSTRAINT apps_default_encode_slots_ck CHECK (default_encode_slots > 0);

COMMIT;
