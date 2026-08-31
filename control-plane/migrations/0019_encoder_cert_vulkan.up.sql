-- 0019_encoder_cert_vulkan.up.sql — VK-06 (Vulkan-encode adoption).
-- Extend host_encoder_certification's encoder CHECK to accept the new 'vulkan'
-- encoder (QUASAR_ENCODER=vulkan, vulkanh264enc). Additive: the hostcfg catalog
-- and agent already accept the value; without this the SPT-06 cert harness's
-- vulkan rows are silently rejected by the constraint.
ALTER TABLE host_encoder_certification
    DROP CONSTRAINT host_encoder_certification_encoder_check;
ALTER TABLE host_encoder_certification
    ADD CONSTRAINT host_encoder_certification_encoder_check
    CHECK (encoder IN ('va', 'nvenc', 'openh264', 'vulkan'));
