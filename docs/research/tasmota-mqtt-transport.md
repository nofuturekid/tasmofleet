# Tasmota over MQTT: file transfer, Type 1, Backlog, LWT on restart

Source of every claim here: Tasmota **v16.0.0** (tag `v16.0.0`, arendst/Tasmota), read 2026-10-30, plus the live
test of the same day on `balcony-windowsill` (ESP8285 1 MB) and `office-monitor-thomas` (ESP32). Line numbers refer
to the files at that tag. The simulator (`internal/simulator/mqttfile.go`) follows this document; a difference
between the two is a simulator bug.

Paths are under `tasmota/`: `F` = `tasmota_xdrv_driver/xdrv_02_1_mqtt_file.ino`, `M` = `tasmota_xdrv_driver/xdrv_02_9_mqtt.ino`,
`C` = `tasmota_support/support_command.ino`, `T` = `tasmota_support/support_tasmota.ino`, `W` = `tasmota_support/support_wifi.ino`,
`E32` / `E8266` = `tasmota_support/support_esp32.ino` / `support_esp8266.ino`, `S` = `tasmota_support/support.ino`,
`PSC` = `lib/default/TasmotaPubSub/src/PubSubClient_311.cpp`.

## 1. Framing and state (shared by both commands)

- One shared state struct `FMqtt` for download and upload (`F` `struct FMQTT`, lines 36-48). Answers go to
  `stat/<topic>/FILEDOWNLOAD` and `stat/<topic>/FILEUPLOAD` (`MqttFilePublish`, `F` 68-81; `MqttPublishPrefixTopic_P(STAT, command)`),
  not to `RESULT`: the response buffer is cleared at the end, and a command publishes `RESULT` only if the buffer is
  non-empty (`C` `ExecuteCommand`, 518-525).
- `FileTransferHeaderSize = 21` (`{"Id":116,"Data":""}<null>`), `F` 50.
- Errors (`MqttFileValidate`, `F` 52-66): `{"FileUpload":"Error N"}`, or `"Aborted"` for 4. Codes: 1 invalid password, 2 no
  room / buffer, 3 invalid type, 4 aborted, 5 group topic, 6 no buffer.
- JSON keys are matched upper-case (`ID`, `TYPE`, `SIZE`, `MD5`, `DATA`, `PASSWORD`, `BINARY`; `F` 218-229, 460-465). The
  `File` name is **not** parsed. A payload of 8 characters or fewer is not parsed (`F` 214, 456).
- `file_id` is a `uint8_t` (`F` 46): an Id above 255 is truncated at the start, the next message then has `file_id != rcv_id` and
  aborts with Error 4 (`F` 162).
- Password: `strcmp(file_password, SettingsText(SET_MQTT_PWD))`; a missing `Password` key is Error 1 (`F` 93, 336). The Error
  reply is not special: nothing is leaked, but an `Unknown` reply (minimal build, section 3) echoes the whole command
  including the password in `Input` (`C` 503-516). **TasmoFleet must never log a payload of these commands, nor a RESULT
  `Input` of an Unknown reply to them.**
- Group topic (`cmnd/tasmotas/...`): Error 5 (`F` 88, 331).

## 2. FileDownload (Type 2, settings dump)

- Start `{"Password":"..","Type":2}`: validate sets `file_id = (UtcTime() & 0xFE) + 1` (`F` 340), builds the dump, publishes
  `{"FileDownload":"Started"}` (`F` 364-366), then the response `{"File":"Config_<name>_<ver>.dmp","Id":N,"Type":2,"Size":4096}` (`F` 352). The
  header is the answer to the start; the client's first `?` is the first chunk.
- **One step per command.** Each following command (any data) sends the next chunk `{"Id":N,"Data":"<base64>"}` (`F` 395-438); after the
  last chunk the next one sends `{"Id":N,"Md5":".."}` and `{"FileDownload":"Done"}` (`F` 440-453). A client must send one `?` per reply.
  A stray `?` makes the device run one step ahead; this is the mechanism behind the live anomaly (a truncated file and an MD5 mismatch).
  What the source does not explain is the garbage `_1` message seen once live; it is not simulated.
- Chunk size: `((ResponseSize() - 21) / 4) * 3 - 2` raw bytes (`F` 404). `ResponseSize()` is `MAX_LOGSZ` (`S` 1343-1345) =
  `LOG_BUFFER_SIZE - 96` (`tasmota.h` 232-242): 4000 on ESP8266 (chunk 2980, live: 2980 + 1116 = 4096) and 6000 on ESP32 (chunk 4480, so 4096 B in one message).
- `FileDownload 0` while a transfer runs: the chunk is published **first** (`F` 418-439, `payload != 0` is the only early return),
  then the abort path answers `{"FileDownload":"Aborted"}` (`F` 370-376). Clients must drain one chunk after an abort.
- Not simulated: `"Binary":1` (raw 4096 B chunks, `F` 398, 418-421).

## 3. FileUpload

- Start `{"Password":"..","File":"..","Id":116,"Type":1|2,"Size":N}` -> `{"FileUpload":"Started"}` (`F` 140-141), then `{"Id":116,"MaxSize":M}` (`F` 286).
- `M`: `chunk_size = MqttClient.getBufferSize() (1200) - 5 - topic_size - 1` (`F` 151) where `topic_size = strlen(topic of the command)` (`M` 925-927), then
  `((chunk_size - 21) / 4) * 3 - 2` (`F` 283). Topic `cmnd/balcony-windowsill/fileupload` (34 characters) gives 850, which is what the device announced live.
  The conversion runs again on **every** message while `file_pos == 0` (`F` 282-284), so empty-data messages shrink MaxSize each time.
- Data `{"Id":116,"Data":"<base64>"}`: each is acknowledged with `{"Id":116,"MaxSize":M}`. Finish `{"Id":116,"Md5":"<32 hex>"}`: MD5 mismatch -> `"MD5 mismatch"`, no restart (`F` 289-291); match -> `"Done"`, then
  Type 2 `SettingsConfigRestore` (Failed on a bad dump) or Type 1 `Update.end(true)` (Failed when the bootloader rejects the image) and `restart_flag = 2` (`F` 295-309).
- Buffer limit: PubSubClient closes the connection (stops the socket, **no DISCONNECT**) for an incoming packet with `1 + remaining-length bytes + remaining length >= 1200`
  (`PSC` 414-433, `bufferSize` 1200 and `maxIncomingPacketSize` 1200 set in `M` 270-271). With QoS 0 that is `5 + len(topic) + len(payload) >= 1200`
  for payloads above 125 bytes. The 850-byte chunk of a 34-character topic is 1156 payload bytes and 1195 on the wire: 4 bytes of slack. The broker then fires the will.
  (`MqttDataHandler` also ignores `data_len >= 1200`, `M` 914, which the readPacket check makes unreachable.)
- Group topic: Error 5 (`F` 88).
- Type 2 (settings): `SettingsBufferAlloc(size)` failing is Error 2 (`F` 98-101); it takes 2 to 16 KiB, so only more than 16 KiB is refused. A dump that `SettingsConfigRestore` rejects ends in `Failed`, an accepted one restarts (`F` 295-299). The simulator accepts only 4096-byte dumps at the end.
- Type 1 (OTA firmware), the order in `MqttFileUploadValidate` (`F` 97-130):
  1. password (Error 1);
  2. `head_room = (FlashWriteMaxSector() - FlashWriteStartSector()) * 4096`, `rounded = round_up_4k(Size)`; `rounded > head_room` is **Error 2** and happens
     before `Update.begin`, so it is predictable and nothing is sent or touched (`F` 102-108; live: ESP32 safeboot layout, Size 2234912, `Error 2`, no restart);
  3. a type other than 1 or 2 is Error 3, but only after step 2;
  4. `Update.begin(Size)`; `SettingsSave(1)` frees flash (`F` 116-122).
- `head_room` per layout:
  - **ESP8266** (`E8266` 213-219): start = `sketch_size / 4096 + 2` sectors, end = `(_FS_start - 0x40200000) / 4096 - 2`. Status 4 `Free` is the room from the sketch (rounded up to a sector)
    to the file system start, so `head_room = ceil4k(sketch) + Free - floor4k(sketch) - 16 KiB`. Example from the capture (ProgramSize 651 KB, Free 352 KB): 340 KB.
  - **ESP32, one app partition (safeboot layout)** (`E32` 253-255, 305-321, 527-537): `EspSingleOtaPartition()` is true, base = running partition address + sketch size (`E32` 306-309);
    start = `floor64k(base + 128 KiB)`, end = `floor64k(partition end)`. Status 4 `Free` is partition size minus sketch (`E32` 478-486). Roughly Free minus 128 KiB
    (the floor makes it up to 64 KiB larger). Example from the capture (2182 KB, Free 697 KB): 512 KiB; a 2.2 MB image can never fit.
    (The live note says "rounded UP"; the source divides and multiplies, i.e. rounds **down**.)
  - **ESP32, two app partitions**: base = the other partition's address (`E32` 310-314), end = its end: `floor64k(size) - 128 KiB`. Status 4 `Free` is `ESP.getFreeSketchSpace()`,
    the other partition's size (`E32` 486). The synthetic profile `esp32-dual` reports 2880 KB: head_room 2752 KiB.
- **FIRMWARE_MINIMAL excludes both commands**: `M` 63-65 and 90-92 sit inside `#ifndef FIRMWARE_MINIMAL` (48-70, 82-97), so the minimal ESP8266 image and the ESP32 safeboot image
  (built with `FIRMWARE_MINIMAL`, `tasmota_configurations_ESP32.h`, not re-read here; see the live note of 2026-10-30) answer `{"Command":"Unknown","Input":"FILEUPLOAD ..."}` (the input includes the password, see 1). The second stage of a staged
  ESP8266 update therefore cannot come over Type 1, and neither can a push into safeboot.
- `FileUpload 0` (or any non-matching Id): abort, `{"FileUpload":"Aborted"}` (`F` 162-182); for Type 1 `Update.end(true)` runs.
- Not simulated: binary uploads (`FileUpload<index above 299>`, `F` 211).

## 4. Backlog over MQTT

`CmndBacklog` (`C` 530-579) queues the commands and calls `ResponseClear()` (569): **the Backlog command itself publishes nothing** (`ExecuteCommand` only publishes a non-empty
response, `C` 518-524). The queue is processed by `ExecuteCommand(cmd, SRC_BACKLOG)` (`tasmota.ino` 770-786) after `backlog_timer` (SetOption34, default 200 ms between commands; `Backlog0`
no delay); every command publishes **its own** `stat/<topic>/RESULT` (or its own `STATUSn`/`POWERn`). `Backlog2` and `Backlog3` publish no results at all (`C` 538). An empty `Backlog`
answers `{"Backlog":"Empty"}` or `"Aborted"` (`C` 572-577). A client must wait for one result per queued command, matched by content; a merged reply does not exist. (HTTP `/cm` is
different: it returns the merged JSON, which the simulator still does.) The simulator publishes the results back to back, without the 200 ms.

## 5. LWT on restart

`restart_flag = 2` (set by `Restart 1`, FileUpload Done, topic changes, ...) is counted down once per second (`T` 1633-1641): at 2 `FUNC_ABOUT_TO_RESTART` and `SettingsSaveAll()`,
then `EspRestart()` (`W` 1741). `EspRestart` calls `WifiShutdown(true)` (`W` 1638), which calls `MqttDisconnect()` (`W` 1650) -> `MqttClient.disconnect()` (`M` 535-538) with no argument.
Tasmota's PubSubClient declares `disconnect(bool disconnect_package = false)` (`PSC.h` 186) and sends the MQTT DISCONNECT packet only when it is true (`PSC` 866-872), so the TCP
connection is closed without one. **The broker therefore publishes the will `tele/<topic>/LWT = Offline` (retained, QoS 1; `M` 1631-1638) on every software restart**, about 2 s after the
command (the restart_flag countdown) and just before the device goes dark.

Live 2026-10-30 agrees: Offline ~2 s after `FileUpload` Done (08:48:30 -> 08:48:32), Online ~5 s later, and Offline before each reboot of the staged OTA. Earlier notes in this
repository (`docs/research/tasmota-minimal-and-ota.md` section 6, spec 9) said "clean disconnect, no Offline"; they were an inference from MQTT 3.1.1 semantics, not from this library.
The simulator now drops the connection uncleanly in `reboot()` (every restart, OTA reboots included).

Consequences: health sees Online -> Offline -> Online for every restart; "went offline" alone is not a failure; an Offline during a transfer or an OTA is expected. `Device.Close` (test shutdown) still
disconnects cleanly so that closing a fleet does not fire wills.

## 6. What stays unverified

- Type 1 success on a real device (no ESP32 with a dual-OTA layout was tested); only Error 2 on the safeboot layout was seen live. The engine flow built on it (experimental, simulator-tested only) is in the design spec, section 9.4b.
- The empty `?` after a finished transfer: `MqttFilePublish` always publishes (`F` 79), so an empty message arrives on `stat/<topic>/FILEDOWNLOAD`; the simulator does the same (not seen live).
- The `_1` garbage message of the stray-`?` run (not explained by the source read).
- Whether `Update.begin()` can fail for reasons other than size; the simulator assumes it succeeds when head_room is enough.

## 7. WebGetConfig and an unreachable firmware server

`WebGetConfig <url>` downloads synchronously in the web command handler (`xdrv_01_9_webserver.ino` 4510-4588, status texts at 4264). When the device cannot reach the server it answers `{"WebGetConfig":"Connect failed"}` after the connect timeout and neither restarts nor changes settings. The simulator does this under `fw_unreachable` (the default wait is the 66 s of the OTA connect attempt, scaled).

## 8. Which RESULT answers which command (mqttcmd)

`mqttcmd` accepts a `stat/<topic>/RESULT` as the answer when a top-level key names the command (case-insensitive, same index; `POWER` counts as index 1, index 0 takes any). Read against `support_command.ino` at v16.0.0:

- `CmndModule` answers with the command name as key (`S_JSON_COMMAND_NVALUE_SVALUE`, `Module2` for index 2), `CmndModules` with `"Modules"`, `CmndGpio` with `GPIOn` or `GPIO` keys (`Gpio`, `Gpio 1`, `Gpio 10`, `Gpio 255`). All match by name.
- `CmndTemplate` answers `TemplateJson()`: `{"NAME":..,"GPIO":[..],"FLAG":..,"BASE":..}`, without the command name. `CmndGpios` publishes its lines through `ShowGpios` and then `ResponseClear()`s. These two are the table of `DoLoose` (console): the first RESULT counts. An automated read of them is not possible over MQTT and times out by design.
- A failed command answers `{"Command":"Unknown"|"Error","Input":"<COMMAND> <args>"}` on RESULT. `Input` is compared with the command (first word, index stripped) so another poller's failure is not taken as ours; it is never returned or logged (for `WebPassword`, `MqttPassword` and the file commands it holds the secret).
- `WebPassword` and `MqttPassword` echo the new password in clear in their RESULT: the reply of any command `cmdpolicy` marks `Redact` is scrubbed before it leaves the channel.
- A failure reply (`Command` Unknown or Error) without an `Input` is accepted as the answer of the command in flight (nothing says whose it is); with an `Input` it must name the command, so another poller's failure is not taken as ours.
- A Backlog that contains a command without a reply (`Delay`, for example) may return a partial merge: the channel ends it 600 ms after the last reply. Callers that need every effect read it back.
- Status 0 can still be reached indirectly (a rule, `Publish`, `Subscribe`): that is admin console input, classified by `cmdpolicy` as such, and is not refused by the channel.
- `CmndBacklog` strips leading `Backlog` words from every part (case-insensitive, no space needed; `support_command.ino` 550-557). `cmdpolicy` classifies a part after the same stripping; `mqttcmd` refuses any part that starts with `backlog`.
