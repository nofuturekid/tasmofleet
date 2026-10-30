# TasmoAdmin feature inventory (fork branch `local`)

*Historical research from before the build. KEEP/DROP ratings, "Avoid" lines and "should" statements are design input, not a description of what TasmoFleet does today.*

Source: `ta-full/tasmoadmin/` (PHP 8.2, Guzzle, jQuery/Bootstrap frontend, nginx+php-fpm in Docker).
Paths below are relative to `ta-full/tasmoadmin/`. Ratings are for tasmofleet, a fleet manager focused on
OTA updates, config backup, health, and bulk commands.

Legend: KEEP = carry over as-is in concept; REDESIGN = keep the goal, change the mechanism; DROP = HA covers it or rarely useful.

Routes (`includes/routes.php`): `/`, `/start`, `/devices`, `/device_action/{add|edit|delete}/{id}`, `/device_action_modal/...`,
`/device_config/{id}`, `/device_update` (run), `/upload_form`, `/upload`, `/backup`, `/devices_autoscan`, `/health`, `/health_data`,
`/site_config`, `/selfupdate`, `/login`, `/logout`, `/passkey/{action}`, `/actions` (AJAX hub), `/manifest`, `/service-worker`, `/offline`, `/change_language/{lang}`.
All state-changing calls go through POST + CSRF token (session based); a few legacy GET mutations are rejected (405) in `RequestHelper::isLegacyMutationRequest`.

## 1. Device inventory

### 1.1 Device list / table (`pages/devices.php`, `pages/elements/devices_table.php`, `resources/js/devices.js`)
- What: sortable/searchable table of all devices with live status. Columns: ID, position, name, IP, power state (toggle), WiFi signal, firmware version, uptime,
  energy power, temp, humidity, illuminance, and "more" columns (hostname, MAC, MQTT, IDX, PowerOnState, LedState, SaveData, Sleep, BootCount, SaveCount, LogLevels, WifiConfig, VCC).
  Column visibility/order preferences are stored in the browser (`device_list_preferences.js`).
- Tasmota commands: `Status 0` per device (one `GET http://ip:port/cm?cmnd=status 0`), fan-out by `Sonoff::doAjaxAll` (Guzzle Pool, `request_concurrency` default 50) or one browser XHR per row
  in batches of `request_concurrency`; auto refresh every `refreshtime` seconds (default 8). Power: `Power<N> toggle`, `Power<N> 0`.
- Rating: **REDESIGN**. Keep a fleet table with version, uptime, RSSI, health, last-seen, firmware variant. Drop energy/temp/humidity/lux columns and relay toggles (HA owns that).
  Poll server-side on a schedule and push to the UI (SSE/WebSocket) instead of 8 s browser polling that fires N requests per refresh.

### 1.2 Add / edit / delete device (`pages/device_action.php`, `pages/elements/modal_add_device.php`, `src/DeviceRepository.php`)
- What: manual add by IP+port (+ optional HTTP user/password); "Search" step runs `Status 0` and pre-fills friendly names per relay; edit all fields; delete (POST). Fields: names (per relay), friendly names, IP, port, user, password, icon, position, "all off" membership, protect-on/off, updatable flag, confirm-toggle, MQTT topic, hide-from-start. Bulk delete via `/actions removeDevices`.
- Commands: `Status 0` (probe).
- Rating: **KEEP** (add/edit/delete, probe-on-add). **DROP** icon, position, all-off, protect-on/off, confirm-toggle, hide-from-startpage (dashboard concerns). Keep `isUpdatable` as "exclude from OTA", keep per-device credentials, keep MQTT topic.

### 1.3 Autoscan: network (`pages/devices_autoscan.php`, `src/Helper/IpHelper.php`, `Sonoff::search`)
- What: scan an IPv4 range (+ extra ranges, newline separated, max 1024 IPs) for Tasmota devices on a given HTTP port with shared credentials; lists found devices (skips already known `ip:port`), "save one"/"save all".
- Commands: `Status 0` against each IP (Guzzle Pool, concurrency 50, timeout 5 s, `max_execution_time` raised to 180 s). Device identified by `StatusNET.IPAddress` in the reply.
- Rating: **KEEP** (rebuild with Go goroutines + CIDR input + streaming results; identify by `Status 0` + MAC). Show progress; do not block a request.

### 1.4 Autoscan: MQTT discovery (`src/Mqtt/MqttDiscoveryService.php`, `pages/devices_autoscan.php`, `pages/elements/site_config_mqtt_discovery_panel.php`)
- What: connects to the broker, subscribes to `tele/+/LWT` (configurable filters) and `stat/#`, and for each discovered topic publishes `cmnd/<topic>/STATUS` payload `0`; merges the several `stat/<topic>/STATUS`, `STATUS5` replies (reply is split across messages) to get `StatusNET.IPAddress`. Outcome buckets: new devices, updated devices (IP drift fix for known topic, or legacy match by ip+port gets topic backfilled), offline topics (no reply), conflicts (duplicate topic, address already claimed, ...).
- Commands: MQTT `cmnd/<topic>/STATUS 0`; subscribes to `tele/<topic>/LWT`, `stat/<topic>/STATUS*`. Config: `mqtt_discovery_*` (host, port, user, encrypted password, cmnd/stat/tele prefixes, subscriptions, timeout 5 s).
- Rating: **KEEP** (valuable: also fixes DHCP IP drift automatically). Redesign as a long-running MQTT subscriber that continuously maintains topic<->IP mapping instead of a one-shot 5 s scan. Handle FullTopic variants (prefix/topic order) properly; current code assumes `prefix/topic/suffix`.

### 1.5 Start page / dashboard (`pages/start.php`, `resources/js/start.js`)
- What: tile grid of devices with on/off toggles, "All off" tile, per-state health counters linking to `/health?state=...`, night-mode icons, optional confirm-before-toggle and protect-on/off.
- Commands: `Status 0`, `Power<N> toggle|0`.
- Rating: **DROP** tiles/toggles/all-off (Home Assistant does it). Keep only the health summary counters as the landing page.

## 2. Firmware / OTA

### 2.1 Firmware source: automatic download (`pages/upload.php` branch `auto`, `src/Helper/TasmotaOtaScraper.php`, `src/Helper/TasmotaHelper.php`, `src/Update/FirmwareDownloader.php`)
- NOTE: the source is NOT GitHub releases. It scrapes the HTML directory listing of `https://ota.tasmota.com/`:
  - stable ESP8266: `https://ota.tasmota.com/tasmota/release/`, stable ESP32: `https://ota.tasmota.com/tasmota32/release/`
  - dev ESP8266: `https://ota.tasmota.com/tasmota/`, dev ESP32: `https://ota.tasmota.com/tasmota32/`
  - Channel = config `auto_update_channel` (`stable`|`dev`).
  - Parsing (Symfony BrowserKit/DomCrawler): file names = `table tr td:nth-child(2)` (kept only if the cell text starts with `http`, name = `basename`), version = regex `((\d+\.)+\d)` on the first `<h2>`, publish date = column 6 (ESP8266) / column 4 (ESP32), format `Ymd H:i`. Fragile: depends on the page's HTML table layout.
- Variant/URL selection (`TasmotaHelper::getLatestFirmwares`):
  - Two admin-chosen variant names, one per platform: `update_automatic_lang` (default `tasmota-sensors`) and `update_automatic_lang_esp32` (default `tasmota32`). ESP32 is chosen when the configured name starts with `tasmota32`.
  - ESP8266: file `<variant>.bin.gz` plus the fixed `tasmota-minimal.bin.gz` (both must exist or `InvalidArgumentException('Failed to resolve firmware')`).
  - ESP32: file `<variant>.bin`; no minimal image.
  - Dropdown lists (settings page + upload form) are built by listing the same index: ESP8266 = names ending `.bin.gz` minus anything containing `-minimal.bin`; ESP32 = names ending `.bin`.
  - Downloads are saved by `basename(url)` into `data/firmwares/` (Guzzle `sink`), no checksum/size/gzip validation.
  - Every POST to `/upload` first wipes `data/firmwares/` (`FirmwareFolderHelper::clean`), so only the latest download survives.
- Consequence: all ESP32 devices get the same image, no per-chip variant (c3, s2, s3, solo1, ...) and no per-device flavor (lite, sensors, ir, ...).
- Rating: **REDESIGN**. Per device, derive the variant from `Status 2` `StatusFWR.Version` suffix, e.g. `14.4.1(tasmota32c3)` and `Hardware`, then fetch the same-named file. Keep a versioned local cache with SHA256 and magic-byte checks (ESP8266 gz magic `1f 8b`, ESP32 image header `0xE9`). Prefer a machine-readable index over HTML scraping (verify what ota.tasmota.com offers, e.g. GitHub releases API for arendst/Tasmota assets as fallback).

### 2.2 Firmware source: manual upload (`pages/upload.php` branch `upload`, `pages/upload_form.php`)
- What: upload a full firmware and optionally a minimal one. Stored as fixed names `tasmota.bin`/`tasmota.bin.gz` and `tasmota-minimal.bin(.gz)`. Limits: full 5 MB, minimal 502000 bytes. Type check uses the browser-supplied MIME (`application/gzip|x-gzip` -> `.bin.gz`, `application/octet-stream|macbinary` -> `.bin`). Target version guessed from the uploaded file name (`FirmwareVersionExtractor`, regex `\d+\.\d+\.\d+(\.\d+)?`).
- Rating: **KEEP** (custom builds are real), **REDESIGN** with content sniffing, keep original names, derive version from the image header/`Version` string, per-platform slots.

### 2.3 OTA server (`src/Helper/OtaHelper.php`, `data/firmwares/.htaccess`, nginx location rules in the image)
- What: the app itself serves `http://<ota_server_ip>:<ota_server_port><base>data/firmwares/<file>` (plain HTTP on purpose: ESP8266 OTA cannot do modern TLS). `ota_server_ip`/`ota_server_port` default from `$_SERVER['SERVER_ADDR'/'SERVER_PORT']` and are editable on the upload form. `data/firmwares/.htaccess` sets CORS `*`. All other `data/` is denied.
- Server-side reachability check: `POST /actions checkOtaUrl` does a `HEAD` (10 s, no redirects) but only for URLs `OtaHelper` itself could build (anti-SSRF), used when `update_fe_check=1`; `update_be_check=1` (default) additionally HEAD-checks in `upload.php` before showing the device list.
- Rating: **KEEP** the concept, **REDESIGN**: dedicated unauthenticated read-only listener (separate port) for firmware + restore blobs, advertised address detected from the interface that routes to the device (or configured), optional per-device one-time tokens in the URL.

### 2.4 Update flow (browser orchestrated) (`pages/device_update.php`, `resources/js/device_update.js`, `resources/js/device_update_logic.js`)
- What: user picks devices on `/upload` (the device table, non-updatable ones disabled), POST to `/device_update` with `update_targets` JSON (`{esp8266|esp32|default: {minimalOtaUrl, otaUrl, targetVersion, source}}`) and `device_ids[]`. The page's JS then, for ALL selected devices in parallel (`Promise.all`):
  1. `Status 0` (retry up to 15x, 10 s apart) -> version from `StatusFWR.Version`, platform from `StatusFWR.Hardware` containing `ESP32` or Version containing `(tasmota32` (else esp8266).
  2. Decision (`getPreUpdateDecision`): at-target if same version and not `force_upgrade`; "downgrade" skip if `update_newer_only` and target < current; a `(minimal)` version always proceeds.
  3. Plan (`determineUpgradePlan`): auto source + ESP8266 + running version < 9.1.3 -> BLOCKED ("legacy path required"); minimal URL present and device not already minimal -> two steps (minimal, then final); else direct.
  4. Per step: `OtaUrl <url>`, `Upgrade 1`, sleep 10 s, poll `Status 0` up to 15x/10 s until success. Success: minimal step = version string contains `(minimal)`; final step = version equals target and not minimal; no target version = version changed.
  5. Since Tasmota 14.4.0 minimal firmware lacks `Status` (returns `{"Command":"Unknown"}`): fallback `Upgrade` with no parameter, parse `{"Upgrade":"1 or >X.Y.Z to upgrade"}` to learn the running version and fake `StatusFWR.Version = "X.Y.Z(minimal)"`.
  - Log is shown live in the page only (no persistence). Result line "N of M successful".
- Commands: `Status 0`, `OtaUrl <url>`, `Upgrade 1`, `Upgrade` (query).
- Rating: **REDESIGN (core feature)**. Move orchestration server-side as a persisted job state machine with bounded concurrency (e.g. 3-5 devices), per-device phases, resumable after restart, results/history stored. Detect reboot via uptime reset / BootCount / Restart reason, not only version. Rollback awareness (`Status 1` `BootCount`, `Upgrade` result).

### 2.5 Settings for update behaviour (`pages/site_config.php`)
- `update_fe_check`, `update_be_check`, `update_newer_only` (default 1), `force_upgrade` (default 0), `auto_update_channel`, `update_automatic_lang(_esp32)`, `ota_server_ip/port`.
- Rating: **REDESIGN**: per-run options (force, allow downgrade, dry run) instead of global toggles; channel + variant policy in config.

## 3. Config backup / restore (`pages/backup.php`, `src/Backup/BackupHelper.php`, `Sonoff::backup/restore`, `pages/actions.php`)
- Backup: for each selected device `GET http://user:pass@ip/dl` (Tasmota config download, binary `.dmp`) saved as `<id>-<name joined by '-'>.dmp` (`/` -> `_`) in `tmp/backups/` (dir is wiped each run), zipped to `backup.zip`, offered at `/actions?downloadBackup`. Failures are collected per device (only `ConnectException` is caught). Batch backup is available both on `/backup` and from the device list batch action. Backups are NOT kept: each run deletes the previous set; no timestamps, no retention, no schedule, no diffing.
- Restore: single device only. Uploaded `.dmp` (extension check only) is stored in `tmp/restore/<24-hex token>.dmp`; the device is told `WebGetConfig http://<ota_ip>:<ota_port><base>actions?downloadRestore=<token>`, fetches it (file deleted after serving).
- Commands: HTTP `/dl`, `WebGetConfig <url>`.
- Rating: **KEEP + REDESIGN**: scheduled and pre-OTA automatic backups, timestamped versions with retention, stored with metadata (firmware version, device id/MAC), decode/diff of key settings (the `.dmp` format is Tasmota's settings struct; also keep `Backlog`-style export via `Status 0`..`Status 11`/`Template`/`Rule1..3`), restore of a chosen version, multi-device restore. Serve restore blob from the unauthenticated OTA listener with a one-time token (see lessons: the current `downloadRestore` sits behind login).

## 4. Bulk operations (`pages/devices.php`, `resources/js/device_batch_actions.js`, `resources/js/devices.js`)
- Batch actions on selected devices: `command` (free text sent verbatim to each device via `/actions doAjax`, results echoed as raw JSON), `restart` (`Restart 1`, confirm modal), `backup`, `restore` (single), `delete`.
- Per-device row actions: toggle, restart (`Restart 1`), inline edit of some config cells (`updateConfig`), link to device web UI, config page, edit, delete.
- Rating: **KEEP/REDESIGN**: bulk command is a core feature; add allow-list or at least dry-run/preview, `Backlog` chunking, per-device result table with timing, retry failed, saved command templates, and target by group/tag/filter instead of checkboxes only.

## 5. Device settings editor (`pages/device_config.php`, `pages/device_config_tabs/*`, `resources/js/device_config.js`)
- Tabs: General (`FriendlyName`, `LedState`, `PowerOnState`, `Sleep`), Network (`SSId1/2`, `Password1/2`, `Hostname`, `IPAddress1-4`, `WifiConfig`, `AP`, `NtpServer1`), MQTT (`MqttHost/Port/User/Password/Client/Fingerprint/Retry`, `Topic`, `GroupTopic`, `FullTopic`, `Prefix1-3`, `ButtonTopic`, `SwitchTopic`, `ButtonRetain`, `PowerRetain`, `SensorRetain`, `StateText1-4`, `TelePeriod`, `SetOption2/3/4/10/19`), Timers (`Timers`, `Timer1..16` JSON).
- Save sends `Backlog k v; k v; ...` with every non-empty POST field (empty values skipped; empty password fields skipped). Timers: `Timers <0|1>` then `Timer<i> {json}` per timer.
- Load issues many sequential commands with `sleep()` calls: `Status 0`, `NtpServer1`, `FullTopic`, `SwitchTopic`, `MqttRetry`, `SensorRetain`, `TelePeriod`, `Prefix1-3`, `StateText1-4`, `MqttFingerprint`, `Timers`, `Timer1..16`, plus `SetOption` decoding from `StatusLOG.SetOption[0]` via a hard-coded SetOption0-27 table.
- Rating: **REDESIGN**: replace the form editor with fleet-wide "apply settings / template" (named command sets applied to groups) and read-only drift view. Drop per-device timer UI (HA covers schedules) and the stale SetOption table.

## 6. Health monitoring (fleet health, PR #1603) (`src/Health/*`, `bin/health-collector`, `pages/health.php`, `pages/health_data.php`, `resources/js/health*.js`)
- Collector: separate CLI service (s6 in the image). Loop: every `health_http_poll_interval` s (default 60) polls each device sequentially with `Status 0`; between devices it services the MQTT client. MQTT: subscribes to `health_mqtt_subscription` (default `#`), handles only `.../LWT` (payload `Online` = up) and `.../STATE` (marks up, reads `Wifi.Signal` and `Wifi.RSSI`). Topic -> device match by configured topic or topic learned from `Status 0` (`Status.Topic`, skipping unresolved `%` placeholders); segment-bounded match so `kitchen` != `kitchenette`. Learns whether MQTT is enabled from `StatusMQT.MqttHost`; devices without MQTT are HTTP-only.
- State machine (`HealthState::resolve`): `online` (http up and (mqtt up or not expected)), `degraded_mqtt` (http up, mqtt down), `degraded_http` (mqtt up, http down), `offline` (both down and last_seen older than `health_offline_grace`, default 180 s; within grace the previous state is kept), `unknown`.
- Storage: SQLite `health.db` table `device_health` (latest state per device only, no history).
- UI: `/health` table with filters per state, search, last-seen relative times, RSSI bars, channel summary; `/health_data` JSON; dashboard counters on the start page; can be set as homepage; health badge in the device list.
- Rating: **KEEP + REDESIGN** (core). Add history (state transitions with timestamps), firmware version/uptime/BootCount/free heap/restart reason, offline alerts (webhook/MQTT/HA notify), and parallel polling. See `lessons.md` for collector flaws (sequential polling blocks MQTT keepalive, config read once at start).

## 7. Auth and access (`pages/login.php`, `src/Helper/LoginHelper.php`, `src/Helper/PasskeyHelper.php`, `pages/passkey.php`, `src/Helper/ProxyAuthHelper.php`)
- Local user/password (bcrypt via `password_hash`; legacy md5 hashes are upgraded on login); first-run registration form when no credentials exist; `login=0` disables auth; `NO_AUTH` env enables no-auth; reverse-proxy header auth (`TASMO_AUTH_HEADER` + `TASMO_TRUSTED_PROXIES`, based on `REMOTE_ADDR`).
- Passkeys (WebAuthn via `lbuchs/webauthn`): registration needs the local password (step-up), login by discoverable credential, all passkeys revoked when the password changes. Challenge in PHP session, TTL 120 s.
- Sessions: PHP file sessions in `data/sessions/`, 30-day lifetime, cookie `TASMO_SESSION` (HttpOnly, SameSite Lax, or None+Secure when `TASMO_ALLOW_CROSS_SITE_IFRAME`).
- Rating: **REDESIGN**. For a homelab behind Authentik/Traefik: OIDC or trusted-proxy header as primary, optional local admin + API tokens for automation. Passkeys are a nice-to-have (**DROP initially**). Never keep a "login off" mode without an explicit listen-address restriction.

## 8. Settings page (`pages/site_config.php`, `src/Config.php`)
- Groups: login/credentials, language, night mode (`auto` 18:00-08:59 / `always` / `disable`, user override in browser localStorage), refresh time, homepage (`start|devices|health`), scan ranges/port, request timeouts and concurrency, update behaviour, MQTT discovery/health settings, cache cleanup (sessions/i18n/firmwares/config/devices), "check for TasmoAdmin updates".
- Rating: **REDESIGN** to a small config file + UI for the few settings that matter (timeouts, concurrency, MQTT, OTA listener, channel/variant policy).

## 9. Misc
| Feature | Where | What | Rating |
|---|---|---|---|
| Self-update | `pages/selfupdate.php`, `src/SelfUpdate.php`, `src/Update/UpdateChecker.php` | Downloads a release zip (GitHub API `TasmoAdmin/TasmoAdmin`, uses `assets[1]`) and overwrites app files; disabled in Docker image | **DROP** (container image upgrade) |
| Changelog/release notes | `TasmotaHelper::getChangelog/getReleaseNotes`, `TasmoAdminHelper` | Renders Tasmota `CHANGELOG.md`/`RELEASENOTES.md` from raw.githubusercontent.com via CommonMark | **REDESIGN**: show only "what's in the target version" link / short notes in the update dialog |
| i18n | `lang/<code>/lang.ini`, `src/Helper/JsonLanguageHelper.php`, `/actions?i18n` | 12 languages: cs de en es fr he hu it nl pl ru zh_TW; ini -> JSON cache for JS | **DROP** at first (English-only, add later) |
| Night mode | `resources/js/nightmode.js`, `ViewHelper` | Time-based dark theme with manual override | **DROP** (use `prefers-color-scheme`) |
| PWA | `PwaHelper`, `service_worker.php`, `resources/js/service_worker.js`, `offline.php` | Manifest, service worker caching static assets only + offline page | **DROP** (optional) |
| Cache cleanup | `CacheCleanupHelper`, `actions.php clean` | Wipes sessions/i18n cache, firmwares, config (needs password), devices (also deletes key file) | **DROP** (dangerous), keep "clear firmware cache" |
| Device protection | `protect_on/off` flags | UI lock to prevent accidental toggling | **DROP** |
| Device password encryption | `DevicePasswordCipher`, `DevicePasswordKeyProvider` | AES-256-GCM, `enc:v1:` prefix, key from `data/.device-password.key` or env `TASMO_DEVICE_PASSWORD_KEY` | **KEEP** (use same primitive or OS secret store, see data-formats.md) |
| Response normalisation | `src/Tasmota/ResponseParser.php` | Handles old Tasmota JSON quirks (control chars, v5.10 `STATUS = {...}` multi-blob replies, v8.5 missing `}` for shutters), `StatusNET.IP` -> `IPAddress` alias (pre 5.12), localized ON/OFF statetext detection (EN/DE/PL) | **KEEP the idea** (tolerant parser), **DROP** pre-9 compat and localized state text |
| Tasmota GET quirk | `Sonoff::doRequest` | If a reply has `WARNING` (Weblog too low) it sets `Weblog 2` and retries once (not for Backlog) | **KEEP** (known Tasmota behaviour: command output via HTTP depends on WebLog level) |
| Timers editing | `Sonoff::getTimersConfig/saveTimers` | 16 timers via `Timers`/`Timer<i>` | **DROP** (HA/rules) |

## Tasmota command inventory (all used by the app)
`Status 0` (and via MQTT `STATUS 0`), `Power<N> toggle|0`, `Restart 1`, `Backlog ...`, `OtaUrl <url>`, `Upgrade 1`, `Upgrade` (query), `WebGetConfig <url>`, `GET /dl` (config download), `Weblog 2`,
`FriendlyName`, `LedState`, `PowerOnState`, `Sleep`, `SSId1/2`, `Password1/2`, `Hostname`, `IPAddress1-4`, `WifiConfig`, `AP`, `NtpServer1`, `MqttHost/Port/User/Password/Client/Fingerprint/Retry`, `Topic`, `GroupTopic`, `FullTopic`, `Prefix1-3`, `ButtonTopic`, `SwitchTopic`, `ButtonRetain`, `PowerRetain`, `SensorRetain`, `StateText1-4`, `TelePeriod`, `SetOption2/3/4/10/19`, `Timers`, `Timer1-16`.
HTTP is always `GET http://<ip>:<port>/cm?[user=..&password=..&]cmnd=<urlencoded>`.
