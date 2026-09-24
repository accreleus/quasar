-- Rolling back requires every retained history row to have a catalog parent;
-- PostgreSQL rejects the FK restoration rather than discarding history.
ALTER TABLE host_image_success_history
    ADD CONSTRAINT host_image_success_history_image_id_fkey
    FOREIGN KEY (image_id) REFERENCES image_catalog(id) ON DELETE CASCADE;
DROP TABLE host_image_cleanup_attempts;
DROP TABLE host_image_operation_fences;
