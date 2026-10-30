# tasmofleet fleet capture + simulator behaviour spec

Captured 2026-10-18 ~18:17-18:25 CEST from the live fleet (Tasmota 16.0.0, all normal firmware), strictly read-only
(query commands only, <=2 devices in parallel, 0.4 s between requests, `nice`). Minimal-firmware behaviour is derived from
source, not captured: see `docs/research/tasmota-minimal-and-ota.md`.

## 1. Devices captured

| dir (= MQTT topic) | model (Module) | IP | chip / Hardware | notes |
|---|---|---|---|---|
| `livingroom-readinglamp` | Sonoff Basic (module 1) | 172.31.13.31 | ESP8285N08, 1 MB | relay only, no energy |
| `kitchen-spotlight` | Gosund SP111 (module 0) | 172.31.13.28 | ESP8266EX | energy plug (BL0937), `StatusPTH` present |
| `mr-bambu-plug` | NOUS A1T (module 0) | 172.31.13.33 | ESP8266EX | energy plug, `Period` key in tele SENSOR |
| `bedroom-cabinet` | MagicHome RGBW IR LC04 (module 0, template) | 172.31.13.23 | ESP8285N08 | light: Dimmer/Color/HSBColor/White/CT/Channel/Scheme/Fade/Speed/LedTable in STATE |
| `office-monitor-guest` | QuinLED Dig Uno v2r6b (module 0, template) | 172.31.13.35 | ESP32-D0WDQ6 v1.0, 4 MB | tasmota32, Berry, DS18B20 + ESP32 temp in SENSOR, OtaUrl `.../tasmota32.bin` |
| `prusa-plug` | NOUS A1T | 172.31.23.10 (workshop subnet) | ESP8266EX | energy plug |

`fleet-inventory.json` = all 20 devices from retained discovery (topic, name, ip, model, sw, mac). 7 Fairylights were not
checked (offline), only 20-21 retained discovery messages exist (`mqtt/discovery-retained.jsonl`).

## 2. File layout

```
README.md                    this file (the minimal firmware / OTA research is docs/research/tasmota-minimal-and-ota.md)
fleet-inventory.json         inventory of all discovered devices (sanitized MACs)
firmware-versions.json       StatusFWR.Version shapes with their source (capture, Tasmota source line, field report) and the expected identity
devices/<topic>/             one dir per captured device
   status-0.json ... status-11.json   `Status 0` and `Status N` individually (Status 12 skipped)
   upgrade.json   `Upgrade` without param          otaurl.json   `OtaUrl` without param
   state.json module.json modules.json template.json hostname.json topic.json power.json teleperiod.json
   setoption-0.json setoption-65.json seriallog.json webserver.json fallbacktopic.json loglevel.json (=WebLog)
   gpio.json gpios.json energy-status.json (=Status 10 again)
   unknown-foobar.json   `FooBar` -> unknown shape      empty-cmnd.json  `cmnd=`      backlog-empty.json
   <name>.headers        raw HTTP response headers of every request
   root.html root.headers root-title.txt root-footer.txt   GET /   (footer = version/image string)
   notfound.body notfound.headers   GET /nonexistent (404)         up.html up.headers   GET /up (upgrade form)
mqtt/capture.txt             ~6.7 min of `tele/#`, `stat/#`, `tasmota/discovery/#`; format  time|retained|topic|payload
mqtt/discovery-retained.jsonl  retained discovery configs seen at connect
mqtt/samples/<type>.txt      first sample per topic per message type
mqtt/synthetic-no-discovery.txt  SYNTHETIC (fake values): devices without tasmota/discovery (LWT, INFO1..3, STATE, STATUS5 incl. Ethernet)
profiles/<name>/             SYNTHETIC: `profile.json` = a real capture (`base`) plus text replacements, read by internal/simulator (most have a README)
firmware-headers/            first 32 bytes of the v16.0.0 release images (chip ids; see its README)
firmware-upstream/           real ota.tasmota.com listings and a reduced GitHub releases reply for internal/firmware (see its README)
tasmoadmin/                  SYNTHETIC TasmoAdmin sample data (devices.csv, MyConfig.json) for the import
```
Raw files contain exactly what the device sent (JSON on one line, no trailing newline), after sanitising (section 7).

## 3. Observed API facts (OBS)

* `GET http://<ip>/cm?cmnd=<urlencoded>` -> always `200`, `Content-Type: application/json`, body = single JSON object.
  Headers: `Server: Tasmota/16.0.0 (<Hardware>)`, `Cache-Control: no-cache, no-store, must-revalidate`, `Pragma: no-cache`, `Expires: -1`,
  `Accept-Ranges: none`, `Transfer-Encoding: chunked`, `Connection: close`. No `Content-Length`.
* `Status 0` -> one object with keys in this order: `Status, StatusPRM, StatusFWR, StatusLOG, StatusMEM, StatusNET, StatusMQT, StatusTIM,` [`StatusPTH` only on energy devices]`, StatusSNS, StatusSTS`.
  `Status N` returns just the one sub-object: 1 `StatusPRM`, 2 `StatusFWR`, 3 `StatusLOG`, 4 `StatusMEM`, 5 `StatusNET`, 6 `StatusMQT`, 7 `StatusTIM`,
  8 `StatusSNS`, 9 `StatusPTH` (energy only; else `{"Command":"Error","Input":"STATUS 9"}`), 10 `StatusSNS` (identical to 8), 11 `StatusSTS`.  (`Status` without argument was not captured; per source it returns the `Status` object only.)
  `Status 12` skipped.
* `Upgrade` -> `{"Upgrade":"1 or >16.0.0 to upgrade"}`; `OtaUrl` -> `{"OtaUrl":"<url>"}`.
  Stored OtaUrl values seen: ESP8266 `http://tasmoadmin.example.internal:80/data/firmwares/tasmota-minimal.bin.gz` (leftover of minimal hop), ESP32 `.../tasmota32.bin`.
* `Module` -> `{"Module":{"<id>":"<name>"}}` (Sonoff Basic id 1; NOUS A1T / Gosund SP111 / MagicHome / QuinLED show id 0 because they are template modules with BASE != module id; see `template.json`).
  `Modules` = full table of ids (ESP8266 list).  `Template` -> `{"NAME":..,"ARCH":"ESP8266|ESP32","GPIO":[..],"FLAG":0,"BASE":n}`.
* `State` = same object as `StatusSTS` content (without wrapper) = tele STATE payload. `Power` -> `{"POWER":"ON|OFF"}`. `Hostname` -> `{"Hostname":".."}`. `Topic` -> `{"Topic":".."}`.
  `TelePeriod` -> `{"TelePeriod":300}`. `SetOption0` -> `{"SetOption0":"OFF"}`. `Webserver` -> `{"Webserver":"Active for Admin on <host> with IP address <ip>"}`.
  `Gpio` (no index) -> `{"GPIO":"Not supported"}`; `Gpios` -> `{"GPIOs1":{..},"GPIOs2":{..}}` (see gpios.json).
* Unknown: `{"Command":"Unknown","Input":"FOOBAR"}` (upper-cased). `FallbackTopic` is also Unknown on this build. Error: `{"Command":"Error","Input":"STATUS 9"}`.
  Empty `cmnd=`: `{"WARNING":"Enter command cmnd="}`. `Backlog` empty: `{"Backlog":"Empty"}`.
* `GET /` -> 200 text/html chunked, `<title><DeviceName> Main Menu`, footer `<a ...>Tasmota 16.0.0 (release-tasmota) by Theo Arends</a>` (ESP32: `(release-tasmota32)`).
  `GET /nonexistent` -> `404`, `text/plain`, body `File Not Found\n\nURI: /nonexistent\nMethod: GET\nArguments: 0\n`.
* Every `/cm` command is ALSO published to MQTT: `Status N` -> `stat/<t>/STATUS[N]`, other replies -> `stat/<t>/RESULT`, `Power` query -> `stat/<t>/POWER` (`ON`/`OFF` plain). (SetOption4=0 fleet-wide.)
  These (non-retained) stat messages in `mqtt/capture.txt` come from my requests and from other pollers on the broker.

## 4. MQTT facts (OBS, mqtt/capture.txt)

| topic | payload | retained | when |
|---|---|---|---|
| `tele/<t>/LWT` | `Online` / `Offline` | **retained** (will + online message use retain; `mosquitto_sub -F %r` shows 1 only for the copy delivered at subscribe time, live copies show 0) | on connect / unclean drop (OBS: one Offline->Online blip for livingroom-readinglamp at 18:19:08, MqttCount incremented) |
| `tele/<t>/STATE` | the `StatusSTS` body (see state.json); lights add Dimmer/Color/..; ESP32 adds `Berry` | no | every TelePeriod = 300 s (jittered per device by boot time; each device seen 1-2x in 6.7 min) |
| `tele/<t>/SENSOR` | `{"Time":..,"ENERGY":{TotalStartTime,Total,Yesterday,Today,[Period],Power,ApparentPower,ReactivePower,Factor,Voltage,Current}}` ; ESP32 `{"Time","DS18B20":{Id,Temperature},"ESP32":{Temperature},"TempUnit":"C"}` | no | every TelePeriod, right after STATE; devices without sensors send none |
| `tele/<t>/INFO1,2,3` | boot-time only | (not retained) | **not seen** (no device rebooted); format in docs/research/tasmota-minimal-and-ota.md section 1/SRC |
| `stat/<t>/RESULT` | reply JSON of any command (also Unknown) | no | per command |
| `stat/<t>/STATUS`, `STATUS1..11` | `Status N` replies | no | per `Status N` |
| `stat/<t>/UPGRADE` | `{"Upgrade":"Successful. Restarting"}` / `{"Upgrade":"Failed ..."}` | no | after OTA download (SRC; not observable read-only) |
| `tasmota/discovery/<MAC12>/config` | big JSON (ip, dn, fn, hn, mac, md, ty, if, cam, ofln, onln, state, sw, t, ft, tp, rl, swc, swn, btn, so{4,11,13,17,20,30,68,73,82,114,117}, lk, lt_st, bat, dslp, sho, sht, ver:1) | **retained** | at (re)connect |
| `tasmota/discovery/<MAC12>/sensors` | `{"sn":{"Time":..,[ENERGY..]},"ver":1}` | **retained** | at (re)connect |

Broker: mqtt.example.internal:1883, user `tasmota` (password redacted). MqttClient = MqttClientMask = topic = hostname.
`Status 6`: `MAX_PACKET_SIZE 1200, KEEPALIVE 30, SOCKET_TIMEOUT 4`.

## 5. Simulator behaviour spec

### 5.1 State machine
```
              Upgrade 1 / Upgrade <newer ver> (HTTP /cm, MQTT cmnd/<t>/Upgrade, /u1?o=..)
 NORMAL  ----------------------------------------------------------------------> PENDING (3 s: flag 3,2,1)
   ^   ^                                                                              | ota_state_flag hits 0
   |   |     download/flash FAILED (after up to 10 attempts on ESP8266, 5 ESP32)      v
   |   +--------------------------- (publish stat/<t>/UPGRADE "Failed <err>") ---- DOWNLOADING
   |                                                                                  | success (publish stat/<t>/UPGRADE "Successful. Restarting")
   |                                                                                  v
   |                                                                            RESTART_WAIT (~7 s: 2 s + restart_flag 5)
   |                                                                                  | EspRestart
   |                                                                                  v
   |                                                                              REBOOTING (HTTP/MQTT dead for ~3-12 s: boot + wifi join)
   |                                                                                  |
   |   new image is "tasmota"/"tasmota32" (full)                                       | new image = minimal (URL contains "minimal")
   +----------------------------------------------------------------------------------+------> MINIMAL
                                                                                      (stays until a further Upgrade 1 flashes full image)
 MINIMAL --Upgrade 1 (OtaUrl RAM value)--> PENDING --> DOWNLOADING --> RESTART_WAIT --> REBOOTING --> NORMAL
```
* Durations (nominal, as internal/simulator/ota.go plays them): PENDING 3 s; DOWNLOADING 10 s (fixed, not modelled by image size; a failed attempt retries after 1.5 s, 10 attempts on ESP8266, 5 on ESP32); post-download 2 s + 5 s; REBOOTING 8 s (ESP32 7 s; `DeviceSpec.RebootGap` overrides it).
  Observed on real devices: REBOOTING 5-12 s, end-to-end `Upgrade 1` -> HTTP answers again ~20-40 s. `Extra.TimeScale` (`tasmofleet-devenv -time-scale`, default 0.1) multiplies every nominal duration.
* Which image is flashed is decided purely by the OtaUrl at download time: file name contains `minimal` -> state MINIMAL (Version `16.0.0(release-minimal)`),
  `tasmota32` -> `...(release-tasmota32)`, else `...(release-tasmota)`. Version number: taken from the version marker of the served image (`Extra.FlashVersion` when it has none); a URL whose server
  returns 404/refused -> failure path (`Failed File Not Found (404)` etc, device keeps running, stays NORMAL, `ota_retry_counter` loops ~1-2 s apart; while retrying the HTTP server is unresponsive).
* Auto-fallback: if the full image is declared "too large" (config flag on ESP8266 1 MB devices) the device flashes minimal and sets `ota_loader=1`; after boot in MINIMAL it auto-starts
  (6 s) a download from the **stored** OtaUrl (the original one). With TasmoAdmin-style flow (minimal URL set first) `ota_loader` is 0 and the device sits in MINIMAL until told again.
* HTTP/MQTT during PENDING: fully responsive (`Upgrade` again just re-arms the flag). During DOWNLOADING/RESTART_WAIT: web server blocked (no reply, connect may succeed then hang),
  MQTT silent (broker may emit LWT Offline after ~45 s). REBOOTING: TCP connect refused/timeout, MQTT disconnected cleanly (NO LWT Offline). After reboot: MQTT reconnect -> retained LWT Online,
  (full fw) retained discovery config+sensors republished, INFO1/2/3, STATE after TelePeriod; (minimal) LWT Online + INFO only.
* Persistence rules (the OtaUrl rule): a `SettingsSave` happens in NORMAL at PENDING step 2 and via `Restart`/save timer; in MINIMAL **no setting is ever persisted** - `OtaUrl`, `SerialLog` etc. are
  RAM-only and are reverted on the next boot to the last value saved by the normal firmware. Simulator: keep `persistedSettings` and `runtimeSettings`; commit on NORMAL events only; reload on boot.
* After a reboot: `Uptime` 0T00:00:xx, `UptimeSec` small, `BootCount`+1 (NORMAL only - persisted), `RestartReason` = `Software/System restart`, `Heap` ~ 20-25 (ESP8266), `MqttCount` back to 1, `LinkCount` 1, `Downtime` `0T00:00:04`.

### 5.2 Per-state command table
NORMAL (ESP8266 full image): all commands from the capture dirs; replies are replayed from the recorded JSON with live fields (Time/Uptime/UptimeSec/Heap/RSSI/Signal/Power) recomputed.
State-changing commands should follow the usual Tasmota rules (`Power ON` -> `{"POWER":"ON"}` + `stat/<t>/POWER` `ON` + `tele/<t>/STATE` is NOT sent automatically unless SetOption26..; real devices send `stat/<t>/RESULT` + `stat/<t>/POWER`; `OtaUrl <u>` -> `{"OtaUrl":"<u>"}`, persisted).
MINIMAL: only `Upgrade`, `Upload`, `OtaUrl`, `SerialLog`, `Restart`; everything else (incl. `Status N`, `State`, `Power`, `Module`, `Backlog`, `Hostname`) -> `{"Command":"Unknown","Input":"<UPPERCASED INPUT>"}` with HTTP 200.
  `Upgrade` (no param) -> `{"Upgrade":"1 or >16.0.0 to upgrade"}` (this is the only way to read the version over `/cm`; the same string from the page footer in `GET /`).
  `GET /` -> page with `MINIMAL firmware<br>please upgrade`, buttons Firmware Upgrade / Console / Restart; footer `Tasmota 16.0.0 (release-minimal) by Theo Arends`; `Server: Tasmota/16.0.0 (<Hardware>)`.
  Routes: `/ /up /u1 /u2 /u3 /mn /cs /cm`; other paths 404 `File Not Found` (shape in 3.).
Parameter matrix for `Upgrade`: `""`/`0`/`2`/`01`/`ON`/equal-or-older version/garbage -> `1 or >16.0.0 to upgrade` ; `1` or newer version `a.b.c` (>=3 chars) -> `Version 16.0.0 from <OtaUrl>` + enter PENDING.
`OtaUrl` with value: reply `{"OtaUrl":"<value as given, case preserved>"}`; the response for `OtaUrl` is sent even in PENDING/MINIMAL.
`Restart 1` -> `{"Restart":"Restarting"}` then REBOOTING in ~2-3 s (same normal image); `Restart` / `Restart 0` -> `{"Restart":"1 to restart, 2 to halt"}`.

### 5.3 Edge cases to reproduce
* HTTP 200 for Unknown/Error; JSON key order and no spaces; numbers like `0.000`, `0.00`, `29.0` keep their decimals (Total `9.384`, Yesterday `0.000`) - emit raw text, not floats re-marshalled.
* `/cm?cmnd=` (empty) -> `{"WARNING":"Enter command cmnd="}`; missing cmnd param same.
* Case-insensitive commands, Input echo upper-cased; `Status0`, `Status 0`, `status%200` all work (index split). `Status 12`+ and >=13 -> Error shape (not recorded).
* `Status 9` Error on non-energy devices; `StatusPTH` only on energy ones; `Status 0` includes `StatusPTH` only for them.
* Multi-message commands (Status 0, Gpios) are merged into ONE JSON object by /cm (keys concatenated).
* Backlog: `Backlog a; b` returns merged results of all commands (not captured, SRC: delay between commands 200 ms default; `Backlog` empty -> `{"Backlog":"Empty"}`). Never used by TasmoAdmin for OTA.
* Wrong web credentials (if simulated): 401 `{"WARNING":"Need user=<username>&password=<password>"}`.
* ESP32: `Upgrade` same string; URL `tasmota32.bin`; reboot time ~5-10 s; Status 2 has no `Boot`; OtaUrl typically w/o `.gz`; RestartReason differs (`Software reset CPU`).
* MQTT command path: `cmnd/<t>/<Cmd>` payload = parameter; reply on `stat/<t>/RESULT`; empty payload = query. Group topic `tasmotas` (`cmnd/tasmotas/#`) and fallback topic also subscribed.
* TelePeriod 300 s publishes STATE then SENSOR (OBS). Timing of the first tele after boot is not captured (SRC support_tasmota.ino:1223: counter starts once wifi is up).
* Retained discovery of the old firmware stays on the broker while the device is in MINIMAL (minimal does not publish discovery).

## 6. Version string cheat sheet
`16.0.0(release-tasmota)` ESP8266 full | `16.0.0(release-minimal)` ESP8266 minimal (SRC) | `16.0.0(release-tasmota32)` ESP32 full | dev builds `16.0.0(<sha7>-tasmota)` .
Hardware: `ESP8266EX`, `ESP8285N08`, `ESP32-D0WDQ6 v1.0` (OBS; others `ESP32-S3 (revX)`, `ESP32-C3 ...`). `Server:` header shows `Tasmota/16.0.0 (<Hardware>)`.
discovery `sw`: bare `16.0.0`. Details in docs/research/tasmota-minimal-and-ota.md.

## 7. Sanitising
* MQTT password never appears in the captures (checked: password string absent from all files). `MqttUser` (`tasmota`) kept. No web passwords exist on the fleet.
* All MAC addresses, prefix and suffix (device `Mac`, AP `BSSId`, discovery `mac`/topic ids, compact 12-hex form) were rewritten to the fake prefix `02:00:5E:` / `02005E`, with rewritten, still distinguishable last three octets.
  `00:00:00:00:00:00` (ESP32 empty Ethernet MAC) kept. IPv6 link-local `fe80::...%st2` (derived from MAC) replaced with `fe80::ff:fe00:5e00%st2`.
* BSSIDs were rewritten the same way, and DS18B20 sensor ids to fake values.
* IPs were moved to 172.31.0.0/16, hostnames to `example.internal` (including the `tasmoadmin.example.internal` OtaUrl). SSID `IoT` is kept. Device names are generic room/appliance names kept from the capture.
* The capture scripts are not included, and the unsanitised capture is not part of the repository.
