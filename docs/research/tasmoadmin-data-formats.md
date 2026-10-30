# TasmoAdmin on-disk formats (for an importer)

*Historical research from before the build. KEEP/DROP ratings, "Avoid" lines and "should" statements are design input, not a description of what TasmoFleet does today.*

Data directory = env `TASMO_DATADIR` or `<app>/data/` (Docker image: `/data/tasmoadmin` bind-mounted). Defined in `includes/bootstrap.php`.
Other env dirs: `TASMO_TMPDIR` (default `<app>/tmp/`), `TASMO_SESSIONDIR` (default `<data>/sessions/`), `TASMO_HEALTHDIR` (default `<app>/health/`; image default `/health`).

```
<data>/
  devices.csv                 device list (no header row)
  MyConfig.json               settings (JSON, pretty-printed)
  .device-password.key        base64 of 32 random bytes, mode 0600 (device/MQTT password key)
  passkeys.json               WebAuthn credentials (optional)
  firmwares/                  OTA files served over HTTP (+ .empty, .htaccess)
  updates/                    self-update zip scratch (tasmoadmin.zip)
  sessions/                   PHP session files sess_<id>, 0700
  MyConfig.php                legacy (<= tag 1.4.0), migrated to MyConfig.json on first start
<tmp>/backups/<id>-<name>.dmp + backup.zip        last backup run only (wiped each run)
<tmp>/restore/<token>.dmp                         one-shot restore upload
<tmp>/cache/i18n/                                 language caches
<health>/health.db (+ -wal, -shm)                 SQLite
```

## 1. devices.csv

Source: `src/DeviceRepository.php` (`addDevices`, `updateDevice`, `prepareStorageRow`), `src/DeviceFactory.php`, `src/Device.php`.

Format: comma-separated, PHP `fputcsv` with enclosure `"` and **escape char `\`** (explicitly set; `\"` inside a quoted field is an escaped quote on read, not a doubled `""` only).
No header. One device per line, 16 columns (0-based index). Older files have fewer columns; on read, missing columns are filled with the defaults below and the file is rewritten (so the importer must tolerate short rows, e.g. test fixture `tests/fixtures/devices.csv`: `1,socket-1,192.168.1.2,user,password,bulb_1,2,,,,,5000` and a 7-column row).

| Idx | Property (`Device`) | Meaning | Default when missing | Notes |
|---|---|---|---|---|
| 0 | `id` | Integer id, next = max(id)+1. Referenced by `health.db.device_id`, backup file names, URLs | none | Never reused logic-wise only if the max row is deleted, so ids can be recycled |
| 1 | `names` | Relay names joined with `|` (one entry per relay/channel) | `''` | Multi-relay devices have several; `getName()` joins with `-` |
| 2 | `ip` | IPv4 address (or hostname, no validation) | `''` | |
| 3 | `username` | HTTP auth user for the device web UI | `''` | Tasmota default user is `admin`; sent as query param `user=` only when a password is set |
| 4 | `password` | HTTP auth password; `enc:v1:` + base64(iv[12] + tag[16] + ciphertext), AES-256-GCM | `''` | Legacy plaintext cells are encrypted and rewritten on first read. Empty stays empty. Plaintext passwords stored via autoscan may have been HTML-escaped (`&amp;` etc.), see lessons.md |
| 5 | `img` | Icon name (e.g. `bulb_1`) | `bulb_1` | UI only |
| 6 | `position` | Sort position; unique, collisions bumped | `0` -> 1 | UI only; 0 is normalised to 1 and written back |
| 7 | `deviceAllOff` | Included in "All off" (0/1) | `1` | UI only |
| 8 | `deviceProtectionOn` | Block switching on (0/1) | `0` | UI only |
| 9 | `deviceProtectionOff` | Block switching off (0/1) | `0` | UI only |
| 10 | `isUpdatable` | Offer for OTA update (0/1) | `1` | Keep -> "exclude from OTA" |
| 11 | `port` | HTTP port | `80` | `buildBasicAuthUrl` (used for `/dl` backup) ignores the port, see lessons |
| 12 | `friendlyNames` | Friendly names joined with `|` | falls back to column 1 | |
| 13 | `deviceConfirmToggle` | Ask before toggling (0/1) | `confirm_device_toggles` config (`1` if on) | UI only |
| 14 | `mqttTopic` | Tasmota `Topic` (e.g. `kitchen_light`) used to match MQTT messages | `''` | Needed for health/MQTT discovery |
| 15 | `deviceHideFromStartpage` | Hide tile on start page (0/1) | `0` | UI only |

Importer-relevant: keep columns 0-4, 10, 11, 12, 14 (and 1 for naming). Everything else is dashboard UI state. Derived by `DeviceFactory`: keywords (`single|multi`, `IP#`, `ID#`, `POS#`, `TOPIC#`) for search only.
Decrypt: key = base64-decode of `.device-password.key` content (or env `TASMO_DEVICE_PASSWORD_KEY`, same encoding; if both exist they must be equal text, otherwise TasmoAdmin hard-errors); `cipher=aes-256-gcm`, iv 12 bytes, tag 16 bytes, no AAD; payload layout is `iv || tag || ciphertext` then base64.

Sanitized example rows (2 devices; the second is a 2-relay device with an encrypted password; ciphertext is fake):
```
1,"Kitchen Light",192.168.1.50,admin,,bulb_1,1,1,0,0,1,80,"Kitchen Light",0,kitchen_light,0
2,"Desk Left|Desk Right",192.168.1.51,admin,enc:v1:QUJDREVGR0hJSktMMTIzNDU2Nzg5MDEyMzQ1Njc4OTBhYmNkZWY=,socket_1,2,1,0,0,1,80,"Desk Left|Desk Right",0,desk_strip,0
```
(Real password cells are longer than 28 raw bytes; a cell is "recognised as encrypted" if it starts with `enc:v1:`, base64-decodes strictly and the decoded length is > 28.)

## 2. MyConfig.json

Source: `src/Config.php` (`$defaults`, constructor). Flat JSON object, **all values are strings** (`writeAll` trims and stores strings; `0` int is mapped to the default). Missing keys are added with defaults on every start. Sensitive key `mqtt_discovery_password` is stored as `enc:v1:...` with the same cipher/key as device passwords. `password` holds a bcrypt hash (`$2y$...`) or a legacy md5 hex (upgraded on next login).
Dynamic defaults: `ota_server_ip`/`ota_server_port` from `$_SERVER['SERVER_ADDR'|'SERVER_PORT']` at first creation; `scan_from_ip`/`scan_to_ip` = SERVER_ADDR with last octet 2 / 254; `update_channel` = `docker` when `.dockerenv` exists in the app root.

| Key | Default | Meaning | tasmofleet relevance |
|---|---|---|---|
| `ota_server_ip` | server addr | Host/IP devices use to fetch firmware & restore blobs | KEEP (as OTA advertise address) |
| `ota_server_port` | server port | Port of that server | KEEP |
| `username` | `''` | Local login name | map to admin user if wanted (do not import hash blindly) |
| `password` | `''` | bcrypt hash (or md5) of login password | optional import (bcrypt is portable) |
| `refreshtime` | `8` | UI poll seconds; `none` disables | DROP |
| `current_git_tag` | `''` | App version, written automatically | DROP |
| `current_git_branch` | `''` | App branch | DROP |
| `update_automatic_lang` | `tasmota-sensors` | ESP8266 firmware variant for automatic download | KEEP -> default variant policy |
| `update_automatic_lang_esp32` | `tasmota32` | ESP32 variant for automatic download | KEEP (but per-device is better) |
| `nightmode` | `auto` | `auto`/`always`/`disable` (settings page only writes `auto` or `disable`) | DROP |
| `login` | `1` | `1` = auth required; `0` = auth off | map to auth config |
| `scan_from_ip` | `<net>.2` | Autoscan range start | KEEP as last-used scan range |
| `scan_to_ip` | `<net>.254` | Autoscan range end | KEEP |
| `additional_scan_ranges` | `''` | Extra `from-to` ranges, newline separated | KEEP |
| `port` | `80` | Default device HTTP port for scans | KEEP |
| `homepage` | `start` | `start`/`devices`/`health` | DROP |
| `check_for_updates` | `3` | Footer app update check toggle (`1` on) | DROP |
| `minimize_resources` | `1` | Serve `.min.js` | DROP |
| `update_channel` | `stable` (`docker` in image) | TasmoAdmin self-update channel (`stable`/`beta`/`dev`/`docker`) | DROP |
| `hide_copyright` | `1` | Hide footer copyright | DROP |
| `show_search` | `1` | Show filter box | DROP |
| `confirm_device_toggles` | `0` | Confirm before toggling | DROP |
| `update_fe_check` | `0` | Browser-triggered (now server-side) HEAD check of OTA URL before running | REDESIGN (always check) |
| `update_be_check` | `1` | Server HEAD-checks OTA URLs when preparing update page | REDESIGN |
| `update_newer_only` | `1` | Skip devices already newer than target (no downgrades) | KEEP as per-run option |
| `auto_update_channel` | `stable` | Tasmota firmware channel for the OTA scraper: `stable` or `dev` | KEEP |
| `force_upgrade` | `0` | Upgrade even if already at target version | KEEP as per-run option |
| `connect_timeout` | `5` | Guzzle connect timeout (s) for device HTTP | KEEP |
| `timeout` | `5` | Guzzle total timeout (s) | KEEP |
| `request_concurrency` | `50` | Parallel HTTP requests (status refresh, autoscan) | KEEP (lower default) |
| `mqtt_discovery_host` | `''` | Broker host; also read by the health collector | KEEP |
| `mqtt_discovery_port` | `1883` | Broker port | KEEP |
| `mqtt_discovery_username` | `''` | Broker user | KEEP |
| `mqtt_discovery_password` | `''` | Broker password, `enc:v1:` | KEEP (re-encrypt) |
| `mqtt_discovery_cmnd_prefix` | `cmnd` | Tasmota `Prefix1` | KEEP |
| `mqtt_discovery_stat_prefix` | `stat` | `Prefix2` | KEEP |
| `mqtt_discovery_tele_prefix` | `tele` | `Prefix3` | KEEP |
| `mqtt_discovery_subscriptions` | `tele/+/LWT` | Newline-separated subscription filters used for discovery | KEEP |
| `mqtt_discovery_timeout_seconds` | `5` | Scan duration / connect timeout | KEEP |
| `health_enabled` | `1` | Collector on/off | KEEP |
| `health_http_poll_interval` | `60` | Seconds between HTTP sweeps (collector enforces min 5) | KEEP |
| `health_offline_grace` | `180` | Seconds both channels may be down before state becomes `offline` | KEEP |
| `health_mqtt_subscription` | `#` | MQTT filter for health messages | KEEP (narrow default) |

Unknown extra keys may exist (e.g. leftovers; `page` and `use_gzip_package` are deleted on start). A settings POST writes **every posted field** into this file (mass assignment, see lessons.md), so third-party keys can be present.

## 3. health.db (SQLite, WAL)

Source: `src/Health/HealthRepository.php`. Written only by `bin/health-collector`, read by the web app. One table, latest state per device, no history:

```sql
CREATE TABLE device_health (
  device_id     TEXT PRIMARY KEY,   -- devices.csv id as string
  state         TEXT,               -- online | degraded_mqtt | degraded_http | offline | unknown
  http_up       INTEGER,            -- 1/0/NULL (last HTTP poll result)
  mqtt_up       INTEGER,            -- 1/0/NULL (last LWT/STATE verdict)
  last_http_ok  INTEGER,            -- unix seconds
  last_mqtt_ok  INTEGER,            -- unix seconds
  last_seen     INTEGER,            -- max(last_http_ok, last_mqtt_ok)
  rssi          INTEGER,            -- Tasmota Wifi.RSSI (signal quality 0-100 %)
  "signal"      INTEGER,            -- Tasmota Wifi.Signal (dBm, negative)
  updated_at    INTEGER,            -- unix seconds of last upsert
  mqtt_topic    TEXT,               -- effective topic (configured, else learned from Status 0)  [added later via ALTER TABLE]
  mqtt_expected INTEGER             -- 1 if MQTT should be reporting (topic known and MqttHost set) [added later]
);
```
Pragmas: `journal_mode=WAL`, `busy_timeout=5000`. Note `rssi` is Tasmota's quality percentage while `signal` is dBm; `HealthView::signalClass` uses `rssi` thresholds 75/50. Importer value: low (no history); safe to skip.

## 4. passkeys.json

Source: `src/PasskeyRepository.php`. JSON array (pretty-printed, mode 0600, written via `.tmp` + rename):
```json
[
  {
    "id": "<credentialId, base64url>",
    "publicKey": "<PEM public key string from lbuchs/webauthn processCreate>",
    "signCount": 0,
    "name": "Passkey",
    "createdAt": 1760000000,
    "lastUsedAt": null
  }
]
```
`name` max 64 chars. RP id = request host, so credentials only work on the exact hostname they were registered on, which makes them non-portable across a host change: skip in the importer (re-register).

## 5. Firmware folder layout (`data/firmwares/`)

Served as-is over HTTP at `http://<ota_server_ip>:<ota_server_port><BASEURL>data/firmwares/<file>` (`OtaHelper::FIRMWARE_PATH`). Contents are ephemeral: wiped (all files except `.empty`, `.htaccess`) on every POST to `/upload`.
- Manual upload: `tasmota.bin` or `tasmota.bin.gz`, optional `tasmota-minimal.bin` or `tasmota-minimal.bin.gz`.
- Automatic: files keep their upstream names, e.g. ESP8266 `tasmota-sensors.bin.gz` + `tasmota-minimal.bin.gz`, ESP32 `tasmota32.bin` (no minimal); both platforms can be present at once.
- URL allow-list for the reachability check: basename must match `^[\w.-]+$` and the URL must equal what `OtaHelper::getFirmwareUrl` produces.
- Upstream index (scraped) examples: `tests/fixtures/dev.html`, `tests/fixtures/dev_esp32.html`.

## 6. Other runtime data
- `tmp/backups/<id>-<names joined '-', '/'->'_'>.dmp`: raw Tasmota `/dl` config dumps; zipped to `tmp/backups/backup.zip`. Nothing persistent; importer needs nothing from here (but tasmofleet should offer to import any `.dmp` files the user holds, filename prefix = old device id).
- Sessions: `sess_<id>` PHP-serialised, 30 days; irrelevant.
- Fixtures for parser tests: `tests/fixtures/response-invalid-v5100.json`, `response-invalid-v8500.json`, `response-unauthorized.json` (`{"WARNING":"Need user=<username>&password=<password>"}`), `latest.json`.
