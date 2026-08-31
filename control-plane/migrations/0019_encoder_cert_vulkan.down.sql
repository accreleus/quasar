-- 0019_encoder_cert_vulkan.down.sql — revert to the 0018 encoder set.
-- Any 'vulkan' rows must be removed first or the narrower CHECK cannot be applied.
DELETE FROM host_encoder_certification WHERE encoder = 'vulkan';
ALTER TABLE host_encoder_certification
    DROP CONSTRAINT host_encoder_certification_encoder_check;
ALTER TABLE host_encoder_certification
    ADD CONSTRAINT host_encoder_certification_encoder_check
    CHECK (encoder IN ('va', 'nvenc', 'openh264'));
