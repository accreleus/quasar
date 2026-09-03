-- Reverses 0073_host_enrollment_used_by.up.sql.
--
-- Restoring NOT NULL means rows whose minter has since been deleted cannot
-- survive: they are removed first. Down is a development path, and a token
-- nobody can attribute is not worth failing the migration over.
DELETE FROM host_enrollments WHERE created_by IS NULL;

ALTER TABLE host_enrollments DROP CONSTRAINT host_enrollments_created_by_fkey;
ALTER TABLE host_enrollments
    ADD CONSTRAINT host_enrollments_created_by_fkey
    FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE CASCADE;
ALTER TABLE host_enrollments ALTER COLUMN created_by SET NOT NULL;

ALTER TABLE host_enrollments DROP COLUMN used_by_node_name;
