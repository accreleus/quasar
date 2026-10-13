-- Deliberate no-op. 0099 is a DATA migration with no schema half: it removed
-- the console settings amendment 19 retired from stored console_config rows.
--
-- It cannot be reversed, and nothing a rollback could use was lost: every
-- dropped key had a default that the older reader applies when the key is
-- absent, so a trimmed row reads exactly as a row whose retired settings were
-- never edited.

SELECT 1;
