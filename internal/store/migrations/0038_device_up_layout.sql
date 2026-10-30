-- 0038_device_up_layout: the flash layout of an ESP32 as an earlier HTTP read of its /up page showed it
-- ('direct': two app partitions, 'safeboot': one app partition next to safeboot, '': never read).
-- The upload of a firmware image over MQTT (FileUpload Type 1) needs a layout PROVEN to be direct and
-- cannot read the page itself. Written only by the OTA engine when it reads /up.
ALTER TABLE devices ADD COLUMN up_layout TEXT NOT NULL DEFAULT '';
-- up_layout_at: when it was read (unix ms, NULL: never). The layout is cleared when the chip or its flash
-- size changes (a probe) and when the entry is replaced by another device.
ALTER TABLE devices ADD COLUMN up_layout_at INTEGER;

-- ota_jobs.mqtt_upload_state: how an upload of firmware through the broker ended up uncertain, so that it
-- survives a restart of tasmofleet: '' (nothing uncertain), 'interrupted' (the engine restarted while the
-- upload ran) or 'unconfirmed' (every byte was sent and nothing confirmed it).
ALTER TABLE ota_jobs ADD COLUMN mqtt_upload_state TEXT NOT NULL DEFAULT '';
