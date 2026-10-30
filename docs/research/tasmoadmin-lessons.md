# Lessons from TasmoAdmin (known bugs and design flaws)

*Historical research from before the build. KEEP/DROP ratings, "Avoid" lines and "should" statements are design input, not a description of what TasmoFleet does today.*

Paths relative to `ta-full/tasmoadmin/`. "A" items were hit in production on this fork (commit refs from `git log`); "B" items were found while reading the code (not reproduced unless stated).
Each item ends with **Avoid:** how tasmofleet should do it differently.

## A. Flaws we hit

### A1. Browser-side update orchestration
`resources/js/device_update.js` runs the whole OTA state machine in the open browser tab: `OtaUrl`, `Upgrade 1`, sleep 10 s, poll `Status 0` up to 15 times, for all selected devices at once (`Promise.all`).
- Closing/reloading the tab, a laptop sleeping, or a proxy timeout aborts the run. If it happens between step 1 (minimal) and step 2 (final) the device is left on the minimal firmware (no web UI features, no sensors) with nobody finishing the job.
- All devices fetch firmware simultaneously from one small server; no concurrency cap, no ordering, no canary.
- Result log exists only in the DOM. No history of who was updated to what and when.
- `fetchWithRetries` retries any failed POST up to 15x including non-idempotent commands (`OtaUrl`, `Upgrade 1`); a lost response can start an upgrade twice.
- Success is inferred from the version string alone, not from a verified reboot.
**Avoid:** server-side persisted job queue (per device phases: backup -> set OtaUrl -> upgrade -> wait for reboot (uptime reset/BootCount change) -> verify version/variant -> done/failed), bounded concurrency, resume after restart, retries only for idempotent reads, full audit/history, the UI just observes.

### A2. `Status 0` is gone from minimal firmware since Tasmota 14.4.0
After the minimal step `Status 0` returns `{"Command":"Unknown"}`; the old code retried until the device was marked failed (fix 42ade47). Workaround: send `Upgrade` with no argument and parse `{"Upgrade":"1 or >X.Y.Z to upgrade"}` to get the running version (`device_update_logic.js: parseUpgradeQueryVersion`), then synthesize `X.Y.Z(minimal)`.
**Avoid:** model the device state machine explicitly (`normal` / `minimal` / `unreachable`); for liveness after reboot use a command that exists everywhere (e.g. `Upgrade` query, `Status 2`, plain HTTP GET `/`) and treat "Unknown" as a signal that the device runs minimal. Keep a per-version capability table and add a test fixture per Tasmota generation. Prefer `Status 2` (version only) over `Status 0` (large reply) for polling. Consider avoiding the minimal hop where the device has enough flash (ESP32 never needs it; many 1 MB ESP8266 need it) - decide per device from `Status 2` `StatusFWR`/free flash (`Status 4` `FlashSize`, `ProgramFlashSize`, `FreeSketchSpace`).

### A3. Mixed-content HEAD check
`update_fe_check` made the browser send `HEAD http://<ota>/...`; on an HTTPS install the browser blocks it, the update aborted with "Failed to fetch" (fix cd4842d). The server-side replacement (`OtaHelper::checkFirmwareUrl`) is the right shape, and its allow-list (only URLs the helper itself builds) is a good anti-SSRF pattern.
**Avoid:** all device/OTA-side network checks happen server-side. The OTA URL given to the device must be plain HTTP (ESP8266 cannot do modern TLS; Tasmota OTA over HTTPS needs special builds), so the UI can be HTTPS while the OTA listener is a separate HTTP port restricted to the IoT VLAN.

### A4. PHP session files, health check, chmod and the s6 timeout
- The Docker health check did `curl /` which starts a PHP session on every probe: one empty `sess_*` file per 30 s kept for 30 days (about 86k files) (fix fd409fd). 
- The init script then did one `chmod` per file: ~23 s for 18k files, over s6's 5 s oneshot limit, so php-fpm never started and nginx returned 502 (fix a9278e7).
- The same blanket chmod made `sess_<id>` files (file name = login cookie, valid 30 days) world-readable to other host users/SMB (fix 59390eb).
**Avoid:** no session files. Use signed/encrypted cookies or an in-memory/SQLite session store with expiry and cleanup; the health endpoint must not create state. One static binary, no init-time `chmod -R` over data; create files with the right mode at write time (`0600`). Document the data dir ownership (single user/UID) instead of fixing perms on every start.

### A5. `force_upgrade` vs `update_newer_only`
Two global settings interacted wrongly: with both enabled, a device already on the target version was refused as a "downgrade" (fix 00975d9). Logic is split across `getPreUpdateDecision` in JS and flags in `MyConfig.json`; decision text is in i18n keys.
**Avoid:** one explicit per-run policy enum (`only-newer` | `force-reinstall` | `allow-downgrade`) with a single decision function and table-driven tests (current, target, minimal, policy). Show the planned action per device (skip/upgrade/reinstall/downgrade) before starting (dry run).

### A6. Security issues fixed on this fork (settings, passkeys, self-update)
- Any logged-in session (including one granted by trusted-proxy header) could change password/username or turn login off in settings, and wipe config via `clean=config` which resets to open first-run registration (fix 298c4c5). 
- Any logged-in session could register a passkey = lasting login that bypasses the proxy (fix 147eb18); passkeys were not revoked on password change.
- Self-update accepted any POSTed `release_url` and overwrote application code (guarded by `isUnfamiliarUpdateSource`, refused in Docker 3db788a).
- Device values were not HTML-escaped in several pages (0e39b4b, c004fcf).
**Avoid:** capability/role model from day one: sensitive operations (credential changes, token creation, restore, wipe, free-form commands) need re-authentication or a separate admin role; no self-update; auth changes are impossible through a proxy-header session; escape by default (Go `html/template`), never concatenate HTML in handlers.

### A7. HTML-escaping in `Config::read` vs raw write
`Config::read()` returns `htmlspecialchars(value)` (`src/Config.php`) but `write()` stores raw. Any read -> write round trip double-escapes (e.g. `pages/upload.php` re-saves `ota_server_ip/port` from `$Config->read(...)`), and non-HTML consumers get escaped strings (URLs, file names, `User-Agent`). The health collector had to use `readAll()` with a comment "NOT htmlspecialchars'd". Same class of bug in `pages/devices_autoscan.php`: `htmlspecialchars($_POST['device_password'])` before saving, so a device password containing `& < > " '` is stored HTML-escaped and then fails authentication against the device.
**Avoid:** store raw, escape only at output (template layer), typed config struct (no stringly `'0'/'1'` values), validation on input, no escaping helpers in the data layer.

## B. Further flaws found while reading

### B1. Concurrency and storage
- `devices.csv` and `MyConfig.json` are read-modify-write with no locking (`DeviceRepository::updateDevice/removeDevices/rewriteRows`, `Config::writeAll`): two simultaneous saves lose one update. `addDevices` appends with a plain `fopen('a')` after computing the id from a full read (duplicate ids possible).
- Reads cause writes: `getDevices()` rewrites the CSV for migrations (short rows, plaintext passwords). `Sonoff::getDevices()` never resets its `$update` flag, so once one device had position 0 every later device triggers `setDeviceValue` = a full CSV rewrite each (O(n^2) file rewrites on a read path).
- Devices keyed by user-visible `position` in `Sonoff::getDevices`; collisions silently renumber.
- Name lists are `|`-joined in one CSV cell (a `|` in a name corrupts it); ids are `max+1` (reused after deleting the last).
- `Config` constructor writes the git tag/branch to the config file on startup of every request when changed; error paths `exit(var_dump(debug_backtrace()))` (leaks paths).
**Avoid:** SQLite (single writer, WAL, migrations) for devices, settings, jobs, backups metadata, health history; stable surrogate id plus MAC as identity; transactions.

### B2. Device credentials: storage and exposure
- At rest: AES-256-GCM (`src/DevicePasswordCipher.php`) with the key in `data/.device-password.key` next to the CSV (or env). Protects a leaked CSV only; the key lives on the same volume, and `clean=devices` deletes the key file.
- In use: the decrypted password is rendered into the HTML of the device list and health pages as `http://user:PASSWORD@ip:port` link (`Device::getUrlWithAuth` used in `pages/elements/devices_table.php:143` and `pages/health.php:146`), so every page view ships every device password to the browser (history, proxy logs, devtools).
- On the wire: commands use `?user=..&password=..` query parameters (`Sonoff::buildUrl`), which end up in Tasmota's and proxies' logs and in Guzzle exception messages. `Sonoff::doAjax` returns `$exception->getMessage()` to the browser as `ERROR`; Guzzle messages contain the full request URL, i.e. the password. (Backup uses HTTP Basic auth in `buildBasicAuthUrl`, so Tasmota accepts both.)
- `buildBasicAuthUrl` (used for `/dl` backup) omits `:port`, so backup of devices on non-80 ports hits the wrong port.
**Avoid:** never send device passwords to the browser; send auth via `Authorization: Basic` header; redact URLs/credentials in all errors and logs; keep the key outside the data dir (env/file with separate permission, or OS keyring/systemd credential) and support key rotation; use a single credential profile per group of devices (most fleets share one) instead of per-device cells.

### B3. Free-form command and mass-assignment surfaces
- `/actions doAjax` forwards any `cmnd` to any device (`Sonoff::doAjax`); the batch "command" box does the same for N devices and echoes raw JSON. By design for an admin tool, but there is no audit, rate limit, dry run, or "dangerous command" list (`Reset`, `Upgrade`, `OtaUrl`, `Backlog` with `SSId`/`Password`).
- `pages/device_config.php` builds `Backlog k v; ...` from **every** posted field (keys unchecked), so a crafted POST can execute any Tasmota command, and a `;` inside a value injects further commands. The success banner prints the Backlog unescaped (`$msg .= '<br/> '.$backlog;`), including `Password1/2` and `MqttPassword` values, and is an XSS sink.
- `pages/site_config.php` passes the whole `$_POST` to `Config::writeAll` (arbitrary keys incl. `ota_server_ip`, `mqtt_discovery_host`, `health_mqtt_subscription`).
- `device_config.php` does `sleep()` calls (1 to 4 s each, plus `count($settings)`) inside the request, and dereferences `$device` without a null check.
**Avoid:** typed API endpoints with explicit field allow-lists and validation; free-form commands only for an admin role, logged with user, target set, time and result; split `;` safely (reject unless `Backlog` is intended); no blocking sleeps (use async jobs).

### B4. SSRF and network scanning
- `device_action` "search" and autoscan send GET requests to any IP:port from the server, no restriction to private ranges (loopback, link-local, cloud metadata reachable). `IpHelper` caps at 1024 addresses but not at allowed networks; the MQTT discovery host/port come from the POST. `pages/selfupdate.php` downloads from a POSTed URL.
- Responses from probed hosts are parsed and partly reflected (the raw body is concatenated into `ERROR` with `<br/>` markup by `ResponseParser`, then echoed unescaped in `device_action.php` `$msg`): a hostile device on the LAN can inject HTML.
**Avoid:** scan only configured CIDRs (default RFC1918 of the site), deny loopback/link-local/multicast/metadata, cap rate, run scans as cancellable background jobs with streamed results; treat all device output as untrusted text.

### B5. Authentication and sessions
- No login rate limiting/lockout. Login accepts `$_REQUEST` (cookies, GET). CSRF check is skipped for the login route. First-run "register" form is open to whoever reaches the app first, and re-opens whenever the credentials are empty.
- `login=0` and env `NO_AUTH` disable auth entirely; the proxy-header mode accepts any non-empty header value from a trusted IP and has no notion of user/role.
- Legacy md5 password hashes still accepted (upgraded on login).
- Passkey rpId comes from the request Host header; behind a proxy the visible host may differ. `PasskeyRepository` writes via a fixed `.tmp` file without cross-process locking (lost updates possible).
**Avoid:** OIDC (Authentik) or trusted header with explicit user/group mapping as primary; bootstrap admin via CLI/env token, not an open web form; throttle login; API tokens (hashed, scoped, revocable) for automation; no auth-off switch without bind-to-localhost.

### B6. Firmware handling
- Source is HTML scraping of ota.tasmota.com (fixed column indexes 2/4/6, `h2` regex `((\d+\.)+\d)`); a layout change breaks the settings page too, because `site_config.php` fetches both indexes on every render (slow page, outbound calls on each open).
- One variant per platform; ESP32 sub-chips (c3/s2/s3/solo1) and flavors (lite, sensors, ir, knx, zbbridge, ...) cannot be mixed; the browser guesses platform from `StatusFWR.Hardware`/version text.
- Downloads are not verified (no size, gzip/ESP image magic, checksum); uploaded type is checked from the client-supplied MIME; fixed file names (`tasmota.bin`); full-size limit 5 MB, minimal 502000 bytes (hard-coded).
- Every POST to `/upload` wipes `data/firmwares/` first, so a second user starting an upload/download deletes files an update run in progress is still serving.
- ESP8266 legacy guard: automatic source + running version < 9.1.3 is blocked ("legacy path required"); manual source is not guarded.
**Avoid:** versioned content-addressed firmware cache (`<channel>/<version>/<variant>.<ext>` with SHA256), GC only of unreferenced versions; choose image per device from `Status 2`/`Status 1`; verify format before offering; index fetched on a schedule and cached, with a JSON/GitHub-API fallback; capability table for upgrade paths (min safe versions) enforced for all sources.

### B7. Health collector
- Polls devices sequentially with `Status 0` (default timeouts 5+5 s): 20 offline devices = 100-200 s per sweep, during which the MQTT client is only serviced between devices; broker keepalives and LWT handling stall. `Status 0` is also the heaviest reply.
- HTTP "up" means "no `ERROR` field": a wrong password returns `{"WARNING":"Need user=..."}` (HTTP 200, fixture `tests/fixtures/response-unauthorized.json`), which the code counts as up. Offline device state is only "not ERROR".
- Settings are read once at start (apart from the idle loop), so changing the poll interval, grace or MQTT broker requires restarting the collector.
- Stores only the latest row per device: no history, no transitions, no firmware version/uptime/heap, no alerting.
- `rssi` column holds Tasmota's quality percent and `signal` dBm (naming inverted from convention).
**Avoid:** goroutine-per-device pollers with jitter and per-device backoff, one MQTT connection owned by one component, hot-reloaded config, distinguish "unreachable" / "auth failed" / "answers but unhealthy", persist time series and state transitions (SQLite), notify on transitions (webhook/MQTT/HA), record firmware, uptime, boot count, restart reason, free heap, RSSI.

### B8. Backup/restore
- Backup keeps only the latest run (`createCleanBackupDir` removes the previous set), has no schedule, timestamp, retention, device-version metadata, or integrity check; only `ConnectException` is caught, so an auth failure (4xx `ClientException`) or timeout aborts the whole run with a 500 instead of a per-device failure.
- The `.dmp` is opaque; no way to see what changed between backups.
- Restore points the device at `actions?downloadRestore=<token>` on the authenticated `/actions` route. A Tasmota device has no session cookie, so with login enabled this download appears to be answered 401 and the restore cannot complete (inferred from `index.php` auth flow; only tested with login disabled/proxy-bypass). The "download backup" link is hard-coded `/actions?downloadBackup` ignoring `_BASEURL_`.
- Restore is single-device only and there is no automatic backup before an OTA run.
**Avoid:** backup as a first-class scheduled job, timestamped, retained N versions, stored with device id/MAC/version/hash; automatic pre-OTA backup; per-device error capture of any failure; restore served from the unauthenticated OTA listener with a one-time, short-lived token bound to device IP; multi-device restore; surface key settings from the dump for diffs.

### B9. Timeouts, execution model and UI coupling
- PHP request model forced hacks: `ini_set('max_execution_time', 180)` for scans, `session_write_close()` before AJAX so concurrent requests do not queue on the session lock, `sleep()` inside page handlers, i18n JSON cache regenerated in `bootstrap.php` on every request (`JsonLanguageHelper::dumpJson`).
- Default HTTP timeouts are 5 s connect/5 s total for every call, including slow commands (`Status 0` on a busy ESP, `WebGetConfig`).
- The UI polls `Status 0` for every row from the browser every 8 s, so N open tabs multiply device load; ESP8266 web servers are single-threaded and drop connections under load (this is also what makes concurrency 50 risky).
**Avoid:** long-running work in a daemon with a job table; one poller per device with a global cap; per-command timeout classes; cache latest status server-side and push to clients; cap concurrent requests per device to 1.

### B10. Tasmota API handling worth keeping
- `ResponseParser` tolerates malformed replies (control chars, Tasmota 5.10 `STATUS = {...}` blobs, 8.5.0 shutter reply missing a closing brace) and old key names (`StatusNET.IP`). Keep a tolerant JSON decoder but test against fixtures (`tests/fixtures/*.json`).
- `WARNING` with `Weblog` too low: HTTP command replies only contain output at WebLog >= 2, so the client sets `Weblog 2` and retries once (`Sonoff::doRequest`); not done for `Backlog`.
- MQTT discovery details are non-obvious and worth porting: replies to `STATUS 0` arrive as several `stat/<t>/STATUS`, `STATUS5`... messages that must be merged (`MqttDiscoveryService::mergeStatusPayload`); retained LWT arrives at subscribe time.
- IPv4 only (`ip2long`); identity by IP:port, so DHCP changes make devices "offline" until MQTT discovery rewrites the IP.
**Avoid:** identify by MAC (`Status 5`) and refresh IPs via MQTT/mDNS/ARP; support hostnames and IPv6.

## Summary: design rules for tasmofleet
1. Daemon + SQLite + persisted jobs; the UI is a thin observer.
2. All device I/O server-side, bounded, per-device serialised; credentials never leave the server; redact errors.
3. Explicit device model (MAC identity, variant, platform, state: normal/minimal/unreachable/auth-failed).
4. OTA as a verified pipeline: backup -> stage -> upgrade -> reboot check -> version/variant check -> history; policy enum per run; dry run.
5. Firmware cache with checksums and a separate unauthenticated OTA/restore listener on the IoT side.
6. Typed API + role/token auth + audit log; no mass assignment, no free-form Backlog from forms.
7. Raw storage, escape on output.
