-- Reverse of 0040.
--
-- Dropping the table destroys every stored secret. That is unavoidable and
-- honest: the values are only readable with the master key, so there is nowhere
-- else to put them on the way down. An operator rolling back keeps whatever
-- env-var fallbacks they had (e.g. QUASAR_STEAMGRIDDB_API_KEY) and must
-- re-enter anything that was only ever set through the admin UI.
DROP TRIGGER IF EXISTS instance_secrets_set_updated_at ON instance_secrets;
DROP TABLE IF EXISTS instance_secrets;
