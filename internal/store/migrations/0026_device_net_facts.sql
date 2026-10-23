-- 0026_device_net_facts: what a probe read from the device about its network (Status 5:
-- hostname, IP address, gateway; Status 11: SSID, BSSID, channel). Written by the prober only
-- (device create and UpdateProbe), so the device page no longer depends on a backup snapshot.
-- net_at is 0 when the facts were never read.
ALTER TABLE devices ADD COLUMN net_hostname TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN net_ip TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN net_gateway TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN net_ssid TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN net_bssid TEXT NOT NULL DEFAULT '';
ALTER TABLE devices ADD COLUMN net_channel INTEGER NOT NULL DEFAULT 0;
ALTER TABLE devices ADD COLUMN net_at INTEGER NOT NULL DEFAULT 0;
