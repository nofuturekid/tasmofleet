# Config restore and backup diff

Scope: restoring a stored `.dmp` onto a device through a one-time token on the
firmware listener, and the diff / drift views of backups. Code:
`internal/restore/`, `internal/backup/diff.go`, `internal/firmware/server.go`
(the `Extra` mount), `internal/web/{restore,backupdiff}.go`,
`internal/store/restore.go`, `internal/simulator/restore.go` (test stand-in).

Review findings are judged against this document. Every claim has a test.

## Model

Restore = automatic backup of the current config, a 128 bit token, the device
command `WebGetConfig http://<FIRMWARE_BASE_URL>/restore/<token>`, wait for the
reboot, fresh backup, diff. The device pulls the dump itself, so tasmofleet
never uploads through Tasmota's authenticated `/u2` route and the dump is never
on a URL that works twice or for anybody else.

## Restore over MQTT

When HTTP fails or the device cannot reach the firmware listener, the restore sends the dump with
`FileUpload` Type 2 (`mqttcmd.Upload`) instead of the token flow. There is no token and no listener
involved; R1 to R6, R16 and R17 concern the HTTP path only. The rest holds unchanged: R7 (one
confirmation), R8 (safety backup first, over the same path), R13 (identity from Status 5 right before
the upload), R14 (the address is not used by the MQTT path), R15, R9, R10 and R11.

| ID  | Invariant |
| --- | --------- |
| R23 | The path is chosen once, at the start, from `transport.Choose` and recorded on the job (`transport`); the whole job (safety backup, checks, upload, verification) is pinned to it. A restore never falls back to the other path, and a restore pinned to MQTT never sends `WebGetConfig`. |
| R24 | The dump goes to the topic of this device only, with the device's own MQTT password (sealed in the store, never in an error, log, audit record or page). `Error 1` marks the password bad; a bad mark makes `Choose` refuse the path until the password is saved again. |
| R25 | The device applies the dump only after the MD5 matched (v16 `xdrv_02_1_mqtt_file.ino`, Type 2). A wrong password, a refusal, a lost chunk or an abort therefore leave the settings unchanged and the device running; the job fails at the upload step. A lost final "Done" is an unknown outcome: the job checks the device and does not upload again. |
| R26 | The restart is proven by a larger `BootCount` in a fresh `Status 1`, within 3 minutes. An upload that returns while the boot count is unchanged fails at the restart step. |
| R27 | The identity check and the upload are bound to one target. The restore passes the identity it checked (topic, FullTopic, GroupTopic, MQTT capability, stored MAC set) into `UploadConfig`; the router re-reads the device and sends nothing if any of it changed, and asks `Status 5` over the same topic right before the `FileUpload` start: another MAC answering there is refused with "the device on this topic is not <name> any more" (`ErrIdentityChanged`), nothing uploaded. |
| R28 | A restore over MQTT cannot silently cut TasmoFleet off. Over MQTT the service compares the decoded backup with the device (Topic, FullTopic, GroupTopic, MqttHost, MqttPort, live MqttUser, and MqttPassword against the stored own or broker password); any difference, or a backup that cannot be decoded, needs the admin's own acceptance (`AllowMQTTLockout`, part of the one confirmation) with a sentence naming the fields, never the values. Over HTTP the device stays reachable and the check does not apply. |

## Invariants

| ID  | Invariant                                                                                                                                                                                                                                                                |
| --- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| R1  | The token is 128 random bits (`crypto/rand`), kept **only in memory** and only as its SHA-256. It is never stored in the database, never logged and never in an audit record or an error text.                                                                           |
| R2  | A token serves the dump **once**. It is consumed when the request matches, not when the response ends.                                                                                                                                                                   |
| R3  | A token serves only the **device IP** (the inventory address resolved at `Start`, IPv4-mapped IPv6 normalised) as seen in `RemoteAddr` of the firmware listener. There is no proxy and no forwarded header on that listener.                                              |
| R4  | A token expires after 5 minutes (`TokenTTL`), and it is revoked as soon as the job ends, whatever the outcome.                                                                                                                                                           |
| R5  | Wrong IP, second use, expired, unknown, malformed path, non-GET: all answer the same bare **404**. A request that does not match **does not consume** the token, so a stranger cannot burn the real download.                                                           |
| R6  | The response is the raw dump with `Cache-Control: no-store`, `Content-Type: application/octet-stream`, `nosniff`. The route shares the firmware listener's per-IP rate limit.                                                                                            |
| R7  | A restore starts only for a **config dump** backup, by an **admin** (behind the admin guard and the CSRF check, no password step-up since 0.8.0: over-restriction audit #1) after **one** explicit confirmation (checkbox, CSRF-bound form). The page runs the read-only live checks first, so that single confirmation names every risk: the device restarts, the configuration is replaced, old credentials come back, a firmware major change, an uncheckable chip family. A restore the checks refuse shows no form. |
| R13 | Before anything is changed the service reads the **live device**: Status 5 must report the MAC the inventory holds (a missing stored MAC is read from the live device, asked without credentials first and with them only after a 401, and stored in the empty slot only; a MAC that belongs to another entry refuses; the adoption is audited as `device.mac_adopted`), Status 2 must give a known firmware version, the backup must not come from newer firmware, and its chip family (from the decoded header) must match. A family mismatch is final; a different **major** version and an uncheckable family each need the admin's own tick. The identity is checked again right before the token is issued and again after the reboot. |
| R14 | The device address is resolved **once**; more than one distinct IP refuses. That one IP is used for the command target, the identity checks and the token binding. |
| R15 | One **per-device operation lock** (`internal/devlock`) is held from `Start` to the end of the job; a held lock or an active OTA job refuses the restore. Console/power and settings writes take the same lock and refuse while an OTA job is active; OTA Submit holds the locks of all its devices while it creates the jobs, after which the active job excludes the others (`internal/app/devicelocks.go`, `TestWritersExcludeEachOther`, `TestActiveOTAJobBlocksWriters`). |
| R16 | A command error after the device already did the work (timeout, unreadable reply, or a download that happened) is an **unknown outcome**: the job checks the device (reboot, fresh backup, diff) instead of failing. The command is never retried; the web log level is raised by the read-only status commands that run before it. |
| R17 | Refused `/restore/` requests are logged (rate-limited per source and reason, never with the path); a **valid token from the wrong address** is also audited with job and source address. |
| R8  | The safety backup comes first. If it fails the run stops with the device untouched. Its ID is recorded on the job so the restore can be undone.                                                                                                                          |
| R9  | At most one unfinished restore per device (unique index, not only a check). A restart fails orphaned jobs (their tokens died with the process).                                                                                                                          |
| R10 | Every step is audited (`restore.start`, `restore.download` with the actor `device`, `restore.finish`, plus the backups). Audit detail carries the job ID and counts, never values.                                                                                      |
| R12 | Limits are atomic: the check and the reservation of the drift throttle are one critical section; the single-use token and the one-active-restore rule are decided under a lock / by a unique index, so parallel requests admit exactly one (tested with concurrent callers under `-race`). |
| R11 | Verification compares the restored backup with a fresh one. Checksums, counters and the decoder header are ignored (`backup.IsVolatile`); anything else makes the job "restored, but N settings differ". Without a decoded twin only byte-identical dumps count as verified. |

## Accepted residual: trust on first use of the MAC

Only for a device with **no stored MAC** (never for one that has it): the
confirmation page reads the MAC the device at the address reports (asked without
credentials first) and shows "Device at <address> reports MAC <mac>, chip
<family>: this will be recorded as its identity". The admin's one confirmation
covers exactly that MAC (`expect_mac`); the start refuses when the host now
reports another one, and without it nothing is recorded. A MAC that belongs to
another entry refuses. The address must be inside `DEVICE_NETWORKS` (the client
refuses otherwise). Once stored, the MAC is what is re-checked right before the
token is issued and after the reboot, so nothing can be swapped in between
confirmation and send. The record is audited as `device.mac_adopted` ("identity
recorded on first restore"). Residual: if the wrong device sits at the address
and the admin confirms it, its MAC becomes the entry's identity; that is the
accepted trade-off of the over-restriction audit (#22).

## Replace device (0.8.0)

A wizard for a dead board: the entry's backup goes onto new hardware through the
same token mechanism, then the entry takes over the new hardware's MACs. Code:
`internal/restore/replace.go`, `internal/web/replace.go`,
`internal/store/replace.go`.

| ID  | Invariant |
| --- | --------- |
| R18 | The replacement hardware must be **online, inside the device networks (the client's SSRF guard), and bound to no inventory entry**: none of its MACs and not its address belongs to an entry. The entry itself is refused as a candidate (that is a plain restore). The candidate is probed **without credentials**; one that asks for a password is refused, so no stored password goes to a host that is not in the inventory. |
| R19 | **The chip family must match, and that is final**: the stored family of the old entry and the family in the header of the backup's decoded twin are both compared with the live family of the new device; a mismatch refuses, no tick helps. A backup from newer firmware refuses; a different firmware **major** version, or a family that cannot be checked at all (no decoded twin and no stored family), each need the admin's tick, which is part of the one confirmation. |
| R20 | **The old device must be silent.** Replace refuses while the stored address still answers as the old device (its MAC, or any answer when no MAC is stored, or an answer that cannot be classified, such as a 401). Something else on that address, including the new board itself, does not count. Two identical devices would clash (hostname, MQTT topic, static IP). |
| R21 | Order: 1) back up the **new** hardware (filed under the entry, marked `replacement hardware, before replace`; "newest" and an explicit choice never use such a dump as the old device's configuration), 2) re-read the new device's MAC and require the one recorded at the preview, then restore the old dump with the one-time token bound to the new device's IP, 3) wait for the reboot and find the board again by its **own** MAC (at the candidate address, or at the entry's old address if the restored configuration moved it), fresh backup and diff, 4) swap the entry in **one transaction** (`store.SwapHardware`): primary MAC, MAC set, address, port and identity change together or not at all. A MAC bound to another entry (UNIQUE), an address taken by another entry, or an entry whose MAC set changed in the meantime refuses and rolls back; the job then fails with the hardware restored and the entry untouched. Name, tags, notes, topic, credentials, history and backups stay. A verification that cannot complete does not stop the swap (the board answered with its own MAC). |
| R22 | Audit: `replace.start` (backup, new MAC, address) and `replace.finish` with the **old MACs**, the new MAC and the address; the token and credentials are never part of a record. One replace per old entry (the restore job table's unique index) and one per replacement MAC at a time. |

Known limit: if the restored configuration puts the board on an address that is
neither the one it had nor the entry's old address (a new static IP), it is not
found again; the job fails after the timeout and the entry is not changed. Add
the board's real address to the entry by hand (or repeat the replace with the
board at the entry's address).

## Files of the backup (0.26.0, #324)

Restore and replace write the files of a backup's files archive (spec 12.1) to the device's flash file
system before the settings. Code: `internal/restore/files.go`. The archive is secret like the `.dmp`.

| ID  | Invariant |
| --- | --------- |
| R29 | Files are written over HTTP only (`GET /ufsd` + `POST /ufsu` under one per-device lock hold, Basic auth as for `/dl`); never over MQTT, never to the SD card. A restore over MQTT, a device without a flash file system or with firmware older than 14.6.0 restores settings only, and the confirmation page says so before the confirmation. |
| R30 | The identity (Status 5) is checked right before the first file, after the safety backup: files of one device never go to another one. The archive is verified before use (`backup.ParseFilesArchive`: manifest first, every path through `tasmota.CheckFSPath`, size and SHA-256 per file); a damaged archive writes nothing and is said on the page. |
| R31 | Nothing on the device is deleted. Only files in the archive are written; a file is replaced only after its current content was read (at most 1 MiB) and kept, and a file whose content already equals the archive's is not written. A file that cannot be read first is not overwritten. **A differing file is overwritten only when the backup taken just before holds exactly its current content (SHA-256 in that backup's manifest)**; otherwise it is skipped, said loudly, and the job is not verified: a restore must be undoable. |
| R32 | Every upload is a flash write: files go one after the other, an upload is never retried in a loop, and the finished job states the number of flash writes (put-backs included). A failed upload is followed by exactly one put-back of the old file; when that fails the job says "FILE LOST" with the name. |
| R33 | A failure of the files step sends no settings: the job ends "files partly written, settings unchanged" with the step marked. A folder missing on the device skips its files with a stated reason and does not fail the job. |
| R34 | File contents are never in a log line, an error, a job detail, an audit record or a page; only names, sizes, counts and badges (`tlskey` is marked "private key"). |
| R35 | The confirmation is bound to the file plan it showed (`files` in the form, `StartOptions.ConfirmedFiles`): a page that showed no files, or an MQTT restore, never turns into one that writes files. After files were written, a later failure before the settings go over says so ("files written, settings unchanged") and offers the rollback; the put-back of an old file runs even when the restore was cancelled. |

## Diff and drift views

| ID  | Invariant                                                                                                                                                                                                                                                                           |
| --- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| D1  | A **secret** setting is never rendered, in either role. Secret = an explicit decode-config field list (exact path) **or** the fallback regex `pass\|pwd\|pwassword\|ssid.*pass\|token\|secret\|key\|cred\|auth\|fingerprint` on any key of the path, **or** the audit redaction keys. |
| D2  | The row of a secret setting says only that it changed (no old, no new value, no length). A key present on one side only is still a secret row.                                                                                                                                       |
| D3  | A string value in which **any** credential pattern matches (URL userinfo up to the last `@`, `user:pass@`, parameters with prefixed names such as `*_token`/`*_password`/`sig`, quoted values, headers incl. `X-API-Key`, Bearer/Basic, JSON members, Tasmota password/token commands, bot/webhook tokens in URL paths) is **never shown**: the row says only that a secret changed. Policy: when in doubt the whole value is a credential. |
| D4  | Rules, scripts, Berry code, **`mems`** (Mem1..16, the API keys rules use) and **`rf_code`** (replayable RF codes) can hold secrets no pattern knows. **Operators see only that they changed; admins see them** (unless a credential pattern matches, see D3).                                                                                                                                           |
| D5  | Device text is HTML-escaped by `html/template`; both backup IDs must be decoded backups of the device in the URL.                                                                                                                                                                  |
| D6  | The drift page reads Status 1..11 one by one (never Status 0), is throttled to once per 15 s per device, writes nothing, and ignores clocks, uptime, memory and sensor readings.                                                                                                   |
| D7  | Backup **downloads** stay admin-only and audited (unchanged).                                                                                                                                                                                                                       |

## Known limits

- A credential in a shape no scrubber knows, outside rules and scripts, would
  be shown (for example a bare token in a friendly name). The explicit field
  list and the key regex cover the settings decode-config v16.0.0 defines.
- If the device reaches tasmofleet through NAT, its source IP differs from the
  inventory address and the download is refused (404). The firmware listener is
  meant to be reached directly (br0 ipvlan).
- `WebGetConfig` must be compiled into the device firmware. A device without it
  fails the job with "never downloaded the configuration".

## Residual risk: plain HTTP on the IoT network

The dump travels unencrypted from the firmware listener to the device. Tasmota
offers no encrypted config import (the `/u2` upload is plain HTTP as well), and
its HTTPS client does not validate certificates, so there is no better
transport without custom firmware.

- The dump holds the Wi-Fi PSK, the MQTT and web passwords and, depending on
  the setup, Zigbee, KeeLoq and PN532 keys and API tokens. On switched Ethernet
  nobody can read it without ARP or port spoofing. On Wi-Fi with WPA2-PSK, an
  attacker who knows the IoT PSK (every IoT device does) and captures the
  handshake can decrypt it.
- The token limits who can fetch the dump, not who can listen.
- What this adds over normal operation: the web password and the MQTT password
  already travel in clear text (Basic auth, MQTT without TLS). The additional
  exposure is the keys and tokens that are not normally on the wire.

Recommendations:

1. Enable **protected management frames (802.11w / PMF)** on the IoT SSID, so a
   deauthentication cannot force a new handshake.
2. Reach the firmware listener over a wired or management path where possible.
3. **Rotate the secrets the dump holds** (MQTT and web passwords, tokens) after
   restoring on a network you do not trust, and after restoring an old backup:
   its credentials come back, even if they were rotated since (the confirmation
   page says so).
