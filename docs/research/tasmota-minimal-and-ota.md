# Tasmota minimal firmware + OTA flow (derived from source, tag v16.0.0)

Source: https://raw.githubusercontent.com/arendst/Tasmota/v16.0.0/tasmota/ ...
(support_command.ino, support_tasmota.ino, support.ino, settings.ino, xdrv_01_9_webserver.ino,
xdrv_02_9_mqtt.ino, include/tasmota_configurations.h, include/i18n.h, include/tasmota.h).
No device runs minimal at capture time, so everything in this file is **derived from source**
(marked "SRC") unless marked "OBS" (observed on the fleet) or "MEM" (from memory, unverified).

## 1. Version strings

| Where | Format | Example |
|---|---|---|
| `Status 2` -> `StatusFWR.Version` | `<ver>` + `(<image>)` + GetCodeCores() | `16.0.0(release-tasmota)` (OBS ESP8266), `16.0.0(release-tasmota32)` (OBS ESP32), minimal: `16.0.0(release-minimal)` (SRC) |
| image_name (SRC tasmota.ino:698) | `"(" TASMOTA_SHA_SHORT CODE_IMAGE_STR ")"`; release builds use SHA text `release-`, dev builds `<sha7>-` | `(release-minimal)`, `(85cff52-tasmota)` |
| CODE_IMAGE_STR | `tasmota` (default ESP8266), `tasmota32` (ESP32), `minimal`, `lite`, `sensors`, `knx`, `display`, `ir`, `zbbridge`, `mini-custom`, ESP32 minimal = `safeboot` image | |
| `Server:` HTTP header | `Tasmota/<ver> (<Hardware>)`, **no** image suffix | `Tasmota/16.0.0 (ESP8285N08)` (OBS) |
| HTTP footer (every page) | `Tasmota <ver> <image_name> by Theo Arends` | `Tasmota 16.0.0 (release-tasmota) by Theo Arends` (OBS) |
| `Upgrade` (no/invalid param) | `1 or >16.0.0 to upgrade` | `{"Upgrade":"1 or >16.0.0 to upgrade"}` (OBS) |
| `Upgrade 1` | `Version <ver> from <OtaUrl>` (ver WITHOUT image suffix) | `{"Upgrade":"Version 16.0.0 from http://host/tasmota.bin.gz"}` (SRC) |
| MQTT `tele/<t>/INFO1` | `{"Info1":{"Module":"..","Version":"16.0.0(release-tasmota)","FallbackTopic":"cmnd/DVES_XXXXXX_fb/","GroupTopic":"cmnd/tasmotas/"}}` (SRC, boot only) | |
| `tasmota/discovery/<MAC>/config` `sw` | bare version `16.0.0` (OBS) | |

Hardware field (`StatusFWR.Hardware`, and `Server:` header): OBS `ESP8266EX` (Gosund, NOUS), `ESP8285N08`
(Sonoff Basic, MagicHome LC04 - 1 MB flash), `ESP32-D0WDQ6 v1.0` (QuinLED). Others exist (`ESP32-S3 ...`, `ESP32-C3 ...`).
ESP8266 `Status 2` has `Boot`, ESP32 does not; ESP32 has `Core:"3.3.12"`, `SDK:"5.5.5"` vs ESP8266 `Core:"2.7.8"`, `SDK:"2.2.2-dev(38a443e)"`.
TasmoAdmin platform detection: `Hardware` contains `ESP32` or Version contains `(tasmota32` -> esp32, else esp8266.

## 2. Is minimal "really" minimal?  What is compiled in (SRC)

`FIRMWARE_MINIMAL` (tasmota_configurations.h:856-984) `#undef`s: light, energy, sensors, I2C/SPI, rules, scripts,
timers, USE_TASMOTA_DISCOVERY, USE_HOME_ASSISTANT, USE_DISCOVERY (mDNS), USE_DEVICE_GROUPS, emulation, ping, etc.

* **Webserver: YES** (`//#undef USE_WEBSERVER` is commented out).
* **MQTT: YES** (no `#undef` of MQTT; `USE_MQTT_TLS` kept). Minimal reads MqttHost/User/Pass from flash settings and connects:
  publishes retained `tele/<t>/LWT` `Online`, subscribes `cmnd/<t>/#`, publishes `tele/<t>/INFO1/2/3`
  (INFO1 Version `16.0.0(release-minimal)`), `tele/<t>/STATE` every TelePeriod (MqttShowState is not guarded).
  **No** `tasmota/discovery/...` publish (USE_TASMOTA_DISCOVERY undef) - the retained discovery message of the previous
  firmware stays on the broker unchanged (so `sw` in discovery still shows the OLD version while minimal runs). No SENSOR telemetry.
  MQTT commands (`MqttHost`, `MqttUser`, ... ) are `#ifndef FIRMWARE_MINIMAL` (xdrv_02_9_mqtt.ino:48) -> not available.
* Settings are **read** from flash but **never written** (see section 5).
* Relay GPIOs: the module/template is loaded from settings, so relays follow PowerOnState at boot; but there is no
  `Power` command, no `State`, no `Status`.

## 3. Command table in minimal (SRC support_command.ino:20-120)

Exactly these commands exist (everything else is behind `#ifndef FIRMWARE_MINIMAL`, and webserver/MQTT/driver
`FUNC_COMMAND` handlers are also excluded):

| Command | Alias for | Notes |
|---|---|---|
| `Upgrade` | CmndUpgrade | |
| `Upload` | CmndUpgrade (same function, response key echoes the typed name: `{"Upload":...}`) | |
| `OtaUrl` | CmndOtaUrl | |
| `SerialLog` | CmndSeriallog | |
| `Restart` | CmndRestart | |
| `WifiNoSleep` | SetOption127 synonym (first entry of table, `SO_SYNONYMS ... 127`) | calls CmndSetoptionBase; minimal behaviour unclear, ignore in simulator |

Everything else (`Status`, `State`, `Power`, `Module`, `Template`, `SetOption`, `Hostname`, `Backlog`, `TelePeriod`,
`Topic`, `Webserver`, `WebLog`, `Dimmer`, `Color`, `Gpio`, ...) -> **Unknown**.
(NB the TasmoAdmin consequence: `Status 0` on minimal returns Unknown, which breaks naive pollers. Changed in v14.4.0.)

### Unknown / Error response shapes (SRC support_command.ino:497-515, OBS on normal fw)
```
Command not in any table : {"Command":"Unknown","Input":"<CMD upper-cased>[idx][ <data as given>]"}
Known command, no output : {"Command":"Error","Input":"<same format>"}          e.g. normal fw: Status 9 -> {"Command":"Error","Input":"STATUS 9"}
cmnd= (empty)            : {"WARNING":"Enter command cmnd="}                        (HTTP only)
Backlog with nothing     : {"Backlog":"Empty"}                                      (normal fw only)
```
* Input = command name upper-cased, trailing digits split off as the index and re-appended if != 1, then `" " + data` if data present (JSON-escaped).
  `FooBar` -> `FOOBAR`; `FooBar2 x y` -> `FOOBAR2 x y`; `Status 9` -> `STATUS 9`.
* Also OBS: `FallbackTopic` is Unknown on this fleet's normal firmware (command not compiled).
* HTTP status is **200** for all of these (Content-Type `application/json`). Only wrong credentials give 401
  `{"WARNING":"Need user=<username>&password=<password>"}` (no web passwords on this fleet).
* Over MQTT the same JSON is published to `stat/<t>/RESULT` (OBS: the Unknown reply for `FooBar` sent via HTTP shows up in `mqtt/samples/stat-RESULT.txt`;
  with SetOption4=1 the topic would be `stat/<t>/COMMAND` for Unknown, SRC).

### Upgrade (SRC CmndUpgrade, support_command.ino:1331)
Condition: `(data_len==1 && payload==1)` **or** `(data_len>=3 && NewerVersion(data))`.
* accepted -> sets `ota_state_flag = 3`, response `{"Upgrade":"Version 16.0.0 from <OtaUrl>"}` (placeholders `%04d`/`%d` in OtaUrl are expanded with chip id).
* else -> `{"Upgrade":"1 or >16.0.0 to upgrade"}` (OBS). This covers: no param, `0`, `2` (ESP8266), `01`, `ON`, `16.0.0` (equal is NOT newer), `15.0.0`, `abc`.
* `Upgrade 17.0.0` (newer, >=3 chars, 2-4 dotted fields each 0-255) is accepted like `Upgrade 1`; the given version string is only checked for being newer, it does not select what is downloaded (the OtaUrl does).
* `Upgrade 2` is ESP32-only (single OTA partition + not on factory/safeboot partition): `{"Upgrade":"Safeboot"}`.
* `Upload [1|ver]` identical, key echoed as `Upload`.
* NewerVersion: fields split on `.`, packed into bytes (max 4 fields); a letter suffix after the 3rd field counts as 4th byte (`1.2.3a`).

### OtaUrl (SRC CmndOtaUrl)
* No param: `{"OtaUrl":"<stored url>"}` (OBS). With param: stores, responds with the new value (same JSON).
* Param `1` (single char `1`) = reset to compiled default `OTA_URL` (empty in my_user_config.h; official builds inject ota.tasmota.com URL via build flags - MEM).
* Value is limited by the settings text pool (OtaUrl max ~ 100-ish chars incl. pool sharing; `full_ota_url` buffer is 200).
* In **minimal** the new value only lives in RAM (section 5).

### SerialLog
No param/out-of-range: `{"SerialLog":{"2":{"Active":"0"}}}` shape = `{"SerialLog":{"<settings level>":{"Active":"<runtime level>"}}}` (OBS on normal fw; same code in minimal).
Valid values 0..4 (LOG_LEVEL_NONE..DEBUG_MORE). In minimal it only touches RAM/`Settings->seriallog_level` (not persisted).

### Restart
`Restart 1` -> `{"Restart":"Restarting"}`, `2` -> `{"Restart":"Halting"}`, `9` -> `{"Restart":"Go to sleep"}`, `99` restart immediately (no response),
any other incl. none -> `{"Restart":"1 to restart, 2 to halt"}`. ESP32 also `3` -> `{"Restart":"Switching"}`. Negative values (crash/WDT) not in minimal.

## 4. Web UI in minimal (SRC xdrv_01_9_webserver.ino)
Route table in minimal (WebServerDispatch, lines 598-621): `/` (root), `/up`, `/u1`, `/u2` (OPTIONS), `/u3`, `/mn`, `/cs` (console), `/cm`;
the `/cn /md /wi /lg /tp /co /dl /rs /rt /in` configuration pages are removed -> `404 File Not Found\n\nURI: /x\nMethod: GET\nArguments: 0\n` (Content-Type text/plain; same shape as on normal fw, OBS).
Additionally `/u2` POST (upload).
* `/` page: head style via `HTTP_HEAD_STYLE3_MINIMAL`: a **warning heading `MINIMAL firmware<br>please upgrade`** (HTML: `<h3>MINIMAL firmware<br>please upgrade</h3>`),
  then `<h3>Module name</h3><h2>Device name</h2>`, then buttons **Firmware Upgrade** (`/up`), **Console** (`/cs`), **Restart** only.
  No toggle buttons, no status table; no `Configuration`/`Information`.
* `<title>`: `<DeviceName> Main Menu` (same as normal, OBS "Prusa Plug Main Menu").
* Footer identical format with `(release-minimal)`.
* `/up`: form `GET /u1?o=<OtaUrl>` + file upload form (POST `/u2?fsz=`); `/u1` runs `OtaUrl <o>` (if non-empty) then `Upgrade 1` through ExecuteWebCommand and answers with the "Upgrade started ..." page whose JS reloads after `HTTP_OTA_RESTART_RECONNECT_TIME` = **24000 ms on ESP8266, 10000 ms ESP32**.
* `/cm` works exactly as on normal fw (200 JSON, same headers, `Server: Tasmota/16.0.0 (<hw>)`) but with the 5-command table above.
* `/cm` response body = concatenation of every JSON log line produced while the command ran, merged into one object: `{"a":..,"b":..}`.
  Commands producing several messages (e.g. `Status 0` produces STATUS+STATUS1..) are merged into one object (OBS: `Status 0` -> `{"Status":{..},"StatusPRM":{..},...}`).
* Headers (OBS, identical on all 6 devices): `HTTP/1.1 200 OK`, `Content-Type: application/json` (html pages: `text/html`), `Server: Tasmota/16.0.0 (<Hardware>)`,
  `Cache-Control: no-cache, no-store, must-revalidate`, `Pragma: no-cache`, `Expires: -1`, `Accept-Ranges: none`, `Transfer-Encoding: chunked`, `Connection: close`.

## 5. Why OtaUrl set while running minimal is NOT persisted (SRC)
`SettingsSave()` (settings.ino:729-738): the whole body is wrapped in `#ifndef FIRMWARE_MINIMAL ... #endif` -> **no-op in minimal**
(`SettingsSaveAll`, `SettingsSave(1)` in the OTA state machine, `save_data` timer, Restart all call it). `RtcSettings` etc. unaffected.
The `WARNING This version does not support persistent settings` is logged at boot of minimal (tasmota.ino:751).
Timeline of the stored OtaUrl for the TasmoAdmin two-step flow:
1. Normal fw: `OtaUrl .../tasmota-minimal.bin.gz` (RAM + will be flushed) ; `Upgrade 1` -> state 3->2 runs `SettingsSave(1)` **(persists minimal URL to flash)** -> flashes minimal -> restart.
2. Minimal boots (reads flash: OtaUrl = minimal URL). TasmoAdmin sets `OtaUrl .../tasmota.bin.gz` -> RAM only; `Upgrade 1` -> `SettingsSave(1)` is a no-op; flashes full fw; `restart_flag=5` -> `SettingsSaveAll()` no-op -> restart.
3. Full fw boots and reads flash: OtaUrl is still the **minimal URL**. This matches the observed fleet state (stored OtaUrl = tasmota-minimal.bin.gz after minimal-hop upgrades).
Contrast: Tasmota's own automatic fallback (ESP8266, `Upgrade 1` with a binary that is too large -> `HTTP_UE_TOO_LESS_SPACE`/`BIN_FOR_WRONG_FLASH`):
`RtcSettings.ota_loader=1`, URL rewritten at runtime `tasmota[-xx][.bin[.gz]]` -> `tasmota-minimal[...]`; after boot in minimal `ota_loader==1` -> `ota_state_flag=6` ("OTA: Propagating upload")
and minimal downloads the **stored** (original, normal) OtaUrl by itself. There the stored OtaUrl stays the normal one.
Note `RtcSettings` survives the software restart (RTC memory), so `ota_loader` crosses the minimal boot; with TasmoAdmin's flow `ota_loader` is 0 (reset at state 2), so minimal does NOT continue by itself.

## 6. OTA state machine and timeline (SRC support_tasmota.ino:1385-1545, 1565-1645)
Variables: `ota_state_flag`, `restart_flag`, `ota_retry_counter` (OTA_ATTEMPTS = 10 on ESP8266, 5 on ESP32), 250 ms tick, all `*_flag` decrements once per second and only when no backlog is pending (`CommandsReady()`).

```
t=0      HTTP /cm?cmnd=Upgrade 1  -> response {"Upgrade":"Version 16.0.0 from <url>"}  (+ MQTT stat/<t>/RESULT same JSON)  ota_state_flag=3
t~1 s    flag 3->2 : RtcSettings.ota_loader=0; ota_retry_counter=OTA_ATTEMPTS; SettingsSave(1) (flash write, normal fw only)
t~2 s    flag 2->1
t~3 s    flag 1->0 : download starts. BLOCKING in main loop: webserver, MQTT keepalive, timers are NOT serviced.
         url handling (ESP8266): ota_loader? -> -minimal rewrite; after OTA_ATTEMPTS/2 failed tries and url not .gz -> append ".gz"
         ESPhttpUpdate.update(client, url, "<ver><image_name>") ; HTTP GET with header x-ESP8266-version
         (gzip image: unzipped on the fly; ~ 0.5-1 MB: several seconds to ~20 s)
         on failure: ota_state_flag=2 -> retry after ~1-2 s, up to ota_retry_counter attempts (each blocks again)
success  flag=92 -> 91 -> 90 (2 s): publishes STAT message  stat/<t>/UPGRADE = {"Upgrade":"Successful. Restarting"}  (not retained)
         failure after all retries: {"Upgrade":"Failed <ESPhttpUpdate lastErrorString>"}; device stays on old firmware and keeps running
         then restart_flag=5 ("allow time for webserver to update console")
+5 s     restart_flag 5->4->3->2 (SettingsSaveAll, no-op in minimal) ->1->0 : EspRestart(): MQTT clean DISCONNECT, wifi shutdown
reboot   bootloader copies new image (ESP8266 ~1-3 s), Tasmota init, WiFi join (1-5 s), webserver up, MQTT connect (LWT Online, INFO1/2/3)
```
Typical total (OBS from the user): ~20-40 s from `Upgrade 1` to HTTP reachable again on ESP8266 (for the ~500 KB-1 MB gz images).
During the whole blocked/rebooting window `/cm` gets no answer (connect timeout or refused), not an HTTP error.
* TCP: the ESP8266 lwIP stack may still complete the 3-way handshake during the blocking download (queued, never answered) -> client sees read timeout; while rebooting: no ARP/host unreachable. A simulator should offer both "accept and hang" and "refuse/close" modes.
* Error strings (MEM, ESP8266httpUpdate::getLastErrorString): `HTTP error: connection refused`, `HTTP error: connection lost`, `File Not Found (404)`, `Forbidden (403)`, `Wrong HTTP Code`, `Not Enough space`, `New Binary Does Not Fit Flash Size`, `Verify Bin Header Failed`, `Update error: ...`.
* Failed upgrade does NOT reboot; the next `Upgrade` works again; `ota_retry_counter` is re-armed at state 2.
* MQTT view: no `tele/<t>/LWT Offline` is published by the device on a software restart (clean disconnect does not fire the will).
  The broker publishes the will only on an unclean drop (keepalive 30 s -> ~45 s): possible if the blocking download lasts > ~45 s.
* After reboot (normal fw) Info1/Info2/Info3 are published once per boot (not retained unless SetOption `mqtt_info_retain`), `RestartReason` = `Software/System restart`, Uptime restarts at 0, `BootCount` +1.
  (OBS fleet: RestartReason "Software/System restart" and BootCount 218 on Sonoff.)

### ESP32 differences (SRC)
* URL `tasmota32.bin` (no gz needed). Normal fw does NOT hop through minimal, unless `EspSingleOtaPartition()`: then it writes the factory flag, restarts into the
  safeboot image (minimal-like page, header "Safeboot"), which propagates the upload (`OtaFactoryRead` -> `ota_loader=1`) and the OTA continues from there.
* `HTTP_OTA_RESTART_RECONNECT_TIME` 10 s.
* FIRMWARE_MINIMAL on ESP32 is the safeboot variant; Status etc. likewise Unknown.

## 7. What TasmoAdmin does (upstream master, resources/js/device_update.js + device_update_logic.js)
1. `Status 0` (retry 15 x 10 s) to learn Version/Hardware -> platform (esp32 / esp8266).
2. Plan: if `minimalOtaUrl` configured (ESP8266): steps = [minimal, final]; ESP8266 `automatic` source with current < 9.1.3 is blocked ("legacy path required").
3. Each step: `OtaUrl <url>` (separate command via server-side `actions` proxy, not Backlog) -> `Upgrade 1` -> sleep 10 s -> loop up to 15 times: `Status 0`
   (each failing fetch is retried 15 x 10 s) until version changed (no targetVersion) or equals targetVersion (`shouldTreatStatusAsSuccessful`).
4. Known problem (>= v14.4.0): after the minimal step `Status 0` returns `{"Command":"Unknown","Input":"STATUS 0"}` -> JS throws -> step 2 never runs (a TasmoAdmin fork has a probe via `Upgrade`: `{"Upgrade":"1 or >16.0.0 to upgrade"}` + version from the web footer).
A robust manager should: poll `Upgrade` (no param) as liveness/version probe in minimal (returns the bare version number in the message); read the
footer (`GET /` -> `Tasmota 16.0.0 (release-minimal) by Theo Arends`) to learn image name; treat `{"Command":"Unknown"}` for `Status` as "device is on minimal".

## Relay state across an update (power restore)

Fleet policy is PowerOnState 0 and SetOption0 0 (no flash writes for relay
state), except the two ESP32 lamps (SetOption0 1).

**Observed on the fleet, 2026-10-19 (Home Assistant history of that morning's
updates):**

- `dehumidifier-plug` (NOUS A1T, ESP8266 1 MB, relay) was ON, went through the
  two-step update through minimal (four unavailable phases, 08:08:59 to
  08:11:23) and came back ON every time. **A relay keeps its state across the
  minimal hop**; the RTC restore works there. The earlier source-based
  assumption that minimal loses the state was wrong.
- `balcony-windowsill` (MagicHome LC04, ESP8285, PWM light, no relay) was ON;
  after the last reboot HA saw ON and 4 ms later OFF (06:29:24.357 on,
  .361 off). **A light driver switches itself OFF right after the boot**
  (PowerOnState 0 applied by the light module). Lights lose their state,
  relays do not.
- `office-monitor-example` (ESP32, SetOption0 1) came back ON.

What this means for the OTA power restore:

- **Only light channels are restored by default.** A light is recognised by
  Dimmer, Color or CT in Status 11 (its channel is the last POWERn). Relays
  keep their state across an update on this fleet, so a relay that is OFF
  after the boot was most likely switched off by someone; restoring it would
  override them. Plain relays are restored only on request (per batch, "Power
  after update" = "Restore lights and relays"; the default is "Restore lights"). The preview names the relays
  that are left alone.
- The reading after the final boot has to be taken after the device settled
  (a light reports ON for a moment): the engine reads until two consecutive
  readings, one probe interval apart, are equal; a state that keeps changing
  is not acted on.
- Dimmer, colour and colour temperature of a light are not restored, only
  Power (ON); the light comes back with the values it saved.
- The simulator implements exactly these rules (`DeviceSpec.Light`,
  `DeviceSpec.SetOption0`, `internal/simulator/ota.go`), so its tests prove
  the engine against the observations, not against the real devices.
- Known residual windows (a light is OFF after the boot either way, so the
  protection there is the readings and the MQTT reports): between the second
  reading and the moment the device blocks for the download (the `Upgrade 1`
  round trip and Tasmota's few-second countdown); on the staged path the time
  in minimal (no MQTT there); on HTTP-only devices or with the bus down,
  where nothing but the readings sees a user's OFF; and a restart of
  tasmofleet inside the maximum age: the resumed job reads the state afresh,
  so an OFF that happened while it was down looks like a lost state.
- MQTT "switched off" reports count by the time they were received: after the
  state was read and before the device first went offline for the update.
  Earlier ones are old news, later ones are the boot announcements.
- The restore never overrides a user's OFF: the state is read in preflight,
  again right before the first Upgrade and (settled) right after the final
  boot; a relay that was ON in ANY reading after the boot was kept by the
  device and is never touched; relays with PulseTime, Interlock or a shutter, relays whose state
  is neither ON nor OFF, and a device whose MAC differs from the inventory
  are never restored (`PulseTime`, `Interlock`, `ShutterRelay` bare queries
  are pure reads in Tasmota, checked in `support_command.ino` and
  `xdrv_27_shutter.ino`); a state older than `OTA_POWER_MAX_AGE` (20 min) is
  not acted on. While the device downloads it cannot be switched by anyone
  (the download blocks its main loop), so the remaining window is short: after
  the second reading until the download starts, and between the post-boot
  reading and the switch.

## File upload on an ESP32 with the safeboot layout (SRC v16.0.0, OBS 2026-10-24)

OBS: office-monitor-thomas (ESP32-D0WDQ6, 4 MB, 16.0.0 release-tasmota32; Status 4 ProgramSize 2182,
Free 697, ProgramFlashSize 4096) answered a push of `tasmota32.bin` (2234912 bytes) with "Upload Failed
... Not enough space". Nothing was flashed, the device did not restart.

SRC (`tasmota/tasmota_xdrv_driver/xdrv_01_9_webserver.ino` unless noted):

* Layout: `EspSingleOtaPartition()` = one app partition (`support_esp32.ino:253`), i.e. `safeboot`
  (factory) + `app0`. Status 4 `Free` is `ESP_getFreeSketchSpace()/1024` (`support_command.ino:1034-1035`);
  on this layout it is the rest of the running partition (`support_esp32.ino:478-486`: partition size minus
  sketch size), so it is always smaller than a full image.
* `HandleUploadLoop`: an `0xE9` image is refused with error 4 when `fsz` > `ESP.getFreeSketchSpace()`
  (3676), then `Update.begin((ESP.getFreeSketchSpace() - 0x1000) & 0xFFFFF000)` fails with error 2
  "Not enough space" (3681-3683), as does a short `Update.write` (3716). The image must therefore fit
  `Free * 1024 - 4096`; tasmofleet's push preflight uses exactly that bound (`ota.pushSpaceRefusal`).
* The web UI on this layout: `HandleUpgradeFirmware` sends `HTTP_FORM_RST_UPG_FCT` instead of
  `HTTP_FORM_RST_UPG` when `EspSingleOtaPartition() && !EspRunningFactoryPartition()` (3420-3421). The
  form has the same `action='u2?fsz='` and file field (382-396), so the `/up` form check does not tell
  the layouts apart; its button calls `upl(this)`.
* `upl()` (`html_uncompressed/HTTP_HEAD_LAST_SCRIPT32.h:28-47`) reads the file's first byte: `0xE9`
  (a full, uncompressed image) goes through `fct()`, anything else is posted directly. `fct()` (48-65)
  polls `GET /u4?u4=fct&api=`: `false` = a restart into safeboot was triggered, poll again in 6 s (2 s
  after a network error); `true` = running safeboot now, post the form to `/u2` there.
* `/u4` = `HandleSwitchBootPartition` (route 604, handler 3845-3895): with `u4=fct` on the single-OTA
  layout and not in safeboot it saves the settings, erases `otadata` (`EspPrepRestartToSafeBoot`,
  `support_esp32.ino:266`), answers `false` (3880) and restarts. In safeboot (or on a dual-OTA layout) it
  answers `true` (`none` without a factory partition, 3889). Safeboot's own upload writes `app0` and
  reboots into it.
* The download path (`OtaUrl` + `Upgrade 1`) on this layout already restarts into safeboot, which
  downloads and flashes `app0`; it needs no extra step from tasmofleet.

Consequence: a push to this layout needs a restart into safeboot first (a separate device action with its
own failure mode: a device left in safeboot). tasmofleet does it as the push into safeboot (#30, design
below) when `GET /up` shows `upl(this)`. "Push where possible" uses the download for any other device whose
Status 4 `Free` is too small for the image (and for an ESP32 whose `/up` cannot be read), and a refusal by
the device ends the job at once.

### Telling the layouts apart from `GET /up` (research for #30, SRC v16.0.0, no device)

Verified by reading the v16.0.0 tag (`1c21739`), `xdrv_01_9_webserver.ino` and
`html_uncompressed/HTTP_HEAD_LAST_SCRIPT32.h`:

* The only upload form with a call of `upl(` is `HTTP_FORM_RST_UPG_FCT` (line 382; `return upl(this);` at
  line 391). The dual-OTA/ESP8266 form `HTTP_FORM_RST_UPG` (lines 367-380) has an `onclick` that only hides
  `f1`, shows `f2`, appends the file size and calls `this.form.submit()`; it contains no `upl(`.
  `grep -rn "upl(this)"` over the whole tag finds line 391 and one coding guide, nothing else. The
  script that defines `function upl(t)` (SCRIPT32.h:28) says `upl(t)`, never `upl(this)`, so the string
  `upl(this)` occurs on the page only in the form's button.
* `HandleUpgradeFirmware` (3406) picks the form at 3420: `EspSingleOtaPartition() && !EspRunningFactoryPartition()`
  gives the FCT form, everything else (dual layout, ESP8266, **or the same single-OTA device while it runs
  safeboot**) gets `HTTP_FORM_RST_UPG` (3423, 3426). (`HTTP_FORM_RST_UPG` is also the restore form at 3044,
  not on `/up`.)
* So `upl(this)` on `/up` means exactly: "single-OTA (safeboot) layout, normal firmware running; a file
  upload needs the restart into safeboot first". Its absence means a dual layout (direct upload) **or**
  safeboot itself; the two are told apart by the running image (Status 2 Unknown / image `safeboot`), never
  by the form. The marker is read-only (a plain GET of `/up`, which the upload does anyway).
* `/u4` has no side effect on the "wrong" side: `u4=ota` while running the normal firmware on a single-OTA
  device does nothing and answers `true` (3856-3862); `u4=fct` in safeboot answers `true` and restarts
  nothing (3864-3866, 3889). Only `u4=fct` from the normal firmware restarts (3880), after sending `false`.
  tasmofleet still sends `fct` exactly once and never polls with it (the browser's `fct()` re-sends it every
  6 s; that is safe there only because the second call arrives in safeboot).
* `upl()` only takes the safeboot route for a file that starts with `0xE9`; a gzip (`0x1F`) is posted
  straight to the running firmware. A tasmofleet safeboot push therefore refuses an image that does not start
  with `0xE9`.

Design of the safeboot push (tasmofleet, #30): `GET /up` shows `upl(this)` -> job state `safeboot_switch`:
Guard, one `GET /u4?u4=fct&api=`, probe until the device answers as safeboot; Guard again, one upload to
`/u2` there, then the normal `await_reboot`/`verify`. A job that ends with the device in (or possibly in)
safeboot goes to `safeboot_recover`: it probes first and sends one `GET /u4?u4=ota&api=` only when a probe
answers from safeboot (`recovery_step` is stored before the request goes out), then probes again; if that
does not bring the old version back the job fails with the address so the user can finish by hand.

#### Real-device test of the safeboot push (office-monitor-thomas, ESP32, 16.0.0, 2026-10-29)

Build of PR #190 (0590da9) with the gate opened locally.

* R0 (read-only): `/up` has `upl(this)` and `action='u2?fsz='`; Status 4 `Free` 697, `ProgramFlashSize` 4096.
* R1, reinstall 16.0.0 by push: succeeded, 52 s. States: queued, preflight, backup, set_ota_url,
  safeboot_switch, upgrade_issued, verify, succeeded. `await_reboot` was skipped: the first probe after the
  upload hung in a TCP retry until the app was up and saw the new uptime (Status 11 `UptimeSec` 4), so the
  job went straight to verify. Device commands: exactly one `GET /u4?u4=fct&api=` (answer `false`), the
  probes (Status 2 `{"Command":"Unknown"}`, the bare `Upgrade` read probe, `/`), `/up` (plain form),
  exactly one `POST /u2?fsz=2234912`, verify. No `Upgrade 1`, no OtaUrl set. Relays off before and after.
* Timings: `/u4` reply to the first safeboot answer 5.7 s; upload about 30.6 s; upload reply to the first
  answer of the app 7.3 s; `/u4` to app 44.8 s; whole job 52 s.
* R2, cancel 0.1 s after the `/u4 fct`: ended cancelled, exactly one `/u4?u4=ota` (on the first probe in
  `safeboot_recover`), no upload, app back on 16.0.0 after 10.7 s, text "Upload into safeboot failed; the
  device was switched back to 16.0.0 (cancelled while the device switched into safeboot)", `recovery_step` 2.

`SafebootWindow` stays at 2 min (measured about 6 s and 11 s).
