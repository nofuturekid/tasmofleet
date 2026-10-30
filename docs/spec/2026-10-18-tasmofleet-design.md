# tasmofleet design

Status: accepted 2026-10-18, implemented; every statement about current behaviour was checked against the code of 0.19.2 on 2026-10-30. Owner: the maintainer. Author: architect agent.
The code in `internal/` implements the contracts in this document. If code and
spec disagree, fix one of them in the same change.

Inputs: device captures in `testdata/fixtures/` (Tasmota 16.0.0, 6 devices, 6.7 min of
MQTT), `docs/research/tasmota-minimal-and-ota.md` (minimal firmware and OTA flow, derived
from the v16.0.0 source), and `docs/research/tasmoadmin-*.md` (feature inventory, data
formats, lessons learned). Labels used below: **OBS** = observed on real devices, **SRC** =
read from Tasmota source, **DEC** = design decision.

---

## 1. Purpose and scope

tasmofleet manages a fleet of Tasmota devices and replaces TasmoAdmin with feature
parity. Home Assistant keeps automation. tasmofleet does inventory, live state, health,
firmware updates, configuration backups, device settings and fleet-wide commands.

### 1.1 Goals

1. **Safe firmware updates.** They run on the server as a persisted state machine and
   survive restarts. A hard guard refuses any image for the wrong chip family, and 1 MB
   ESP8266 devices are staged through minimal firmware based on free flash, not on the
   model name.
2. **HTTP is enough.** Every feature works without MQTT. MQTT only makes health and live
   state faster.
3. **Usable backups.** The raw `.dmp` is the authority. Decoded JSON is added for diffs
   and drift checks.
4. **First-class SSO.** Native OIDC (code + PKCE, discovery, groups, RP-initiated logout)
   or trusted forward-auth headers, both correct behind reverse proxies, and a full
   standalone mode (local accounts, own TLS) for hosts without proxy or IdP.
5. **Simple to operate.** One static binary, SQLite, one OCI image, config through env
   and file, structured logs, Prometheus metrics. Ready for a Home Assistant add-on.
6. **AI-maintainable.** Small single-purpose packages with fixed interfaces, enforced
   boundaries, a device simulator, and gates that catch regressions (mutation tests on the
   safety-critical code).

### 1.1a Principles (product decisions, 2026-10-19)

1. **Minimal mandatory configuration.** A fresh install starts with almost no
   settings. Defaults: self-signed TLS, local auth with a first-run admin and a first-run
   wizard. The environment holds only bootstrap values (data dir, listeners, and the
   things needed before the database is open). Everything else, SSO/OIDC included, is
   adjusted live in the UI and stored in the database. An environment value still
   locks its field. Legacy environment values are imported into the database once.
2. **Proportionate security for a homelab tool.** Keep the solid basics: CSRF protection,
   authorisation on every route, no secrets in logs or HTML, the chip-family guard, and
   secrets sent only to the address they are bound to. Step-up (re-authentication) only
   for local account management, auth/SSO changes and restore; routine settings don't
   need it. Security reviews: High and Medium findings are fixed; Low and Info findings
   become documented residual risks without further review rounds.
3. **The reference deployment is not a design constraint.** Network specifics of one
   installation (e.g. firewall rules between IoT VLANs and the Home Assistant host) are
   handled in its deployment docs. The Home Assistant add-on (M3) targets other users.

### 1.2 Non-goals

- Automations, schedules, timers, scenes (Home Assistant).
- A switching dashboard beyond a per-device power toggle in the device list (M2).
- Self-update of tasmofleet, passkeys/WebAuthn, open self-registration, an "auth off" mode.
- Self-service registration or password reset by e-mail.
- Building custom Tasmota firmware. Uploading custom builds is in scope for M2 (§8.6).
- Serial flashing / first install of Tasmota.
- Multi-tenant or multi-instance clustering.

### 1.3 Milestones (summary, details in §23)

| M         | Content                                                                                                                                                                                                                                                           |
| --------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| M1 core   | inventory (manual + TasmoAdmin import), health (HTTP poll + optional MQTT), firmware cache + firmware server, OTA engine, backups (raw + decoded), auth (OIDC, header, local accounts, standalone TLS), audit, metrics, image, Unraid template, Grafana dashboard |
| M2 parity | device list with live state + power toggle, device settings editing, console/commands, bulk commands, autoscan (HTTP) + MQTT discovery, policy/drift view, backup diff + restore, custom firmware upload (notifications were planned, not built: §14.5)                                           |
| M3        | Home Assistant add-on (ingress), move to public GitHub                                                                                                                                                                                                            |

---

## 2. Example deployment

This is the kind of installation tasmofleet is built for. The code stays generic. Concrete
values appear only in `deploy/` examples and in this section (all names and addresses are
placeholders).

| Aspect        | Example                                                                                                                                                                                                                                                                                                                                                 |
| ------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Host          | A Docker host with a macvlan/ipvlan network and fixed container IPs in 172.31.12.0/24. An existing TasmoAdmin runs at 172.31.12.60; tasmofleet gets its own fixed IP (172.31.12.61) and can take over .60 when TasmoAdmin is retired (§22)                                                                                                              |
| Container     | Unraid template `deploy/unraid/tasmofleet.xml` (any Docker host works), data directory mounted at `/data`                                                                                                                                                                                                                                               |
| Reverse proxy | Traefik v3, single entrypoint `websecure` on 172.31.12.20. Public name `tasmofleet.example.com`, internal split DNS `tasmofleet.example.internal`. `TRUSTED_PROXIES=172.31.12.20/32`                                                                                                                                                                    |
| SSO           | Authentik at `auth.example.com`, one OAuth2/OIDC provider per app. Issuer `https://auth.example.com/application/o/tasmofleet/`. Groups `Tasmofleet Users` (operator), `Tasmofleet Admins` (admin). Providers created through the API need `grant_types` authorization_code + refresh_token. Redirect URI `https://tasmofleet.example.com/oidc/callback` |
| Devices       | IoT VLANs 172.31.13.0/24 and 172.31.23.0/24, a ~20 device fleet: Sonoff Basic / MagicHome (ESP8285N08), Gosund SP111 / NOUS A1T (ESP8266EX), all ESP8266 with 1 MB flash, plus ESP32-D0WDQ6 lamps (tasmota32). All on 16.0.0, image `release-tasmota` / `release-tasmota32`. Device names `<room>-<thing>` = MQTT topic = hostname. No web passwords    |
| Firewall      | IoT may reach only MQTT (172.31.12.11:1883) and the OTA server, TCP 80. If the rule is IP-based, the firmware server must listen on **plain HTTP port 80 on the container IP** (`FIRMWARE_LISTEN=:80`, `FIRMWARE_BASE_URL=http://172.31.12.61`)                                                                                                         |
| MQTT          | `mqtt.example.internal:1883` (Mosquitto), user `tasmota`. Tasmota discovery retained on `tasmota/discovery/+/config`. TelePeriod 300, FullTopic default `%prefix%/%topic%/`                                                                                                                                                                             |
| Observability | Prometheus scrapes `/metrics`; blackbox probes `/healthz`. A log collector (e.g. Loki via Alloy) reads Docker JSON logs, so tasmofleet logs JSON to stdout. tasmofleet ships `deploy/grafana/tasmofleet.json`                                                                                                                                           |
| Notifications | optional, later: mail, or a Home Assistant webhook (§14.5)                                                                                                                                                                                                                                                                                              |
| Fleet policy  | PowerOnState 0, SetOption0 0 (`save_state`), SaveData 0, Timezone 99 with DST rules, NTP from DHCP. Exception: ESP32 lamps use SetOption0 1. M2 policy view flags drift (§14.4)                                                                                                                                                                         |
| CI            | GitHub Actions on GitHub-hosted runners. Images go to `ghcr.io/nofuturekid/tasmofleet` and `docker.io/nofuturekid/tasmofleet` (same tags)                                                                                                                                                                                                                                                                  |

Example environment for this deployment: `deploy/unraid/tasmofleet.env.example`.

---

## 3. Architecture

### 3.1 Process view

```
                   Traefik (websecure, TLS)            Authentik (OIDC / forward auth)
                          |                                    |
                          v                                    |
   +------------------------------- tasmofleet ---------------------------------+
   |  UI listener :8080  (auth, CSRF, HTMX pages, SSE, /metrics, /healthz)      |
   |      |                                                                     |
   |   web --> inventory, health, ota.Engine, backup, firmware.Cache, audit     |
   |              |            |          |           |          |              |
   |            store (SQLite /data/tasmofleet.db, WAL)        files /data/...  |
   |                           |          |           |                         |
   |                 mqttbus (optional)   tasmota.Client (HTTP, SSRF-guarded)   |
   |                                                                            |
   |  Firmware listener :80 (plain HTTP, GET/HEAD /fw/<release>/<file> only)   |
   +--------------|--------------------------------|----------------------------+
                  | MQTT 1883 (optional)           | HTTP 80 to devices (/cm, /dl, /)
                  v                                v
            Mosquitto  <------- tele/stat -----  Tasmota devices (IoT VLANs)
                                                   | HTTP GET firmware (port 80)
                                                   +-----> firmware listener
```

There are two listeners on purpose. Devices reach only the firmware listener, which has
no authentication, no UI and no state-changing routes. People reach only the UI listener,
through the reverse proxy.

### 3.2 Package map and contracts

Every package has one purpose and can be tested in isolation. The skeleton contains each
interface listed here. Imports point downwards: `model` is the leaf and `app` wires
everything.

| Package                                 | Purpose                                       | Key exported API (contract)                                                                                                                                                                                                        | Depends on                             |
| --------------------------------------- | --------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------- |
| `model`                                 | domain types                                  | `Device`, `Identity`, `ChipFamily`, `MQTTCapability`, `Health`, `Artifact`, `Job`, `JobState`, `Stage`, `Plan`, `UpdatePolicy`, `Backup`, `AuditEvent`, `Principal`, `Role`                                                        | stdlib                                 |
| `config`                                | configuration                                 | `Config`, `Load(environ)`                                                                                                                                                                                                          | stdlib                                 |
| `clock`                                 | testable time                                 | `Clock{Now, After}`, `Real`                                                                                                                                                                                                        | stdlib                                 |
| `secretbox`                             | AES-GCM at rest                               | `Box{Seal, Open}`, `LoadOrCreate(dataDir)`                                                                                                                                                                                         | stdlib                                 |
| `store`                                 | persistence                                   | `DeviceStore`, `HealthStore`, `JobStore`, `ArtifactStore`, `BackupStore`, `AuditStore`, `SessionStore`, `LocalUserStore`, `Store`, `Open(path, box)`, `Migrations`                                                                 | model, secretbox, modernc.org/sqlite   |
| `tasmota`                               | device HTTP protocol                          | `Client{Command, DownloadConfig, RootPage}`, `Target`, `TargetFor(Device)`, `ParseStatus2/4/11`, `ParseUpgradeProbe`, `ParseOtaURL`, errors `ErrUnknownCommand`, `ErrAuth`, `ErrUnreachable`, `ErrTimeout`, `ErrForbiddenAddr`     | model                                  |
| `fwselect`                              | **pure** firmware decisions                   | `Detect`, `FamilyFromHardware`, `Select`, `Decide`, `InspectImage`, `Guard`, `Plan`, `BuildOptions`, `DetectedVariant`, errors `ErrUnknownHardware`, `ErrBuildChoiceNeeded`, `ErrFamilyMismatch`, `ErrFilenameMismatch`, `ErrSafeboot`, `ErrVariantChange`, `ErrNoSuchVariant`, `ErrBadImage` | model                                  |
| `firmware`                              | upstream sources, cache, device-facing server | `Cache{Releases, Ensure, URLFor, Prune}`, `Source`, `Server(Cache)`                                                                                                                                                                | model, fwselect, store                 |
| `ota`                                   | update state machine + engine                 | `Step(job, event, now, cfg)` (pure), `Engine{Run, Submit, Preview, Cancel}`, `DeviceAPI`, `Firmware`, `Backups`, `Notifier`                                                                                                        | model, fwselect, store, clock          |
| `mqttbus`                               | optional MQTT                                 | `Bus{Subscribe, Command, Connected}`, `Connect`, `Message`, `Kind`                                                                                                                                                                 | paho                                   |
| `mqttcmd`                               | commands over MQTT (SAFETY)                   | `Channel{Do, DoLoose, Info, Connected}`, `Doer`, `ErrRefused`                                                                                                                                                                      | cmdpolicy, mqttbus, tasmota, model     |
| `transport`                             | path choice and routing (SAFETY)              | `Choose(Facts) Paths` (pure), `Router` (a `tasmota.Client`), `WithPath`, `WithTrace`, `Memory`                                                                                                                                     | cmdpolicy, mqttcmd, tasmota, model     |
| `health`                                | reachability                                  | `Tracker{Get, All, Run}`, `Options`                                                                                                                                                                                                | model, mqttbus, tasmota, store, events |
| `events`                                | in-process pub/sub                            | `Hub{Publish, Subscribe}`, `Event`, `Topic`                                                                                                                                                                                        | stdlib                                 |
| `inventory`                             | device service                                | `Service{List, Get, Create, Update, Delete, Upsert}`                                                                                                                                                                               | model, store, audit, events            |
| `tasmoadmin`                            | import parsers                                | `ParseDevices(r, key)`, `ParseConfig(r)`                                                                                                                                                                                           | model                                  |
| `backup`                                | backups                                       | `Service{Backup, List, Open}`, `Decoder{Version, Decode}`                                                                                                                                                                          | model, store, tasmota                  |
| `audit`                                 | audit trail                                   | `Recorder{Record}`, `RedactKeys`                                                                                                                                                                                                   | model, store                           |
| `auth`                                  | authentication                                | `Authenticator{Principal, Routes}`, `RoleMapping`, `HashPassword`, `VerifyPassword`, `FromContext`, `WithPrincipal`                                                                                                                | model, store, httpx, go-oidc, oauth2   |
| `httpx`                                 | HTTP plumbing                                 | `Proxy{Trusted, Middleware}`, `BasePath(ctx)`                                                                                                                                                                                      | stdlib                                 |
| `web`                                   | UI                                            | `Handler(Deps)`, `OTA`, `Assets`                                                                                                                                                                                                   | interfaces of the above                |
| `metrics`                               | Prometheus text format                        | `Registry`, `Handler`                                                                                                                                                                                                              | stdlib                                 |
| `app`                                   | wiring, lifecycle                             | `Run(ctx, cfg)`                                                                                                                                                                                                                    | everything                             |
| `simulator`                             | Tasmota simulator (test only)                 | `NewFleet`, `Fleet`, `Device`, `DeviceSpec`, `Profile`, `Fault`                                                                                                                                                                    | model, mochi-mqtt (embedded broker)    |
| `testutil/oidcmock`                     | mock IdP (test only)                          | `Provider{Issuer, Handler, NextLogin}`                                                                                                                                                                                             | stdlib                                 |
| `discovery`, `policy` | MQTT-first and scan discovery; policy rules and outcomes | see package docs | |
| `cmdpolicy` | **pure** classification of free-form commands | `Authorize`, `Classify`, `Class`, `PolicyReadable`, `PolicySetText` (console, bulk "Send command", policy rules) | model |
| `devicecmd`, `devlock` | single device commands; one operation at a time per device | restore, OTA and backup take the per-device lock | see package docs |
| `restore`, `settings`, `sysconfig` | restore and replace device; curated device settings; system settings and policy rule versions in the database | see package docs | |
| `changelog`, `netaddr`, `web/ui` | What's new page parser; address formatting; templ components | see package docs | |
| `bulk`, `notify` | **not wired** | `bulk.DefaultAllowlist` and the `notify.Notifier` interface exist; nothing in `app` uses them (§14.1, §14.5) | |

Boundaries are enforced in `.golangci.yml` (depguard) and listed in `CLAUDE.md`.

### 3.3 Dependencies

Each module needs a reason. Anything not listed needs a spec change first.

| Module                                                                                   | Why                                                                                                                                                                                                                                                                                                                                                                                                                                                                          | Scope         |
| ---------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------- |
| `modernc.org/sqlite`                                                                     | SQLite without CGO, so the binary is static and the image distroless                                                                                                                                                                                                                                                                                                                                                                                                         | prod          |
| `github.com/eclipse/paho.mqtt.golang`                                                    | mature MQTT 3.1.1 client (Tasmota speaks 3.1.1) with auto-reconnect; writing MQTT by hand is not worth the risk                                                                                                                                                                                                                                                                                                                                                              | prod          |
| `github.com/coreos/go-oidc/v3`                                                           | ID token verification, JWKS rotation, discovery; security-critical, do not hand-roll                                                                                                                                                                                                                                                                                                                                                                                         | prod          |
| `golang.org/x/oauth2`                                                                    | authorization code exchange with PKCE (`oauth2.S256ChallengeOption`)                                                                                                                                                                                                                                                                                                                                                                                                         | prod          |
| `github.com/mochi-mqtt/server/v2`                                                        | embedded MQTT broker for tests and `tasmofleet-devenv` only (depguard keeps it out of prod packages)                                                                                                                                                                                                                                                                                                                                                                         | test          |
| `github.com/a-h/templ`                                                                   | typed HTML components for the web UI (design v2 port, plan section 4): template errors become compile errors, props are typed. The runtime is a small package; the generator is pinned in `tools/go.mod` and its output (`*_templ.go`) is committed, so the build and the Dockerfile need no generator (`make templ`, `make templ-check`). templ imports only in `internal/web` and `internal/web/ui` (depguard); `templ.Raw`, script and css components are forbidden (CSP) | prod          |
| htmx 2.0.11 + htmx-ext-sse 2.2.4                                                         | vendored JS files, no npm in the build                                                                                                                                                                                                                                                                                                                                                                                                                                       | prod (static) |
| `@playwright/test` 1.64.1                                                                | e2e only, in `e2e/`                                                                                                                                                                                                                                                                                                                                                                                                                                                          | test          |
| `pixelmatch` 5.3.0, `pngjs` 7.0.0 (+ `@types`)                                           | pixel comparison against the design prototype (`make visual`); pixelmatch pulls pngjs 6 for its CLI                                                                                                                                                                                                                                                                                                                                                                          | test          |
| tools module (`tools/go.mod`): staticcheck, golangci-lint v2.15.0, govulncheck, gremlins | gates, pinned, isolated from the main module graph                                                                                                                                                                                                                                                                                                                                                                                                                           | dev           |

Deliberately not used: `prometheus/client_golang` (counters and gauges are about 150 lines; it
would add ~15 modules), web frameworks/routers (Go 1.22+ `ServeMux` patterns are enough),
ORMs and migration libraries (about 60 lines of migrator), config libraries, CLI frameworks.
Stdlib features used: `net/http.CrossOriginProtection` (CSRF), `crypto/pbkdf2` (password
hashing), `log/slog`, `embed`, `html/template` (its escaping helpers and `template.HTML` for markup a component already escaped; the pages are templ).

---

## 4. Data model and migrations

SQLite file `<DATA_DIR>/tasmofleet.db`. Opened with `journal_mode=WAL`,
`foreign_keys=ON`, `busy_timeout=5000`, `synchronous=NORMAL`, and **one** open connection
(`SetMaxOpenConns(1)`). The fleet has tens of devices, so a single writer removes
`SQLITE_BUSY` handling entirely.

Migrations are plain SQL files in `internal/store/migrations/NNNN_name.sql`, embedded
and applied in order inside one transaction each. `PRAGMA user_version` stores the last
applied number. A merged migration file is never edited. Downgrades are not supported:
the store refuses to open a database whose `user_version` is newer than the binary
knows, and says so. Before applying migrations, the store writes a copy of the database
to `<DATA_DIR>/tasmofleet.db.pre-<N>` (only when migrations are pending; the newest 3
copies are kept).

Tables (35 migrations, `0001` to `0035`; the files in `internal/store/migrations/` are authoritative, this list names what each table is for):

| Table                                       | Content                                               | Notes                                                                                                                                          |
| ------------------------------------------- | ----------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `devices`                                   | inventory + detected identity + coarse health         | unique MAC (if known), unique topic (if set), unique (address, port). `http_password` sealed with secretbox. `ota_excluded`, `mqtt_capability` |
| `health_events` | health state transitions | written on change only, pruned after 90 days (fixed) |
| `firmware_artifacts`                        | cache index                                           | unique (release, filename), sha256, family proven by image header, `safeboot` flag                                                             |
| `ota_batches`, `ota_jobs`, `ota_job_events` | OTA                                                   | partial unique index: at most one non-terminal job per device. Every transition writes a row to `ota_job_events`                               |
| `backups`                                   | backup index                                          | files under `<DATA_DIR>/backups/<dir>/` (`<dir>` is the sanitised hostname, see below; every row stores its own relative path); `kept` pins a run, `trigger` says what started it                                                          |
| `audit_log` | append-only audit | pruned by `AUDIT_RETENTION` (default 1 year); the same age also prunes old jobs and failed backup attempts |
| `sessions` | web sessions | the store holds `sha256(cookie)`, never the cookie |
| `device_cookies` | remembered browsers for the login rate limit | hashed id, bound to the password generation (§15.1) |
| `local_users`                               | local accounts (role, disabled, must_change_password) | written by CLI and admin UI with step-up                                                                                                       |
| `settings` | small key/value (e.g. last firmware index refresh) | not for configuration |
| `system_settings`, `policy_rule_versions` | operational settings (§16) and every saved version of the policy rules | sealed secrets, append-only versions |
| `principals`, `user_prefs` | SSO and proxy users seen at sign-in; per-user UI preferences | roles of principals are a record of the last sign-in |
| `discovery_proposals`, `device_macs` | discovered devices waiting for an admin (expire after 30 days); the MAC set of a device | |
| `restore_jobs`, `firmware_custom`, `backup_attempts` | restores; provenance of uploaded firmware; failed backups | |

Timestamps are INTEGER unix milliseconds, UTC. JSON columns (`tags`, `identity_before`,
`principal`, `detail`) are plain TEXT, encoded and decoded in `store`.

File layout under `/data`:

```
/data/tasmofleet.db (+ -wal, -shm)
/data/secret.key                 32 random bytes, 0600 (device passwords at rest)
/data/firmware/<release>/<file>  cache, 0644 (served to devices; contains no secrets)
/data/backups/<dir>/<UTC stamp>_<hostname>.dmp               0600
/data/backups/<dir>/<UTC stamp>_<hostname>.snapshot.json
/data/backups/<dir>/<UTC stamp>_<hostname>.decoded.json
/data/tmp/                       decoder scratch (PyInstaller extraction), cleaned on start
/data/tasmofleet.env             optional config file
/data/csrf.key                   32 random bytes, 0600 (CSRF, device cookies, session signing)
/data/tls/                       generated self-signed certificate (§15.3a)
/data/initial-admin-password     only until the first admin has changed it (§15.1)
```

Backup naming (0.18): `<hostname>` is the device's current hostname (Status 5) reduced to
`[A-Za-z0-9._-]` (other characters become `_`, leading dots dropped, at most 63 bytes), else the
sanitised inventory name, else `device-<id>`. A directory that already holds another device's
or a deleted device's backups (case-insensitive, decided from the `backups` index under one
manager-wide lock from the choice to the first indexed file) is not shared: `<hostname>_<id>` is
used. A hostname change only affects new backups; old files are never moved, the index finds them
by path. Files of 0.17 (`backups/<device-id>/<stamp>-<kind>.<ext>`) are moved at every start by an
idempotent migration: SHA-256 of the file must equal the row, `os.Rename` on the same file system,
then a compare-and-set update of the row path. A crash between rename and update is repaired at the
next start; anything unclear (checksum mismatch, both or neither file, target used by another row)
is left untouched and logged; nothing is deleted except empty numeric directories (`os.Remove`).
Backups of deleted devices stay where they are. No SQL migration; a downgrade to 0.17 still reads
the moved files because it reads by the stored path.

All files are created with explicit modes. The process never runs `chmod -R` over the
data directory (TasmoAdmin lesson A4).

---

## 5. Device protocol facts (package `tasmota`)

- `GET http://<addr>:<port>/cm?cmnd=<urlencoded>` always answers 200 with
  `application/json`, chunked, `Connection: close`, one JSON object (OBS). Unknown
  command: `{"Command":"Unknown","Input":"<UPPER>"}`. Known command without output:
  `{"Command":"Error",...}`. Empty: `{"WARNING":"Enter command cmnd="}`. Wrong
  credentials: 401 `{"WARNING":"Need user=<username>&password=<password>"}` (SRC).
- Credentials are sent as `Authorization: Basic`, never as `user=`/`password=` query
  parameters (those end up in logs and error messages, TasmoAdmin lesson B2). Every
  error and log line passes through a redactor.
- The client allows **one request at a time per device** (ESP8266 web servers are
  single-threaded). Requests to the same device queue, requests to different devices run
  in parallel up to a global limit (default 8).
- Timeout classes: `Status N` 10 s, `OtaUrl`/`Upgrade` 15 s, `/dl` 20 s, `GET /` 10 s.
  Responses over 1 MiB are truncated and rejected.
- SSRF: `Target` can only be built from an inventory `Device`. The dialer resolves the
  host and refuses any IP outside `DEVICE_NETWORKS`. The check runs on the resolved
  address at connect time (`net.Dialer.Control`), so DNS rebinding does not bypass it.
  Redirects are not followed.
- **`Status 0` is never sent by automated code.** It returns 12 sections at once; on ESP8266
  with <20 kB heap this produced spliced, undecodable payloads in this fleet. Identity
  probing uses `Status 1`, `Status 2`, `Status 4`, `Status 5`, `Status 6`, `Status 11` and plain `Status` (the
  Status object only: DeviceName and the resolved Topic), one at a time, ≥200 ms apart.
- Minimal firmware (SRC): only `Upgrade`, `Upload`, `OtaUrl`, `SerialLog`, `Restart`
  exist; everything else is Unknown. The running version comes from bare `Upgrade`
  (`{"Upgrade":"1 or >16.0.0 to upgrade"}`). The image name and the language build come from
  `GET /`: footer `Tasmota 16.0.0 (release-minimal) by Theo Arends`, `<html lang="..">`,
  and the heading `MINIMAL firmware<br>please upgrade` (`RootPage`). On ESP32 the
  safeboot partition shows a "Safeboot" page with the same command table.
- **Minimal detection rule**: a device runs minimal (or ESP32 safeboot) only if
  `Status 2` answers `{"Command":"Unknown"}` **and** `GET /` shows the minimal/safeboot
  heading or the image tag `release-minimal`/`safeboot`. The bare `Upgrade` reply
  (`1 or >X to upgrade`) is identical on normal firmware (OBS, all fixtures). It only
  supplies the version (`tasmota.UpgradeProbe.Idle`, `fwselect.UpgradeProbe.Version`)
  and never proves minimal on its own.
- **Identity source (#87).** The HTTP probe is the primary source. For a device on our broker
  (`mqtt_capability = ours`) whose identity no HTTP probe has stored (`identity_at` unset: HTTP
  unreachable, refused, or not yet tried) MQTT is a second source, one that never replaces the
  first: (1) `tele/<topic>/INFO1`, published at every boot, carries `Version`; it is read with
  the `StatusFWR.Version` grammar (`fwselect.DetectInfo1`). INFO1 has no hardware, so on its own
  it fills the version only (never image or variant, and nothing once the hardware is known) and
  the identity is not `Known()`. (2) Once a device has
  been online for 30 s (HTTP gets the first go) and still lacks its hardware, tasmofleet asks
  `Status 2` over MQTT (`cmnd/<topic>/STATUS` with payload `2`, through the configured
  FullTopic) and reads `stat/<topic>/STATUS2` through `fwselect.Detect`, so family, hardware
  and the `Known()` rule are the same as over HTTP. The question follows discovery's Status 5
  limits: only online devices, once per device per 10 minutes, at most one per second on the
  bus, one at a time per device, never `Status 0`, logged and audited (`identity.mqtt_ask`);
  minimal and safeboot firmware (no Status 2) are not asked, and an answer claiming such an
  image is refused. Only devices recorded as `ours` are read (an INFO1 seen earlier is kept
  until then); the online state is dropped when the broker connection is lost, so the grace
  starts anew after a reconnect. Only a reply within 30 s of the
  question is taken: anyone on the broker can publish a STATUS2. MQTT writes only the six
  identity fields (family, hardware, single core, version, image, variant) and only while
  `identity_at` is unset (`store.UpdateIdentityFromMQTT`, compare-and-set; the OTA preflight is an
  HTTP probe and sets it too); MQTT never sets
  `identity_at`, so the first HTTP probe still runs and its identity replaces the MQTT one, and
  a probe that finishes meanwhile always wins. OTA is unchanged: the engine probes over HTTP
  before it plans and `Guard` runs on that identity. A retained INFO1 may describe the last
  boot only; a device on a stale one is corrected by its next boot or its HTTP probe.
- **Refresh identity (#91, #65).** `Prober.Refresh(ctx, id, actor)` re-reads and stores every
  identity field: firmware, build, chip, hardware and flash (Status 2/4, `GET /`), the network
  facts of Status 5 (hostname, IP, gateway, subnet mask, DNS servers; Wi-Fi from Status 11 follows
  in the background), MQTT broker (Status 6, SetOption3), the OTA URL (one read-only `Status 1`, best
  effort, never the `OtaUrl` command; credentials in the URL are stripped before it is stored, a value
  that cannot be parsed is dropped) and DeviceName, resolved topic and relay names (the FriendlyName
  array) from plain `Status`. Subnet, DNS, OTA URL and relay names are stored on the device
  (migration 0031: `net_subnet`, `net_dns`, `ota_url`, `relay_names`); minimal firmware and a failed
  `Status 1` keep the stored values. The group topic (`GroupTopic` of the same `Status 1` answer, or of
  the INFO1 boot announcement, normalised from `cmnd/<group>/` to `<group>`) and the times `ota_url` and
  `group_topic` were read are stored too (migration 0035: `group_topic`, `ota_url_read_at`,
  `group_topic_read_at`, the stamps NULL until read); the device list shows both as optional columns, read-only,
  and never reads a device or a backup file to fill them (#239).
  _MAC first:_ the probe sends `Status 5` first, without credentials, and compares the MAC
  (Wi-Fi or Ethernet) with the stored set before any other command. A device that answers it
  without credentials has no web password, and the whole probe runs without them; only a 401
  repeats `Status 5` with the stored credentials. Another MAC refuses the refresh
  (`app.RefreshRefusedError`, audit `device.refresh_refused`, shown on the device page; nothing
  written, nothing more sent). A device stored without a MAC gets the reported one bound
  (audit `device.mac_bound`) when a person refreshes it (`actor` set; operators may refresh, so
  an operator's refresh binds a missing MAC too, accepted 2026-10-26); automatic refreshes
  (discovery accept/reprobe, `actor` "") never bind (discovery D8). Only a valid, non-zero MAC
  (`inventory.NormaliseMAC`) is bound. A MAC another device holds is refused the same way
  (`store.ErrMACInUse`). Minimal firmware has no Status 5: a device with a stored MAC is then
  refused (`refusedUnverified`, the stored identity and `identity_at` stay); one without a MAC
  is refreshed. _Name and topic:_ the store records the DeviceName and topic of every probe
  (`device_name`, `device_topic`, migration 0029). The stored name follows the DeviceName while
  it is still the recorded one, or, for devices probed before 0029, while it is the hostname,
  topic or address tasmofleet filled in itself (not when the DeviceName is Tasmota's default
  `Tasmota`). A name the user chose differs from these and is kept (#52); the Identity card then
  shows the notice "Name on the device: X" (#125) while `device_name` differs from `name`, is not
  Tasmota's default `Tasmota` and is not equal to `device_name_kept`. Admins can answer it: "Use device
  name" sets the name through `Inventory.UpdateIf` (compare-and-set, audited as `device.update`), "Keep"
  stores the declined name in `device_name_kept` (migration 0030, audited as `device.name_kept`), so the
  notice returns only when the device reports another name. An answer for a name the device no longer
  reports is refused. Operators see the notice without the buttons. The stored topic is
  only a mirror used to match MQTT messages, so it always follows the topic the device reports
  (#111; also for rows from before migration 0029, whose `device_topic` is empty), audited as
  `device.topic_changed` with old and new. If another device already has that topic, the
  stored one is kept, `device.topic_kept` is audited and the device page says so after the
  refresh (`TopicKeptError`). The retained LWT of the new topic is applied at once by the
  health reload (#105); what health learned on the old topic (its LWT, often the Offline Tasmota
  sends there on a topic change) is dropped first (#119). _Transport:_ for a device recorded as `ours` whose HTTP probe could not be used
  (unreachable, timeout, HTTP API refused, address outside the device networks) the refresh
  goes over MQTT: `Status 5`, MAC rule as above, then `Status 2`, one question at a time per
  device and one per second on the bus (shared with the #87 reader; if the reader asked the
  device between the two, the refresh waits 2 s and asks once more), audited
  (`identity.mqtt_ask`), a live answer on the device's own topic within 10 s. MQTT data stays
  the weaker source: it writes the identity only while `identity_at` is unset
  (`UpdateIdentityFromMQTT`), never sets `identity_at`, refuses minimal/safeboot answers, binds
  no MAC and leaves the topic alone; the network facts are stored (`UpdateNet`). It also reads
  the DeviceName with the plain `Status` (never Status 0) and stores it with `UpdateProbeName`
  under the same follow rule as over HTTP; that call never writes the identity (#125). This is
  accepted and cosmetic: anyone with broker access can change the name of a device whose name was
  never edited by hand; a hand-set name only gets the notice below.
  Contract change in this step: `web.Prober.Refresh` takes the actor; `store.ProbeResult` gained
  `DeviceName`, `NameFrom`, `DeviceTopic`, `BindMACs` and lost `Topic` (the old repair rule is
  part of the topic rule above).
- Minimal firmware never persists settings (SRC: `SettingsSave` is compiled out). An
  `OtaUrl` set while minimal runs lives in RAM only.
- Command output over HTTP depends on `WebLog >= 2`. If a reply is a `WARNING` about the
  web log level, the client sets `WebLog 2` once, logs it (flash write!), and retries.
- **Push upload (F-9, optional per-transaction delivery, ESP32 only; SRC `xdrv_01_9_webserver.ino`)**:
  the web UI's "Upgrade by file upload" is `GET /up` then a multipart `POST /u2?fsz=<size>`
  (field `u2`, non-empty file name, first byte `0xE9` or `0x1F` for gzip, exact
  `Content-Length`, no chunking). The upload type is set by the `GET /up`, not by the
  POST: a POST without it answers "Upload Successful" and restarts the device without
  flashing. `Client` therefore fetches `/up` first, requires the upload form
  (`action='u2?fsz='`) and holds the device slot for both requests (`tasmota.Uploader`).
  The reply page says `Successful` or `Failed` plus a reason; a refusal (or a missing
  form) is certain, nothing was flashed, and the job fails at once instead of waiting for a
  reboot. Any other error after the body started is not certain and is probed. The image
  must fit Status 4 `Free` less one 4 KiB sector (Tasmota's `Update.begin` bound); an
  ESP32 with the safeboot layout (one app partition) reports only the rest of its running
  partition there and cannot take a full image while running (research:
  `tasmota-minimal-and-ota.md`, "File upload on an ESP32 with the safeboot layout"). The state machine is unchanged: `set_ota_url` only runs Guard,
  `upgrade_issued` uploads, a lost reply is probed and never resent, `OtaUrl` is never
  touched. The choice is "push where possible": the download stays the default for every
  device, and with push chosen each device that cannot take an upload (ESP8266, unknown
  chips, safeboot/minimal, staged plans, an image larger than the free flash or an
  unknown free flash) gets the download instead. The preview says per
  device which delivery it gets and why, the job stores the delivery it really uses
  (shown on the transaction page), and a job submitted as a push whose plan no longer
  allows one fails before touching the device instead of switching unseen. A push job
  sends its image once (no second attempt after a stalled or lost upload). ESP8266 stays
  on the download until a real-hardware test.
- **Push into safeboot (#30, ESP32 with the safeboot layout; gate `safebootPushEnabled`, open since the
  real-device test)**: an image larger than the running partition's free flash goes
  through the factory partition, as the web UI does. `GET /up` tells the layout without side effects:
  the button calls `upl(this)` only in the single-OTA form (`tasmota.ParseUpLayout`, research doc).
  The job (`Job.ViaSafeboot`, decided in the preview, `PreviewItem.ViaSafeboot`) runs
  `set_ota_url` (Guard only) -> `safeboot_switch` (Guard, **one** `GET /u4?u4=fct&api=`; `false`
  means restarting; probe until the device answers as safeboot, `SafebootWindow`) -> `upgrade_issued`
  (Guard again, `/up` must now show the plain form, one upload to `/u2`) -> `await_reboot` -> `verify`.
  `/u4` is never re-sent (the browser script re-sends it every 6 s; tasmofleet does not) and a lost
  reply is probed. A job that ends with the device in or possibly in safeboot (upload refused or lost,
  deadline in safeboot, Guard refusal, cancel) goes through `safeboot_recover`: probe first, **one**
  `GET /u4?u4=ota&api=` on the first answer from safeboot (an app answering needs nothing, a device
  still offline would swallow it; `Job.RecoveryStep` is stored before the request and a second
  recovery after a failed verify is refused); the old version answering ends the job `failed` ("Upload into safeboot
  failed; the device was switched back to <old>"), anything else ends it `failed` with the address
  and the manual instruction. A restart of tasmofleet resumes both states by probing only. The image
  must start with `0xE9` (checked before the device is restarted). With the gate closed the preview says
  "Downloads instead: push into safeboot is not enabled yet". `await_reboot` may be skipped: when the first
  probe after the upload already sees the new app (uptime below the reboot proof), the job goes straight to `verify`.
- Device output is untrusted text. It is size-limited and parsed with tolerant JSON
  (control characters stripped), and HTML-escaped by templ on output.

### 5.x Transport and path choice (package `transport`, `Choose`)

`transport.Choose(Facts) Paths` decides how each action reaches a device. It is pure
(`internal/transport/choose*.go`, depguard, mutation-gated) and returns one `Path` per action
(`HTTP | MQTT | DownloadHTTP | DownloadMQTT | PushHTTP | PushSafeboot | PushMQTT | None`) with one
English sentence each (`Paths.Reasons`), shown verbatim in the UI and in errors. `Facts` come from
a `FactsSource` (`internal/app/transportfacts.go`): HTTP state (ok = success within 15 min; failing =
the router remembers failed calls since a time and a kind, cleared by a later success; else unknown),
MQTT usability, the MQTT password state, `FWReach` ("no" older than 24 h counts as unknown),
whether FIRMWARE_BASE_URL is set, the identity, and for an update the request (Delivery,
AllowMQTTUpload, the `fwselect.MQTTUploadFits` verdict, the HTTP push decision the OTA engine makes).

MQTT is usable when all hold: the bus is connected, `Device.MQTT` is ours, the topic is one device's
topic (non-empty, no `/ + #`, no whitespace, not `tasmotas`), no device in the inventory listens on it
(`TopicExclusive`), the FullTopic equals MQTT_FULLTOPIC (empty = `%prefix%/%topic%/`, trailing `/`
ignored), the topic is not the device's own GroupTopic (even a stale value blocks), and the last will says
Online. `mqttcmd` checks the topic again before every publish.

Exclusivity is decided on command stems, expanded as Tasmota's `GetTopic_P` does with the bus's real command
prefix (mqttbus `Prefixes[0]`): `%prefix%`, `%topic%`, `//` collapsed, `/%prefix%` appended when missing,
trailing `/` dropped. Tasmota subscribes to stem + `/#`, so two stems collide when they are equal or one is a
level prefix of the other (`cmnd/a` and `cmnd/a/b`; not `cmnd/a` and `cmnd/ab`). The stem of the device's
Topic and of every device's GroupTopic (fresh or stale, the device's own included) are compared with it, plus
the raw topic strings. A FullTopic without `%topic%` or with another placeholder (`%hostname%`, `%id%`)
has no knowable stem: for this device it means not exclusive, and so does such a FullTopic on any other device
(unknown is not exclusive). Devices with neither Topic nor FullTopic are HTTP only and ignored.

Limits: only `GroupTopic` (1) is stored (Status 1), not GroupTopic2-4; devices outside the inventory on the
same broker are invisible. Both can still make a message reach more than one device.

The GroupTopic is trusted as "known" only when it is set, stamped and not older than the last Status 1
read (it can be stale: INFO1 is sent at boot, Status 1 only when probed). Commands are reads (Status N,
identity refresh) and tolerate an unknown own GroupTopic; Writes (settings, Make compliant, console
commands that change the device, backlog), transfers and the download need a known one and also that no
other device with a topic has an unread GroupTopic (empty, never stamped, or older than that device's last probe; a device whose last will is Offline is left out, since our commands are never retained, while its topic still counts for exclusivity): "N other devices' group topics
have not been read yet; refresh their identity". Reads are unaffected. The reason text says "refresh the
device's identity".

The consumer (the router) computes Facts and calls Choose from the same device snapshot it sends with, and
re-checks the topic at send time (`mqttcmd`), so an edit between the decision and the publish cannot redirect
a message.

Order (HTTP first: the established path with synchronous replies; MQTT makes devices HTTP cannot
reach manageable, #45). HTTP "not failing" means ok or unknown.

| Action | Rule |
| --- | --- |
| Commands (reads) | HTTP; MQTT when HTTP is failing and MQTT is usable; else None |
| Writes | HTTP; MQTT when HTTP is failing and MQTT is usable with a known own GroupTopic; else None |
| Backup | HTTP `/dl`; MQTT FileDownload when HTTP is failing and MQTT is usable with a password that is set and not refused; else None |
| Restore | HTTP WebGetConfig when HTTP is not failing, FIRMWARE_BASE_URL is set and FWReach is not "no"; else MQTT FileUpload Type 2 (usable, password usable); else None |
| Update, pull | DownloadHTTP (HTTP not failing, base URL set, FWReach not "no"); else DownloadMQTT (usable, FWReach not "no"); else None |
| Update, push where possible | HTTP not failing: the engine's decision (PushHTTP, PushSafeboot, or its fallback to DownloadHTTP, kept with its note). HTTP failing and MQTT usable: PushMQTT when the per-transaction "experimental" checkbox is set, the image fits and the password is usable; else DownloadMQTT; else None |

Both download paths also need FIRMWARE_BASE_URL (the device needs an address to fetch from). A password "not set", "refused since T" or in broker mode without a configured MQTT_PASSWORD removes transfers over MQTT, not commands. A None for an
update whose experimental upload would have worked ends with "choose Push where possible and allow
upload over MQTT (experimental)". There is no per-device "force transport" setting.

---

## 6. Identity detection (package `fwselect`, `Detect`)

Inputs: `Status 2` (`StatusFWR`), `Status 4` (`StatusMEM`), and on minimal firmware the
`Upgrade` probe plus `RootPage`.

### 6.1 Chip family from `StatusFWR.Hardware`

| Hardware string (prefix match, case-sensitive)                            | Family                     | Source                           |
| ------------------------------------------------------------------------- | -------------------------- | -------------------------------- |
| `ESP8266EX`, `ESP8266`                                                    | `esp8266`                  | OBS                              |
| `ESP8285N08`, `ESP8285H16`, `ESP8285`                                     | `esp8266`                  | OBS / SRC                        |
| `ESP32-D0WD`, `ESP32-D0WDQ6`, `ESP32-D0WD-V3`, `ESP32-D2WD`, `ESP32-PICO` | `esp32`                    | OBS (`ESP32-D0WDQ6 v1.0`) / SRC  |
| `ESP32-U4WDH` (`-D`: dual core)                                           | `esp32`                    | SRC (`ESP32-U4WDH-D`)            |
| `ESP32-S0WD`, `ESP32-SOLO`, `ESP32-U4WDH-S` (single core)                 | `esp32`, `SingleCore=true` | SRC / OBS (`ESP32-U4WDH-S v3.0`) |
| `ESP32-S2`                                                                | `esp32s2`                  | SRC                              |
| `ESP32-S3`                                                                | `esp32s3`                  | SRC                              |
| `ESP32-C2`                                                                | `esp32c2`                  | SRC                              |
| `ESP32-C3`                                                                | `esp32c3`                  | SRC                              |
| `ESP32-C5`                                                                | `esp32c5`                  | SRC                              |
| `ESP32-C61` (check before `ESP32-C6`)                                     | `esp32c61`                 | SRC                              |
| `ESP32-C6`                                                                | `esp32c6`                  | SRC                              |
| `ESP32-P4`                                                                | `esp32p4`                  | SRC                              |
| anything else (e.g. `ESP32-H2`, `ESP32-S31`, empty)                       | `unknown` -> never updated | DEC                              |

Longest prefix wins. The table lives in one place (`fwselect`) with a test row per entry
and one per captured fixture. Unknown is final: no fallbacks, no substring guesses.
`ESP32-S31` and other new chips are added only together with a fixture.

### 6.2 Image, variant and language from `StatusFWR.Version`

`Version` = `<version>(<sha-or-release>-<image>)<cores>` (SRC: `support_command.ino` Status 2 prints
version, image name and `GetCodeCores()`), e.g. `16.0.0(release-tasmota)`, `16.0.0(release-tasmota32)`,
`16.0.0(release-minimal)`, dev builds `16.0.0.3(85cff52-tasmota)`, solo1 `16.0.0(release-solo1)single-core`:

- `<version>` is `<n>.<n>.<n>`, plus `.<n>` for development and patch builds (`tasmota.ino`). Nothing else
  (no `-dev`, no two- or five-field versions).
- `<cores>` is empty or `single-core` (only builds with `CORE32SOLO1`, `support_esp32.ino`; Tasmota never
  prints `dual-core`). `single-core` on hardware that is known and not the classic ESP32 is `ErrFamilyMismatch`.
- A `Version` that does not follow this grammar does not abort `Detect` (#45): the identity keeps family,
  hardware, language and flash, `Version` stays empty and the variant `unknown`. It is never `Known()`, so
  Guard refuses every OTA (rule 1), the same contract as unknown hardware. Nothing is guessed from the string.
  The minimal-firmware path (Upgrade probe and main page) stays strict.

`testdata/fixtures/firmware-versions.json` lists every shape with its source.
`Identity.Image` keeps the raw tag. A self-compiled build reports the bare environment name
(`16.0.0(tasmota32)`, no prefix): the variant stays `unknown`, so OTA never replaces a custom build
without an explicit choice; the UI shows the reported name as "tasmota32 (self-compiled)". The variant is normalised:

The image tag is the build's `CODE_IMAGE_STR` (SRC, arendst/Tasmota `63c3861`; every row with its source line is in
`testdata/fixtures/firmware-versions.json`), not the upstream file name (#85). The chip always comes from
`Hardware` (§6.1):

| image tag (after the `<sha-or-release>-` prefix)                               | runs on (Hardware)         | Variant -> file (§7.1)                                                  |
| ------------------------------------------------------------------------------ | -------------------------- | ----------------------------------------------------------------------- |
| `tasmota`                                                                      | esp8266                    | `tasmota` -> `tasmota.bin.gz`                                           |
| `minimal`                                                                      | esp8266                    | `minimal`                                                               |
| `lite`, `sensors`, `knx`, `zbbridge`, `zigbee`                                 | esp8266                    | same -> `tasmota-<v>.bin.gz`                                            |
| `tasmota-4M`                                                                   | esp8266                    | `4M` -> `tasmota-4M.bin.gz`                                             |
| `display`, `ir` (one `CODE_IMAGE_STR` for both families)                       | esp8266 or esp32 dual core | same -> `tasmota-<v>.bin.gz` / `tasmota32-<v>.bin`                      |
| `webcam`, `bluetooth`, `nspanel`, `zbbrdgpro`                                  | esp32 dual core            | same -> `tasmota32-<v>.bin`                                             |
| `lvgl-haspmota` (`lvgl` up to v11)                                             | esp32 dual core            | `lvgl` -> `tasmota32-lvgl.bin`                                          |
| `tasmota32` (every `FIRMWARE_TASMOTA32` build but solo1, one per chip)         | esp32 dual core            | `tasmota` -> `tasmota32.bin`                                            |
|                                                                                | s3, c2, c3, c5, c6, c61    | `tasmota` -> `tasmota32<chip>.bin`                                      |
|                                                                                | s2, p4                     | `unknown`: two builds each (s2/s2cdc, p4/p4r3), the tag cannot tell     |
|                                                                                | esp32 single core          | `unknown`: no official build reports this; not provable                 |
|                                                                                | esp8266                    | `ErrFamilyMismatch`                                                     |
| `solo1` (with suffix `single-core`)                                            | esp32 single core          | `tasmota` + `SingleCore` -> `tasmota32solo1.bin`                        |
| `safeboot`                                                                     | any esp32 chip             | `safeboot` (device sits in safeboot; treated like minimal)              |
| anything else, file names (`tasmota32c3`, `tasmota32-webcam`), bare tags (#52) |                            | `unknown` -> refuse updates until the user chooses a variant explicitly |

History and limits (SRC):

- The `tasmota32` tag exists since v12.5 (`tasmota_configurations_ESP32.h`). ESP32 builds up to v12.4 report
  `release-tasmota`, which `Detect` refuses as `ErrFamilyMismatch` on ESP32 hardware (known limitation, fails
  safe).
- Until v14.0.0 the USB-CDC builds of C3, C6 and S3 were separate OTA files (`tasmota32c3cdc.bin`, ...); since
  v14.0.0 one build per chip has USB HWCDC with serial fallback (Tasmota CHANGELOG, #21212). The cdc files are
  not in §7.1 and never selected.
- S2 and P4 identify with variant `unknown`. After an explicit variant choice, Select still has to prove the
  build (§7.2): P4 by the chip revision Status 2 reports in `Hardware` (`ESP32-P4 v<major>.<minor>`): `p4`
  (board `esp32p4`, chip_variant `esp32p4_es`, built for revisions 0.1-1.99) or `p4r3` (ESP-IDF default
  3.1-3.99, `Kconfig.hw_support`); any other revision is refused. S2 is refused
  (`ErrBuildChoiceNeeded`, which is an `ErrUnknownHardware`) unless the user chose the build: nothing the
  device reports tells `s2` from `s2cdc`. The choice is `Choice.Build` (§7.2). After an update the device
  reports the shared tag, so its variant reads `unknown`: `DetectedVariant(Selection)` is the variant
  `Detect` will read back for a selection (`unknown` for S2 and P4, the selected variant otherwise), and
  the OTA verify step compares with it instead of the selected `tasmota` (family, version, language and
  reboot proof are checked as before).

With unknown hardware the tag is not checked (the identity is never `Known()`). If the
tag's chips do not include the Hardware family (e.g. `webcam` on an ESP32-C3), `Detect`
returns `ErrFamilyMismatch`. This must never happen, so if it does, stop.

Language builds (`tasmota-DE.bin.gz`, `tasmota32-DE.bin`) keep the image tag `tasmota` /
`tasmota32`. The language is read from `<html lang>` on `GET /` (`de` -> `DE`, `en` ->
empty), and Detect takes it from `RootInfo`.

**Language is known only from the web page.** Tasmota prints the language build in `<html lang>` of the
main page and nowhere else: not in `Status 2`, not in `INFO1`. `Identity.LanguageKnown` is true only when
the identity was detected with a `RootInfo` whose `LangKnown` is set (`tasmota.ParseRootPage` sets it when
the page carries a non-empty `lang` attribute, so every HTTP probe does). An identity read over MQTT, or
stored before 0.20.0, keeps whatever language it had and `LanguageKnown` false; an empty `Language` is then
no proof of the English build (D10). `mqttidentity` and the MQTT identify keep both fields of the stored
identity, they never set them.

### 6.3 Flash from `Status 4`

`FlashSize`, `ProgramFlashSize`, `Free` (KB). The fleet's 1 MB devices report
`FlashSize 1024` (fixtures `livingroom-readinglamp`, `bedroom-cabinet`). Values are stored in
`Identity` and refreshed on every preflight. When firmware is minimal, `Status 4` is not
available and the last stored values are kept.

### 6.4 MQTT capability (`Device.MQTT`)

From `Status 6` (`StatusMQT`): missing, `MqttHost` empty, or SetOption3 off -> `none`.
Otherwise `other`, until a message on our broker matches the device's topic -> `ours`.
Health uses MQTT only for `ours` devices. A device is never marked degraded because MQTT
is missing (TasmoAdmin `mqtt_expected` lesson).

### 6.5 Commands over MQTT (package `mqttcmd`, #256)

`mqttcmd.Channel` is the one place that publishes a command to `cmnd/<topic>/...` and reads its
answer. Rules (each has a test; see also `docs/research/tasmota-mqtt-transport.md`):

- Never `Status 0` in any spelling `cmdpolicy` reads as 0, alone or inside a Backlog (`ErrRefused`,
  nothing published). Group topics, topics with `/+#` and `FileDownload`/`FileUpload`/`Upload`
  are refused too, and so is every command that runs other commands, alone or in a Backlog: `Json`
  (`{"Status":0}` is a Status 0), `Publish`, `Publish2`, `Rule` with a rule text (a bare `Rule1` or
  `Rule1 1` that only reads or switches is fine), `RuleTimer`, `WebSend`, `WebQuery`, `Br`, `Script`, `UfsRun`.
  They stay available over HTTP, where an admin confirms them.
- One command in flight per device topic; at least 100 ms between two publishes on the bus.
- The listener is registered before the publish, and `Bus.Connected()` is true only once the
  subscriptions of the current connection are in place (#272).
- The answer is the first live (never retained) `stat/<t>/STATUSn` for `Status n`, else a `RESULT`
  whose top-level keys name the command and its index (`POWER1` answers `Power1`, not `Power2`),
  or `{"Command":"Unknown"|"Error"}` for that command. Other messages on the topic (button
  presses, other pollers) are ignored. `DoLoose` (console) takes the first `RESULT` for
  `Template` and `Gpios` only.
- A Backlog has no merged reply over MQTT: one `RESULT` per queued command is collected and
  merged (later keys win).
- LWT Offline -> `tasmota.ErrUnreachable`, nothing published. No answer in 8 s -> `tasmota.ErrTimeout`
  (unknown outcome, never resent by the channel). Replies to credential-setting commands are
  scrubbed (`cmdpolicy.ScrubReply`).
- `Close()` while a command waits for its answer returns an unknown outcome (`ErrTimeout`), not
  "not sent": the command was published. A closed channel never publishes.
- `Info(topic)` returns the INFO1/INFO3 seen since the device's last LWT Offline (every software
  restart publishes one).

---

## 7. Firmware selection and the safety guard (package `fwselect`)

### 7.1 Artifact naming (upstream, verified on release v16.0.0)

| Family              | Directory on ota.tasmota.com | File                                                                                                                                                     |
| ------------------- | ---------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| esp8266             | `/tasmota/release/`          | `tasmota.bin.gz`, `tasmota-<variant>.bin.gz` (`minimal`, `lite`, `sensors`, `ir`, `knx`, `display`, `zbbridge`, `zigbee`, `4M`), `tasmota-<LANG>.bin.gz` |
| esp32               | `/tasmota32/release/`        | `tasmota32.bin`, `tasmota32-<variant>.bin`, `tasmota32-<LANG>.bin`, `tasmota32solo1.bin`                                                                 |
| esp32 chips         | `/tasmota32/release/`        | `tasmota32<chip>.bin` (`s2`, `s2cdc`, `s3`, `c2`, `c3`, `c5`, `c6`, `c61`, `p4`, `p4r3`)                                                                 |
| never an OTA target |                              | `*.factory.bin` (serial flashing), `tasmota32*-safeboot.bin`, `*.map.gz`                                                                                 |

ESP8266 uses `.bin.gz` (gzip OTA since Tasmota 8.2). ESP32 uses the plain `.bin` app image.
GitHub release assets (`arendst/Tasmota`, tag `v16.0.0`) have the same file names.

### 7.2 Select

`Select(identity, choice)` returns the file name. It keeps the detected variant, language
and chip build unless the user explicitly chose otherwise (`Choice.AllowVariantChange`,
set only from a UI confirmation or a CLI flag). For esp8266 it always also names
`tasmota-minimal.bin.gz` of the same release. Language and variant cannot be combined
upstream (there is no `tasmota-sensors-DE`). Asking for that returns `ErrNoSuchVariant`.
The chip build of the newer chips comes from `Hardware` alone: the one build of S3, C2, C3, C5, C6 and C61; for
P4 the build whose revision range contains the chip revision (§6.2); every other case returns
`ErrUnknownHardware`. ESP32-S2 returns `ErrBuildChoiceNeeded` (an `ErrUnknownHardware`) unless
`Choice.Build` is `s2` or `s2cdc` (`BuildOptions(identity)`); that explicit build implies the plain `tasmota`
variant and the change from the detected `unknown` of an official `tasmota32` tag, and only it does (a
self-compiled or custom S2 image stays `unknown` and needs an explicit variant choice as well, §6.2). A build on any other chip, or one that
contradicts a provable revision (`p4` on a 3.1 P4), returns `ErrNoSuchVariant`; a P4 outside the known
revisions stays refused with or without a build. The build is chosen per update in the update preview and
kept on the job (`Job.TargetBuild`, `ota_jobs.target_build`, migration 0033); Guard rule 7 holds the artifact
to it. Remembering the choice per device is not part of this. This also covers the minimal/safeboot auto-default ("tasmota" chosen for the device).

**Language unknown.** When `Identity.LanguageKnown` is false and `Choice.Language` is empty, Select returns
`ErrLanguageUnknown` unless English is proven without a page (below; checked right after the unknown-hardware check, before any variant rule, because the
language is what the person must supply). The language is proven when no language build can be running: an official tagged image (`Image` has a
`<release>-` prefix), no stored language, not minimal/safeboot, and no language build for the hardware and variant
(`hasLanguageBuild`: only `tasmota` or `unknown` on an ESP8266 or a dual-core ESP32 has one). So C3/S3 and the other
chips, solo1 and every non-`tasmota` variant need no page. `Choice.Language = "EN"` is a valid explicit answer, as is any
upstream language suffix. Guard rule 7 runs Select, so an unread language also stops an OtaUrl or Upgrade. Devices stored before 0.20 whose language was read from a page are
marked known by migration 0037 (identity_at set, and the identity of every stored job). A
synthetic identity that stands for no device (the custom-upload file name) is built with `LanguageKnown`
true. This mirrors the ESP32-S2 build choice (`ErrBuildChoiceNeeded`): never guess. The update preview
offers the choice (WP-D2).

### 7.3 Decide (update policy)

Per batch: `only-newer` (default), `reinstall`, `allow-downgrade`. Versions compare
numerically per dotted field. A device on minimal/safeboot always decides "upgrade". One
function, table-tested (TasmoAdmin lesson A5).

### 7.4 InspectImage (verified against `testdata/fixtures/firmware-headers/`)

- Byte 0 must be `0xE9`, otherwise `ErrBadImage`. `.bin.gz` is gunzipped first; byte 0-1
  of the file must be the gzip magic `1f 8b`.
- ESP32 app image: byte 8 = `0xEE` and `chip_id` = uint16 LE at offset 12. Mapping:
  `0x0000` esp32, `0x0002` s2, `0x0005` c3, `0x0009` s3, `0x000C` c2, `0x000D` c6, `0x0012`
  p4, `0x0014` c61, `0x0017` c5. Unknown chip_id -> `ErrBadImage`.
- Otherwise ESP8266 (no extended header; byte 8 is a segment load address byte).
- solo1 and dual-core ESP32 share chip_id 0. The file name and `SingleCore` decide.

The firmware cache runs InspectImage on every download and rejects files whose header
family disagrees with their name.

### 7.5 Guard (hard, runs immediately before `OtaUrl` and again before `Upgrade`)

Refuses, with a typed error, unless **all** hold:

1. `identity.Known()` and family != unknown -> else `ErrUnknownHardware`.
2. `artifact.Family == identity.Family` (header-proven) -> else `ErrFamilyMismatch`.
3. The file-name family (`tasmota32c3...` -> esp32c3, `tasmota-*`/`tasmota.bin.gz` -> esp8266)
   equals both, and `path.Base(otaURL) == artifact.Filename`, and the URL is the
   firmware server's URL for that artifact -> else `ErrFilenameMismatch`.
4. Not `artifact.Safeboot`, not `*.factory.bin` -> else `ErrSafeboot`.
5. esp8266 artifacts end in `.bin.gz`, esp32 artifacts end in `.bin` -> else `ErrFilenameMismatch`.
6. Single-core ESP32 gets only `tasmota32solo1*`, dual-core never gets solo1 -> `ErrFamilyMismatch`.
7. Variant/language equal the detected ones, or `AllowVariantChange` is set -> else
   `ErrVariantChange`. Exception: the minimal stage artifact for esp8266.
8. URL length ≤ 100 characters (Tasmota settings text pool) -> else `ErrFilenameMismatch`.

The engine also checks that the `Upgrade 1` reply `Version X from <url>` echoes exactly
the URL it set. If not (another tool changed OtaUrl in between), the job fails.

Guard and Plan run under mutation testing with a required efficacy ≥ 90 % (§21).

### 7.6 Plan (per-family update policy)

| Family  | Plan                                                                                                                                                                                                    |
| ------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| esp32\* | **direct**. If the device uses a single OTA partition with safeboot, Tasmota itself goes through safeboot and continues with the stored OtaUrl (SRC). The engine tolerates seeing `safeboot` in between |
| esp8266 | **staged** if `target .bin.gz size > Free*1024 - 16 KiB` (margin), or if Free is unknown and `FlashSizeKB <= 1024`; otherwise **direct**                                                                |

Never decided by model name. Fleet check (OBS, fixtures): every ESP8266 in the fleet, the
ESP8285N08 lamps and the ESP8266EX plugs alike, reports `FlashSize 1024`,
`Free 352` (360 448 bytes). `tasmota.bin.gz` 16.0.0 is 469 152 bytes and
`tasmota-sensors.bin.gz` is 512 993, so they are **staged**. `tasmota-minimal.bin.gz`
(263 670) fits. The compressed image is what lands in flash (the bootloader unpacks it),
so the gz size is the one to compare. For ESP32, `Free` (697 KB on the QuinLED) is not
the OTA partition size and is ignored; ESP32 is always direct.

### 7.7 MQTT upload fit (`MQTTUploadFits`)

`MQTTUploadFits(identity, layoutHint, imageSize) (ok, needKB, roomKB, reason)` decides before anything is sent
whether Tasmota will accept the image as `FileUpload` Type 1. It is pure and never "tries and sees". The rule,
for both ESP32 flash layouts and without knowing which one the device has:

```
round_up_4096(imageSize) <= FreeKB * 1024 - 192 KiB          FreeKB = Status 4 "Free"
```

Refused outright, each with its own reason: ESP8266 (its minimal firmware has no `FileUpload`), identity not
known, variant `minimal` or `safeboot` (neither has `FileUpload`), a known safeboot layout, `FreeKB` <= 0, image
size <= 0. `UpLayoutHint` (`unknown`, `direct`, `safeboot`; from a read of `/up`, #30): `safeboot` (one app
partition) is refused outright, because `MqttFileUploadValidate` calls `Update.begin` without the single-OTA
handling of the web upload and the next update partition can be the running one. The other values never change
the numeric decision. An unknown layout keeps the numeric rule until WP-D2/G1 have checked the Arduino core's
Updater.

Derivation (SRC: Tasmota v16.0.0, `xdrv_02_1_mqtt_file.ino` `MqttFileUploadValidate`; `support_esp32.ino`):

- The device refuses (error 2, "Not enough space") when `round_up_4096(file_size) > head_room`, with
  `head_room = (FlashWriteMaxSector() - FlashWriteStartSector()) * 4096`.
- `FlashWriteStartSector()` = `EspFlashBaseAddress() + 2 * 64 KiB`, rounded down to 64 KiB.
  `FlashWriteMaxSector()` = `EspFlashBaseEndAddress()` rounded down to 64 KiB.
- Two app partitions (direct): `EspFlashBaseAddress()` is `esp_ota_get_next_update_partition()->address`, the
  start of the other partition, and `EspFlashBaseEndAddress()` is its end. `ESP_getFreeSketchSpace()` (what
  Status 4 prints as `Free`) is `ESP.getFreeSketchSpace()`, the size of that same partition. So
  `head_room > Free - 128 KiB - 64 KiB` (rounding the end down loses less than 64 KiB); the size of the running sketch is irrelevant.
- One app partition next to safeboot: `EspFlashBaseAddress()` is `running->address + ESP_getSketchSize()`, the
  end is the end of the partition, and `Free` is `partition size - sketch size`
  (`ESP_getFreeSketchSpace()`, `EspSingleOtaPartition()` branch). So again `head_room > Free - 128 KiB - 64 KiB`.
- Status 4 prints `Free` in whole KB (rounded down), which only makes the figure smaller and the bound more
  conservative. 128 + 64 = 192 KiB. The bound holds without knowing the layout.

The rule does not use `ProgramSize`: on a dual layout it plays no part. The simulator's esp32-dual profile reports
`Free` 2880 KB (the other partition) next to the safeboot capture's `ProgramSize` 2182 KB. Not verified on a real
dual-OTA device (#263).

---

## 8. Firmware sources, cache and server (package `firmware`)

### 8.1 Sources

| Name              | Kind              | Index                                                                                                                                     | Download hosts (allowlist)                                                                              |
| ----------------- | ----------------- | ----------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| `github`          | `github-releases` | `GET https://api.github.com/repos/arendst/Tasmota/releases?per_page=10` (JSON, machine-readable)                                          | `api.github.com`, `github.com`, `objects.githubusercontent.com`, `release-assets.githubusercontent.com` |
| `ota.tasmota.com` | `ota-tasmota`     | HTML listing of `/tasmota/release/` and `/tasmota32/release/` (only `href` file names are parsed; version from the release notes heading) | `ota.tasmota.com` (http allowed)                                                                        |

`FIRMWARE_SOURCES` (System settings, applied at the next restart) orders them. A file only one source offers is refused unless `FIRMWARE_ALLOW_SINGLE_SOURCE` is on. GitHub is the primary index because it is JSON. If both
sources are enabled, every download is fetched from both and the SHA-256 must match
(cheap cross-check against a tampered mirror). A mismatch rejects the file and raises an
error in the UI. The index is refreshed at start (async, backoff 30 s..30 min until the first success) and then every `FIRMWARE_REFRESH_INTERVAL` (default 6 h); a failed refresh keeps the previous index (gauge `tasmofleet_firmware_index_age_seconds`). It is never fetched on page render
(TasmoAdmin lesson B6), only by an explicit refresh or a background refresh (default 6 h).

### 8.2 Cache

`/data/firmware/<release>/<filename>`, immutable, indexed in `firmware_artifacts` with
size, SHA-256, header-proven family, variant, language and safeboot flag. Downloads go to
a temp file, then fsync and rename. Size limit 8 MiB. A redirect hop outside the allowlist
aborts. The cache is never pruned automatically: `Cache.Prune(keep)` exists, but nothing calls it and there is no
`FIRMWARE_KEEP_RELEASES` setting (the original plan kept the newest 3 releases). Files leave the cache by the
admin's Delete and "delete unused" (§8.2a). Uploads never wipe the cache (TasmoAdmin lesson B6).

### 8.2a Firmware page: cache management, prefetch, changelog (M2)

- **Release notes.** The GitHub index already carries each release's `body`; the cache keeps it
  (cut to 32 KiB per release, at most the 10 indexed releases) next to the index and the
  release page URL. The `ota.tasmota.com` source has none. Notes are rendered by `firmware.ParseNotes`,
  a minimal Markdown subset (headings, bullet/ordered lists, fenced code, quotes, `**bold**`,
  `` `code` ``, `[text](url)`, bare URLs) that produces plain data, never HTML. templ escapes every
  string; there is no `template.HTML` on the path. Raw HTML in a body is shown as text. A link survives only if
  `firmware.NoteLink` accepts it (absolute `http`/`https`, public DNS host, standard port, no user info, no whitespace or control
  characters) and its host must be on an allow-list (project and GitHub hosts, configurable); it is rendered with `rel="noopener noreferrer nofollow"` and `target="_blank"`. Images are never
  loaded (shown as links). **No new dependency:** goldmark plus bluemonday would be a large parsing and
  sanitising surface for four constructs, and a sanitiser allow-list is the weaker design than "the parser cannot
  emit markup".
- **Delete.** `firmware.Manager.Delete` / `DeleteUnused`: admin only, a confirmation page, audited
  (`firmware.delete`, also the refusals). Refused while an active or queued job holds the artifact as target
  or tasmota-minimal ("in use by job N"); refused if the lookup fails. The same per-file lock as `Ensure`
  serialises a delete against a download of that file. Residual: the window is the whole planning step (it can include a download); the job then fails on the artifact id mismatch, never flashes the wrong file. For direct ESP8266 plans the job row records the minimal image too.
- **Fleet needs.** `firmware.FleetNeeds(release, devices)`: per device `fwselect.Select` (keeping variant
  and language; a device on minimal/safeboot gets the standard build) plus `tasmota-minimal.bin.gz` for every
  ESP8266, excluded devices and devices with unknown hardware need nothing. This is the single computation behind the
  per-release coverage ("cached N of M needed files"), the "devices that would use this file" column, and
  the prefetch file list. It makes no safety decision; the OTA engine still runs the Guard.
- **Prefetch.** `POST /firmware/prefetch` (admin) computes the file list on the server from the inventory (the form
  only names a release that is in the index), starts `firmware.Prefetcher` (one run at a time, sequential
  `Cache.Ensure`, so size limit, cross-check and image-header check are unchanged, 30 minute timeout, audited
  `firmware.prefetch`) and redirects to a progress page fed by an SSE stream like the discovery scan.
- **Update indicator.** `firmware.UpdateAvailable(latest, identity)` is the one definition of "update
  available" (known version older than the newest stable release) for the firmware page and the device list;
  `firmware.Latest` is the "recommended" release.

### 8.3 Device-facing firmware server

Its own listener (`FIRMWARE_LISTEN`, default `:80`). It serves only `GET`/`HEAD`
`/fw/<release>/<filename>` for indexed artifacts, with `Content-Length` (ESP8266 checks
free space from it) and `Content-Type: application/octet-stream`. There is no directory
listing, no other routes, and no authentication (devices cannot authenticate). Every
request is logged with the device IP and published as an event. The engine uses "device X
started downloading" as a progress hint. All files of one release share a directory, so
Tasmota's own fallback (it rewrites the OtaUrl file name to `tasmota-minimal.bin.gz` in the
same directory, SRC) also resolves. It also serves `/restore/<token>` (§12.4) and, for uploaded firmware, `/c/<key>/fw/<release>/<filename>` (§8.6). Requests are rate limited per client IP (120 per minute).

`FIRMWARE_BASE_URL` is the URL devices use (`http://<container-ip>`). It must be plain
`http://` (validated). There is no auto-detection: in ipvlan/bridge setups the right
address is only known to the operator.

### 8.4 Variant list for the UI

The update dialog offers, per device, only files of the device's family that exist in
the chosen release (from the index). The default is the detected variant.

### 8.5 Channels

`stable` (releases) only. A `development` channel (ota.tasmota.com root) was planned and is not built.

### 8.6 Custom firmware upload (M2)

Admin uploads `.bin`/`.bin.gz` (`POST /firmware/custom`, multipart, CSRF token in the
header, streamed to a temp file) with a label, a dotted numeric version (the release
directory) and a build name (default `tasmota`). Container (gzip magic) and chip family come
from the bytes via InspectImage; the file name and MIME type are display data. Limits on the
uncompressed image: 4 MiB ESP8266, 8 MiB ESP32. Refused: unknown or mismatching chip,
safeboot/factory/bootloader names, a partition table at 0x8000 (full-flash image), corrupt
or tiny files, the same bytes twice. The version may equal an upstream release (self-built
images report the version they were built from): an upload is identified by `Artifact.Custom`,
the first 10 hex digits of its SHA-256, and fetched at `/c/<key>/fw/<release>/<file>`
(`Request.Custom`/`Job.TargetCustom` carry the key; the engine resolves such jobs with
`EnsureCustom`, never `Ensure`). The Guard is unchanged. The image is stored
as `custom/<key>/<version>/<name from fwselect.Select>` (ESP8266 gzip, ESP32 raw, whatever was uploaded)
with `Source=custom` and provenance in `firmware_custom` (the release column of an upload is stored as `<key>~<release>`, so the old UNIQUE(release, filename) holds) (label, uploader, time, original
name). An ESP8266 custom release gets `tasmota-minimal.bin.gz` as a verified copy of the
newest upstream minimal (`Source=custom-minimal`) on first use, so the two-step update
works. Uploads are exempt from Prune and "delete unused"; Delete refuses while a job needs
them. The update page offers them as `custom: <label>`, never as default or recommended.
The same Guard applies; audit: `firmware.custom.upload`, `firmware.custom.use`,
`firmware.delete`. Security notes: docs/security/custom-firmware.md.

---

## 9. OTA state machine (package `ota`)

### 6.6 The transport router (package `transport`, #256)

`transport.Router` implements `tasmota.Client` and is `app.Services.Client`; `Services.HTTP` is the raw
HTTP client with the router's failure memory attached, used by what measures or verifies HTTP (health
poll, prober, MAC verifier, discovery scan). The router passes `Uploader` and `BootSwitcher` straight to
HTTP. Rules:

- A call whose Target has no device ID (scan, MAC verifier, address probe) is always HTTP.
- Unpinned calls go over HTTP while the device's HTTP state is not "failing". A command that
  `cmdpolicy` classifies as a read follows `Paths.Commands`, any other command `Paths.Writes`.
- **Failover inside one call** from HTTP to MQTT happens only on `tasmota.NotSent(err)`: a refused
  connection (the dial failed), an address outside DEVICE_NETWORKS, a 401 before execution, or a device
  that refuses the HTTP API (SetOption128; for a *write* only when the failure memory already held that
  kind before the call, because the silence of a refusing handler can also follow a command that ran). A timeout, an unparsable answer or a connection dropped after
  the request was written prove nothing and are never followed by an MQTT send. Never MQTT to HTTP after a
  publish. Uploads never fail over.
- Every HTTP outcome goes into `Memory` (the failure kind and its start; a failure lasts 10 minutes and is
  cleared by the next answer, a health poll included), which is how `Facts.HTTP` becomes "failing".
- `transport.WithPath(ctx, HTTP|MQTT)` pins a job's transport: no failover, no rerouting. A command that starts
  an upgrade or a restore (`Upgrade <x>`, `WebGetConfig <x>`) is sent over MQTT only when the path is pinned
  to MQTT; the generic router never starts one on its own.
- The device is read once, the facts and the choice come from that snapshot, and the device is read again
  right before an MQTT send; a changed topic, group topic, FullTopic or MQTT capability sends nothing.
- Replies of credential-setting commands are scrubbed on the MQTT path (`cmdpolicy.ScrubReply`).
- `WithTrace(ctx)` records which transport carried the calls; the audit details of console commands,
  settings writes and Make compliant carry it as `"via":"http"|"mqtt"` (`"http+mqtt"` when a job used both).
- A write that can cut off a device reached over MQTT only (`cmdpolicy.CutOffName`: Topic, FullTopic, GroupTopic,
  Prefix<n>, Mqtt host/port/user/password/client/fingerprint, `SetOption3 0`, `SetOption103`, `SetOption132`) is
  published over MQTT only when the call carries `transport.WithCutOffConsent`, on the routed path and after a
  failover alike; otherwise `ErrCutOff` ("not sent: this device is reached over MQTT only; changing X can cut
  TasmoFleet off from it. Use the console, which asks to confirm"), nothing published, audit `via` "none". The
  console sets the consent on the confirmation that showed the consequence. Bulk commands are refused for such
  devices in the preview. Make compliant never sets it: `policy.Fixer.Apply` sends such lines last in chunks of their
  own, a refusal is reported as `SkippedError` ("applied N; skipped Topic, MqttHost: reached over MQTT only (use the
  console)"), the job succeeds for the rest and verifies only what was sent, and the preview lists them as skipped.
  The settings page reports the refusal per field.
- `DownloadConfig` and `RootPage` stay HTTP (the MQTT backup and identity come with their own work).

Background GroupTopic read (`internal/app/grouptopic.go`): shortly after start and after a device is
adopted, one `Status 1` per two seconds (through the router: HTTP while it works, else the MQTT channel) for
every device with a topic whose GroupTopic was never read or is older than its last probe; a device that
gave no answer is asked again after 10 s, 30 s, 2 min, then every 15 min, but at once when it comes online after a failed attempt. Offline devices are not asked. When no device needs a read the reader does not list the inventory for 30 s. The MQTT identity refresh reads
`Status 1` too and stores the GroupTopic.

### 9.1 Principles

- One job per device, grouped in batches. Jobs are persisted, and every transition is
  written to the database **before** its actions run (write-ahead).
- `Step(job, event, now, cfg) -> (job', actions)` is pure and total. The engine runs the
  actions and turns their results into events.
- Only idempotent commands are retried automatically (`Status N`, `OtaUrl <url>`, `GET /`).
  `Upgrade 1` is sent at most `MaxAttempts` (2) times per stage, and before a resend the
  engine always probes whether the first one took effect.
- Every wait has a deadline. Every failure has a reason string in `LastError` and a row in
  `ota_job_events`.
- HTTP only. MQTT `stat/<t>/UPGRADE` and firmware-server download events are hints that
  can only speed things up; nothing depends on them.

The same engine, batch and transaction pages also run the other multi-device operations as job
kinds (`model.JobKind`): `policy` (Make compliant), `command` (bulk Send command and Restart) and
`backup` (bulk Back up). They reuse persistence, per-device locking (`devlock`), cancel and "Retry
failed"; none of them touches firmware, so the Guard and the states below concern `update` jobs.

### 9.2 States

`queued`, `preflight`, `backup`, `set_ota_url`, `safeboot_switch` and `safeboot_recover` (push into
safeboot only, below), `upgrade_issued`, `await_reboot`, `verify`,
`restore_ota_url`, `restore_power`, terminal `succeeded`, `failed`, `cancelled`. `Stage` is `minimal` or
`final`; staged jobs pass through `set_ota_url -> ... -> verify` twice.

### 9.3 Transition table

| From                                   | Event                       | Guard / condition                                                                | To                                                                                                  | Actions                                                             |
| -------------------------------------- | --------------------------- | -------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------- |
| queued                                 | start                       | slot free (scheduler)                                                            | preflight                                                                                           | probe_identity                                                      |
| preflight                              | identity                    |                                                                                  | preflight                                                                                           | plan (record IdentityBefore, OriginalOtaURL if empty, UptimeBefore) |
| preflight                              | planned                     | decision skip                                                                    | succeeded (detail "already at target")                                                              | notify                                                              |
| preflight                              | planned                     | `SkipBackup`                                                                     | set_ota_url (stage = minimal if staged and not already minimal, else final)                         | set_ota_url                                                         |
| preflight                              | planned                     | otherwise                                                                        | backup                                                                                              | backup                                                              |
| preflight                              | guard_refused               |                                                                                  | failed                                                                                              | notify                                                              |
| preflight                              | command_failed / timeout    |                                                                                  | failed                                                                                              | notify                                                              |
| backup                                 | backup_done                 |                                                                                  | set_ota_url (stage as above)                                                                        | set_ota_url                                                         |
| backup                                 | backup_failed               |                                                                                  | failed                                                                                              | notify                                                              |
| set_ota_url                            | command_ok                  |                                                                                  | upgrade_issued, Attempt+1, deadline now+UpgradeAccept                                               | upgrade (guard re-run first)                                        |
| set_ota_url                            | command_failed / timeout    |                                                                                  | failed                                                                                              | notify                                                              |
| set_ota_url                            | guard_refused               | (engine re-runs Guard before OtaUrl)                                             | failed                                                                                              | notify                                                              |
| upgrade_issued                         | command_ok                  | reply echoes the URL                                                             | upgrade_issued                                                                                      | probe after ProbeInterval                                           |
| upgrade_issued                         | command_ok                  | URL differs                                                                      | failed ("OtaUrl changed by someone else")                                                           | notify                                                              |
| upgrade_issued                         | command_failed              |                                                                                  | upgrade_issued (unknown whether it started)                                                         | probe after ProbeInterval                                           |
| upgrade_issued                         | guard_refused               | Guard re-run before `Upgrade` refused; Upgrade not sent                          | failed                                                                                              | notify                                                              |
| upgrade_issued                         | probe_offline               |                                                                                  | await_reboot, SeenOffline, deadline now+RebootWindow                                                | probe after ProbeInterval                                           |
| upgrade_issued                         | probe_online                | uptime < UptimeBefore or version/image changed                                   | verify                                                                                              | probe_identity                                                      |
| upgrade_issued                         | probe_online                | unchanged                                                                        | upgrade_issued                                                                                      | probe after ProbeInterval                                           |
| upgrade_issued                         | upgrade_report "Failed ..." | Attempt < Max                                                                    | set_ota_url                                                                                         | set_ota_url                                                         |
| upgrade_issued                         | upgrade_report "Failed ..." | Attempt = Max                                                                    | failed                                                                                              | notify                                                              |
| upgrade_issued                         | timeout                     | Attempt < Max                                                                    | set_ota_url                                                                                         | set_ota_url                                                         |
| upgrade_issued                         | timeout                     | Attempt = Max                                                                    | failed ("device did not start the upgrade")                                                         | notify                                                              |
| await_reboot                           | probe_offline               |                                                                                  | await_reboot                                                                                        | probe after ProbeInterval                                           |
| await_reboot                           | probe_online                |                                                                                  | verify                                                                                              | probe_identity                                                      |
| await_reboot                           | timeout                     |                                                                                  | failed ("device did not come back")                                                                 | notify                                                              |
| verify                                 | identity                    | stage minimal, variant minimal                                                   | set_ota_url, stage final, Attempt 0 (or cancelled if CancelRequested)                               | set_ota_url                                                         |
| verify                                 | identity                    | stage final, version = target, variant/lang/family as planned                    | restore_ota_url, or succeeded if restore disabled / nothing to restore / CancelRequested            | restore_ota_url / notify                                            |
| verify                                 | identity                    | stage final but device on minimal/safeboot (Tasmota auto-continue, `ota_loader`) | await_reboot, deadline now+RebootWindow (once, recorded in `AutoContinueSeen`; then treat as "old") | probe after ProbeInterval                                           |
| verify                                 | identity                    | family or hardware changed                                                       | failed ("hardware changed", critical)                                                               | notify                                                              |
| verify                                 | identity                    | still old version (download failed, device kept running)                         | set_ota_url if Attempt < Max, else failed                                                           | set_ota_url / notify                                                |
| verify                                 | command_failed / timeout    |                                                                                  | await_reboot                                                                                        | probe after ProbeInterval                                           |
| restore_ota_url                        | command_ok                  |                                                                                  | succeeded                                                                                           | notify                                                              |
| restore_ota_url                        | command_failed / timeout    |                                                                                  | succeeded, LastError "could not restore OtaUrl"                                                     | notify                                                              |
| queued, preflight, backup, set_ota_url | cancel                      |                                                                                  | cancelled                                                                                           | notify                                                              |
| upgrade_issued, await_reboot, verify   | cancel                      |                                                                                  | same state, CancelRequested                                                                         | none (the device is flashing; finish the stage)                     |
| set_ota_url                            | command_ok                  | `ViaSafeboot`                                                                    | safeboot_switch, deadline now+SafebootWindow (Attempt unchanged)                                    | safeboot_switch (Guard, one `/u4?u4=fct`)                           |
| safeboot_switch                        | command_ok                  | device answered `false` or `true`                                                | same state                                                                                          | probe after ProbeInterval                                           |
| safeboot_switch                        | command_failed              | certain: the device did not switch (no factory partition, error status, cancel)  | failed ("device was not touched"), cancelled if CancelRequested                                     | notify                                                              |
| safeboot_switch                        | command_failed              | lost reply (uncertain)                                                           | same state                                                                                          | probe after ProbeInterval, never `/u4` again                        |
| safeboot_switch                        | probe_offline / probe_online| not answering as safeboot                                                        | same state                                                                                          | probe after ProbeInterval                                           |
| safeboot_switch                        | probe_online                | answers as safeboot                                                              | upgrade_issued (Attempt+1, UptimeBefore = safeboot's uptime, deadline now+UpgradeAccept)            | upgrade (Guard again, `/up` must be the plain form, one upload)     |
| safeboot_switch                        | probe_online                | answers as safeboot, CancelRequested                                             | safeboot_recover                                                                                    | safeboot_back                                                       |
| safeboot_switch                        | timeout                     |                                                                                  | safeboot_recover (reason "did not answer as safeboot")                                              | safeboot_back                                                       |
| upgrade_issued (ViaSafeboot)           | command_failed (refused) / guard_refused / timeout | the device is in safeboot                                         | safeboot_recover                                                                                    | safeboot_back                                                       |
| await_reboot (ViaSafeboot)             | timeout                     | never seen offline                                                               | safeboot_recover                                                                                    | safeboot_back                                                       |
| verify (ViaSafeboot)                   | identity                    | device is in safeboot                                                            | safeboot_recover                                                                                    | safeboot_back                                                       |
| any of the above (ViaSafeboot)         | (would enter safeboot_recover) | `RecoveryStep` != 0: a recovery ran already                                   | failed ("... a recovery already ran", state unknown)                                                | notify                                                              |
| safeboot_recover                       | probe_online                | answers from safeboot, `RecoveryStep` 1                                          | same state, `RecoveryStep` 2 (stored first), deadline now+SafebootWindow                            | safeboot_back (the one `/u4?u4=ota`)                                |
| safeboot_recover                       | command_ok / probe_offline / probe_online | still safeboot or unknown; lost reply; nothing sent yet            | same state                                                                                          | probe after ProbeInterval, never `/u4` again                        |
| safeboot_recover                       | probe_online                | app answers with the version from before (nothing is sent)                       | failed ("Upload into safeboot failed; the device was switched back to <old>"), cancelled if CancelRequested | notify                                                              |
| safeboot_recover                       | probe_online                | app answers with another version                                                 | verify                                                                                              | probe_identity                                                      |
| safeboot_recover                       | timeout                     | `RecoveryStep` 2                                                                 | failed ("Device is still in safeboot at <address>: ...")                                            | notify                                                              |
| safeboot_recover                       | timeout / command_failed (certain) | `RecoveryStep` 1, or the request was refused (HTTP error, 401)            | failed ("Could not bring the device back from safeboot (...), its state is unknown. Check it at <address>: ...") | notify                                                              |
| safeboot_switch, safeboot_recover      | cancel                      |                                                                                  | same state, CancelRequested (the recovery ends the job as cancelled)                                | none                                                                |
| any terminal                           | any                         |                                                                                  | unchanged                                                                                           | none                                                                |
| any other (state, event)               |                             |                                                                                  | unchanged                                                                                           | none (logged at debug)                                              |

Refinements (implemented in `machine.go`, tests per row):

- **Never send `Upgrade` into a running or imminent download.** A retry is allowed only
  with reboot proof (uptime < `UptimeBefore`) or after the deadline. Until then the engine
  keeps observing.
- `await_reboot | probe_offline` sets `SeenOffline`. `await_reboot | probe_online` without
  `SeenOffline` and with an old, intermediate or unknown image stays and probes: the
  minimal of `ota_loader` answers ~6 s before it continues, and Tasmota answers between
  download attempts. `await_reboot | timeout`: without `SeenOffline` -> retry
  (`set_ota_url` if Attempt < Max, else failed); with `SeenOffline` -> failed "device did
  not come back".
- `verify | identity` "still old" without reboot proof -> `await_reboot` with
  `SeenOffline=false`.
- Entering auto-continue sets `AutoContinueSeen` and clears `SeenOffline`.
- `Submit` is all-or-nothing: a `*RefusedError` (`errors.Is(err, ErrRefused)`) lists every
  refused device with its reason. `Cancel` on a terminal job -> `ErrNotActive`.
- Approved additions (WP4b): `Engine.Hint(deviceID, report)` feeds MQTT
  `stat/<t>/UPGRADE` reports; `Engine.CancelBatch(batchID)`.

**Power restore** (`restore_power`, after `restore_ota_url`; opt-out per batch with
`SkipPowerRestore`). **By default only light channels are restored** (a device whose
Status 11 has Dimmer/Color/CT; its channel is the last POWERn). Plain relays are restored
only with the per-batch opt-in `RestoreRelays` (default off, warned in the UI). A relay
or light that is ON in **any** settled post-boot reading counts as kept and is never
switched. Zero or missing input never starts a restore (`TestStepNoRestoreOnZeroOrMissingInput`).

- Relay states are read three times: in preflight (Status 11), right before the first
  `Upgrade`, and after the final boot. Only relays that were ON in both pre-update
  readings are candidates. A non-retained MQTT `POWER OFF` before the reboot removes a
  candidate.
- The post-boot reading must be settled: two equal readings one probe interval apart,
  at most 6 tries. If any candidate is still ON, the device kept its state and nothing
  is switched. Otherwise the missing relays are switched ON once (`Power<n> ON` via
  devicecmd, audited as "tasmofleet (OTA restore for job N, requested by X)") and
  verified once.
- **The engine never switches a relay OFF** and never touches relays the user
  switched. It fails closed on any uncertainty: unreadable or ambiguous readings,
  partially kept state, relays with PulseTime/Interlock/shutter roles or above 8, a MAC
  mismatch after the boot, or readings older than `OTA_POWER_MAX_AGE` (default 20 min).
  Then the job still succeeds, and `LastError` notes why nothing was restored.
- Lights restore power only, not dimmer/colour/CT. Observations behind this design
  (relays keep state through minimal; lights come up ON then go OFF; ESP32 keeps state
  only with SetOption0 1) are in `docs/research/tasmota-minimal-and-ota.md`.

Resume (process start): non-terminal jobs get `resume`.

| State                        | On resume                                                 |
| ---------------------------- | --------------------------------------------------------- |
| queued                       | wait for scheduler                                        |
| preflight, verify            | probe_identity                                            |
| backup                       | backup                                                    |
| set_ota_url                  | set_ota_url (idempotent)                                  |
| upgrade_issued, await_reboot | **probe**, deadline = now + RebootWindow. Never `Upgrade` |
| safeboot_switch, safeboot_recover | **probe**, deadline = now + SafebootWindow. Never `/u4` |
| restore_ota_url              | restore_ota_url                                           |

### 9.4 Staged ESP8266 flow and the OtaUrl rule

SRC facts: normal firmware persists OtaUrl when `Upgrade` starts. Minimal never persists.
TasmoAdmin's flow therefore left every device with the **minimal URL** stored. tasmofleet:

1. preflight records `OriginalOtaURL` (normal firmware, from `OtaUrl` without parameter).
2. minimal stage: `OtaUrl <fw>/<rel>/tasmota-minimal.bin.gz`, `Upgrade 1`. Stored = minimal URL.
3. final stage on minimal: `OtaUrl <fw>/<rel>/tasmota-<v>.bin.gz` (RAM only), `Upgrade 1`.
4. full firmware boots with stored OtaUrl = minimal URL. `restore_ota_url` sets
   `OriginalOtaURL` back (normal firmware, persisted). `OTA_RESTORE_OTAURL=false` leaves the
   tasmofleet URL instead. This is a deliberate choice with a flash write either way.

Direct flows also restore. If `OriginalOtaURL` was empty or already equal, nothing is
written.

### 9.5 Timeouts and limits (defaults, configurable)

| Name           | Default                          | Reason                                                                                       |
| -------------- | -------------------------------- | -------------------------------------------------------------------------------------------- |
| CommandTimeout | 30 s                             | per command incl. client retries                                                             |
| UpgradeAccept  | 3 min                            | download blocks the web server, so "no answer" is expected; OBS end-to-end 20-40 s           |
| RebootWindow   | 8 min                            | 10 download attempts on ESP8266 (SRC) plus boot plus Wi-Fi join                              |
| ProbeInterval  | 5 s                              |                                                                                              |
| SafebootWindow | 2 min                            | ESP32 restart into the other partition plus Wi-Fi join; measured about 6 s (to safeboot) and 11 s (back to the app) |
| MaxAttempts    | 2 per stage                      |                                                                                              |
| OTA_PARALLEL   | 2 jobs past `queued`, fleet-wide | ESP8266 web servers and a small firmware server; staged jobs hold their slot for both stages |

### 9.6 Concurrency and scheduling

One engine goroutine owns scheduling. Each active job has a worker goroutine that executes
the actions of one transition at a time. The per-device HTTP serialisation in `tasmota`
also covers health polls running at the same time. Batches start FIFO by job id. A batch
can be cancelled as a whole. `Submit` refuses devices with `OTAExcluded`, devices that
already have an active job, and devices whose Preview decision is `skip` or `refuse`
(returned to the caller, not silently dropped).

### 9.7 Preview (dry run)

`Preview` runs read-only probes, Detect, Decide, Select, Plan and Guard (against the URL
the artifact would get) and returns a per-device row: upgrade / reinstall / downgrade /
skip / refuse with reason, plan (direct/staged), file name. The UI always shows it before
Submit.

---

## 10. Health model (package `health`)

| State       | Rule                                                                                                                         |
| ----------- | ---------------------------------------------------------------------------------------------------------------------------- |
| online      | MQTT device: LWT `Online` and any message within `StaleAfter`. HTTP-only device: last poll succeeded                         |
| stale       | MQTT device: LWT `Online` but silent longer than `StaleAfter` (default 2 x TelePeriod + 60 s = 660 s). Triggers an HTTP poll |
| offline     | LWT `Offline`, or `OfflineAfter` (3) consecutive failed polls                                                                |
| auth_failed | the device answers but refuses our credentials (401 / `WARNING Need user=`). Never counted as online (TasmoAdmin lesson B7)  |
| unknown     | nothing observed since start                                                                                                 |

- The HTTP poll uses `Status 11` (cheap; uptime, Wi-Fi, power) and falls back to the `Upgrade`
  probe when the reply is Unknown (minimal firmware, shown as "minimal" in the UI).
- Poll scheduling: HTTP-only devices every `POLL_INTERVAL` (5 min) with ±20 % jitter. MQTT
  devices only when stale. Offline devices back off exponentially up to 30 min. At most 4
  polls in flight.
- MQTT messages update `LastSeen`, power state and RSSI immediately and push SSE events.
- A software restart publishes LWT `Offline` about 2 s after the command and before every OTA reboot
  (Tasmota's PubSubClient closes the socket without a DISCONNECT packet, so the broker fires the will;
  `docs/research/tasmota-mqtt-transport.md` section 5, live 2026-10-30). Health does not try to
  detect reboots; the OTA engine uses uptime.
- Only transitions are persisted (`health_events`), plus `devices.health_state/last_seen_at`
  for a correct picture right after a restart. A device's first observation after the app
  started (state "unknown" to anything) is a baseline, not a change: `devices.health_state` is
  written, no `health_events` row (decision 2026-10-23: a restart logged "Unknown to Online"
  for every device). The cost: a change that happened while the app was down is not recorded.
- MQTT input is untrusted and bounded: payload cap, bounded topic grammar, a retained
  replay cache with per-device reservations for inventory devices and a small evicting pool
  for unknown topics, and a fail-closed known-device index. Authoritative:
  `docs/security/mqtt-ingest.md` (attacker model, budgets, claims Q1-Q10, residual risks,
  broker ACL recommendation).
- No broker configured -> the tracker runs HTTP polling for every device. The app
  starts and runs without MQTT.

---

## 11. Inventory, import and discovery

### 11.1 Inventory (M1)

Add/edit/delete by admin. Add needs address (+ port, credentials) and runs a probe
(`Status 2/4/5/6/11`) to fill MAC, topic, name, identity and MQTT capability. The
address must be inside `DEVICE_NETWORKS`.

**Identity rule (2026-10-20).** A device is identified by its MAC addresses.
Matching in discovery, MQTT announcements, import and probes is by MAC first; address,
hostname and topic are mutable attributes. A device stores a set of MACs (Wi-Fi station
plus Ethernet where present: ESP32 boards with Ethernet report one MAC per interface).
The MAC set changes only through an explicit action ("Replace device"), with one exception:
a device stored without any MAC gets the MAC it reports bound by a person's "Refresh identity"
(5, Refresh identity; #65). A MAC is not a
secret: it identifies a device, it does not authenticate it. Consequences: a discovered
device whose MAC is in the inventory is the same device, never a new one; a stored
hostname is never replaced by an IP automatically (a hostname that resolves to the
discovered IP is a no-op); a real address change is offered as "Known device – address
changed" (update by an admin, or dismiss), not applied silently. Device credentials
exist only when a password is set (a web user without a password means no auth).
`OTAExcluded` replaces TasmoAdmin's "updatable" flag. Tags are free text and select
devices in batches.

### 11.2 TasmoAdmin import (M1: CLI; M2: web upload)

`tasmofleet import tasmoadmin <dir>` reads `devices.csv` (fputcsv with `\` escape, 7-16
columns), `MyConfig.json`, and optionally `.device-password.key` (AES-256-GCM `enc:v1:`
passwords; layout `iv(12) || tag(16) || ciphertext`). It maps columns 0-4, 10 (updatable ->
OTAExcluded), 11 (port), 12, 14 (topic). Dashboard columns are dropped. Every row is then
probed (MAC). Duplicate MACs are merged and reported. Rows without an address are
reported. `--dry-run` prints the plan. The local login hash is not imported (local
accounts are created fresh). MyConfig `ota_server_ip/port` is printed as a hint for
`FIRMWARE_BASE_URL`.

### 11.3 Discovery (M2)

- **MQTT discovery**: a continuous subscription to retained `tasmota/discovery/+/config`
  (fields `ip, dn, fn, hn, mac, md, sw, t, ft, ...`, OBS) and, since 0.13.0, to the device
  topics themselves (11.3a). Unknown MACs become proposals.
  A known MAC (any MAC of the device's set) is the same device. At the same place (stored IP,
  or a stored hostname that resolves to the discovered IP) nothing happens, not even an audit
  row. A changed IP of a device without a password is verified by MAC and updated (fixes DHCP
  drift). A stored hostname is never replaced by an IP, and a device with a password is never
  moved by discovery: both become an "address changed" suggestion that an admin applies or
  dismisses. Refused attempts are audited at most once per device and day. Dismissed
  proposals can be listed ("Show ignored") and restored.
- **HTTP autoscan**: an admin starts a scan of CIDRs, each of which must lie inside
  `DEVICE_NETWORKS` (at most 1024 addresses per scan). It probes `Status 2` with 16
  concurrent requests, a 2 s timeout and 1 retry, and streams results over SSE. It runs as a
  cancellable background job. Loopback, link-local, multicast and broadcast addresses are
  always skipped.
- Proposals are added only by explicit admin action.

#### 11.3a MQTT-first discovery (0.13.0, issues #47 and #45, decided 2026-10-26)

**Why.** Until 0.12 MQTT discovery read only the retained `tasmota/discovery/<MAC>/config`.
Tasmota publishes it only with `SetOption19 0` and only in builds with
`USE_TASMOTA_DISCOVERY`; `SetOption19 1` removes it, and self-compiled builds often leave
the driver out. Such devices are on the broker (retained LWT, STATE, INFO) but were never
proposed (reporter: 40 devices on the broker, 24 in TasmoFleet). Second, "Add…" on a
discovered device opened the manual form, which probes over HTTP first: an offline device
(or one that refuses the probe) could only be added "without probing", and then without
the MAC and topic that discovery had seen, so MQTT could never match it afterwards.

**Sources.** With a broker, MQTT is the primary discovery source. HTTP discovery (scan)
stays: it is the only way to find devices without MQTT and freshly flashed ones that have
no broker yet. MQTT discovery listens to (decoded with the configured FullTopic, default
`%prefix%/%topic%/`):

- retained `tasmota/discovery/<MAC>/config`: a full claim (MAC, IP, name, topic), as before;
- `tele/<topic>/LWT` (retained), `tele/<topic>/STATE`, `tele/<topic>/INFO1..3`: the topic
  exists and whether it is online. They carry no MAC, so they cannot become a proposal alone;
- `stat/<topic>/STATUS5`, only for a topic discovery itself asked (below): MAC (Wi-Fi, plus the
  Ethernet MAC if any), IP (the Ethernet one when Wi-Fi has none) and hostname.

**On-demand Status 5.** A topic that is online (LWT `Online`, or a live STATE/INFO) and that
neither an inventory device (by topic) nor a discovery config (by `t`) accounts for gets one
`Status 5` over MQTT (`cmnd/<topic>/STATUS` payload `5`). Never `Status 0` (safety rule 2),
never on a schedule: only while the topic is unresolved, at most once per topic per 10
minutes, one command at a time and at least 1 s apart for the whole bus, never to an
offline topic, and only after the topic has been online for 30 s (Tasmota publishes its
discovery config a few seconds after the LWT; a device that announces itself is not asked).
Each query is logged and audited (`discovery.identify`, topic and reason), and switching
MQTT discovery off (`DISCOVERY_MQTT`) stops them. Reason: Status 5 is the smallest reply that carries the MAC, and the MAC is
the identity (11.1); a device answers it as cheaply as a STATE.

**Unresolved topics.** Topics without a MAC yet (offline, no answer, address outside
`DEVICE_NETWORKS`) are kept in memory (at most 1024, oldest dropped) and listed on the
Discovered tab under "Seen on MQTT, not identified yet" with the reason, so "on the
broker but not in the list" is visible instead of silent. They cannot be added: there is no
MAC to bind. A claim refused only because its address lies outside `DEVICE_NETWORKS`
(discovery config or Status 5 answer) is kept with its topic and evaluated again as soon as
the device networks change (settings page, live): Tasmota repeats neither on its own, so
without this the device would stay listed until a restart (#45). The re-evaluation sends no
MQTT command. A kept claim older than 1 hour is not used (its address may be stale); the
topic is asked again with Status 5 instead. A newer stored sighting of the same MAC wins
over a kept claim, and a topic that becomes an inventory device drops its claim. After the
device networks shrink, proposals outside them stay listed until they expire (30 days) but
cannot be accepted.

**One device, two sources.** Both sources resolve to one proposal by MAC (the proposal row
is unique per MAC). For a device seen on MQTT the topic, full topic and online state come
from MQTT: a later scan sighting of the same MAC keeps them (and `via` stays `mqtt`) and
only contributes the HTTP port it found; an MQTT sighting keeps the port a scan found. The
online state shown with a proposal is the topic's last LWT.

**Adding without a probe.** Each discovered row has a direct "Add" that accepts the
proposal as seen (MAC, topic, full topic, address, port; `discovery.Accept`). It does not
depend on an HTTP probe: for a proposal whose LWT is `Offline` the MAC check is skipped (it
could only time out); otherwise an unanswered check is fine as before. With the fleet
default password the MAC must still be proven (security review 2026-10-22), so an offline
device is then added without it (audited as `default_password_withheld`); no first probe is
started while it is offline. "Add…" (the prefilled form) stays for typing a password.

**Filled in later.** A device added this way has no identity yet. When the topic of any
inventory device without a detected identity comes online (LWT `Online`, or a live STATE),
discovery starts the first probe again (the same hook as after an accept), at most once per
device per 10 minutes and not again right after an accept already started it. The trigger
is an MQTT message anyone can publish and the probe carries the stored credentials, so
the stored address is first asked without credentials (D15): another MAC refuses the
probe; for a device with a stored MAC and a stored password, an address that cannot prove
the MAC does not get the password on this trigger (retried every 10 minutes); without a
password the probe runs. A device without a stored MAC (for example a TasmoAdmin import
whose first probe failed) is probed at the address the admin bound with its password; the
probe never binds a MAC, the audit records the MAC that answered. Queries and probes
pause while the broker is unreachable. Topic and MAC of a stored device are never changed
from MQTT (D2).

No migration, no store contract change and no new dependency.

---

## 12. Backups (package `backup`)

### 12.1 What is stored per backup run

1. **Raw `.dmp`** from `GET /dl` (Basic auth, correct port). This is authoritative and the
   only input for restore.
2. **Snapshot JSON** built from `Status 1..11` (one by one, never Status 0) plus `Template`,
   `Module`, `Rule1..3`, `Timers`. Read-only reference.
3. **Decoded JSON** from `decode-config` when `DECODER_PATH` is set and the decoder
   version ≥ the device firmware version. It is stamped with the decoder version. If the
   device is newer, only 1+2 are stored and the backup is flagged "decoder outdated".

Files are `0600`, written via temp file + rename, and SHA-256 indexed.

Over MQTT (0.20.0, #257) the raw `.dmp` comes from `FileDownload {"Password":…,"Type":2}` instead of
`/dl` and must be byte-identical. `mqttcmd.Download` follows the live-tested protocol: `FileDownload 0`
first when the state of the topic is unknown (process start or after a bad transfer), then the start
command, answered by `Started` and the header `{File,Id,Type,Size}` (no `?` after `Started`), then exactly
one `?` per Data message until `{Id,Md5}`. Id must stay constant, Size is 2..16 KiB, the received bytes
must equal Size and hash to the MD5; an empty Data, a timeout (8 s per message, 30 s overall) or any
other irregularity ends with `FileDownload 0` and an error, with no retry. `Error 1` is the typed
`ErrPasswordRefused`. Messages are never logged and never put into errors: the start command carries the
password and a minimal firmware echoes it back as `{"Command":"Unknown"}`. The password per device is
set on the edit form (admin; mode not set / broker / own; write-only, blank keeps, saving clears the
refused mark) and stored sealed (D6).

### 12.2 decode-config integration

- License LGPL-3.0. Shipped in the image as the unmodified upstream PyInstaller binary
  (v16.0.0, x86_64, SHA-256 pinned in the Dockerfile) and executed as a separate process,
  never linked. Documented in `NOTICE`.
- Decision: ship it in the image (+25 MB plus libz) instead of a Python layer (+50-100 MB).
  `--build-arg WITH_DECODER=false` builds a slim image. On arm64 there is no upstream
  binary, so the decoder is off.
- Invocation: `decode-config --source <tmpfile.dmp> --output-format json` (exact flags are
  pinned by a test against the real binary in integration tests; CI installs the pinned binary
  and sets `REQUIRE_DECODER=1`, so these tests cannot silently skip). Timeout 20 s, `TMPDIR=/data/tmp`,
  empty environment except `TMPDIR`/`PATH`, no network needed.
- `header.env` (platform, kernel and Python of the decoding host) is removed before the
  decoded JSON is stored.
- `Decoder.Version()` parses `decode-config v16.0.0.0 [...]`. It is shown on `/about` and in
  `tasmofleet_build_info{decoder=...}`.

### 12.3 Secrets in backups

`.dmp` and decoded JSON contain Wi-Fi passwords, MQTT password and WebPassword. So:
backup files are 0600, downloads need the admin role and are audited, and the UI never
shows the decoded secrets. The diff/drift view redacts keys matching
`(?i)password|passwd|ssid.*pass|token|secret|key`. Backups do not go into logs or metrics.

### 12.4 Schedules, retention, restore

- Manual (device page, Backups tab, bulk "Back up") and automatic before every OTA (job state `backup`, skippable
  per batch).
- Manual backups have a cooldown of 1 minute per device (HTTP 429 "Try again in ..."); deleting the last backup of a device ends it.
  Every backup reads the device config over its single-threaded web server, so repeated
  clicks must not hammer an ESP8266. Pre-OTA and scheduled backups are not limited. Optional periodic schedule `BACKUP_INTERVAL` (default off).
- Retention (`backup.Prunable`, the one rule pruning and the "Pruned at next run" label share): per
  device the newest `BACKUP_KEEP` (10) runs stay, plus every run younger than `BACKUP_MAX_AGE` (30 days,
  `720h`, so repeated manual runs cannot evict the restore points), plus runs pinned with Keep (an
  operator action; kept runs do not count against `BACKUP_KEEP`). Older runs are deleted after a
  successful new backup, never otherwise. A run is the set of files of one backup. A backup that
  failed stores nothing; the attempt is listed as a "Failed" row with Retry (`backup_attempts`).
- M2 diff view: decoded JSON of two backups, redacted, rendered side by side ("what changed
  since the last backup"). Operators see which keys changed with the values hidden; admins see the redacted values.
- M2 restore: admin picks a backup. tasmofleet creates a one-time token (random 128 bit,
  valid 5 min, bound to the device IP) and sends `WebGetConfig http://<fw>/restore/<token>`.
  The firmware listener serves the `.dmp` once to that IP only. Audited. This avoids
  TasmoAdmin's authenticated-route problem (lesson B8).

---

## 13. Device page, console and settings (M2)

- **Device list with live state**: health, version, variant, family, IP, uptime, RSSI, power
  state(s) with a toggle (`Power<n> TOGGLE`, operator role), filters by tag, health and
  family. Updated via SSE, no browser polling.
- **Console**: sends one command line to one device and shows the JSON reply. `cmdpolicy.Authorize`
  classifies every line: queries and harmless actions (`Power`, `Restart 1`, `Status N`, the example
  list `cmdpolicy.DefaultOperator`) need the operator role, everything else the admin role (admins
  may send any Tasmota command, decision 2026-10-21), a malformed line is never sent. Destructive
  or lock-out commands (`Reset`, `WifiConfig`, firmware commands, ...) get exactly one confirmation
  that names the consequence; arguments of credential commands are never logged or audited.
  Every command is audited, with the transport that carried it (`via`). Replies are shown as escaped text.
  `FileDownload`, `FileUpload` and `Upload` are refused for every role (their payload carries the device's
  MQTT password). On a device tasmofleet reaches over MQTT only, a change of Topic, FullTopic, GroupTopic,
  MqttHost/Port/User/Password/Client or `SetOption3 0` gets the confirmation with the consequence
  "this device is reached over MQTT only; this change can cut TasmoFleet off from it". Over MQTT the commands
  that run other commands (see 6.5) are refused. Claims: docs/security/command-policy.md.
- **Settings editing** (admin, device Settings tab): typed forms for a curated set (FriendlyName1..4,
  Hostname, Topic, FullTopic, TelePeriod, PowerOnState, SaveData, SetOption0/3/4/19 shown with names,
  MQTT host/port/user/password, NtpServer1..3, Timezone/TimeDst/TimeStd). Every field has a validator. Values are never
  concatenated into `Backlog` strings: each field becomes its own command, and `;` in a
  value is rejected (TasmoAdmin lesson B3). Secret fields (passwords) are write-only.
- SetOption numbers are always displayed with their internal name and a one-line
  description (e.g. "SetOption0 (`save_state`): restore relay state after power loss").

---

## 14. Fleet operations (M2)

### 14.1 Bulk commands

The device list's selection bar offers "Update", "Back up", "Send command", "Restart" (admins) and
power (operators; `POST /devices/bulk/power`). "Send command" is admin only: one command line,
a `Backlog` included, goes to every selected device. The command passes the same `cmdpolicy`
verdict, confirmation texts and redaction as the console, and the preview is the confirmation: a
token bound to the command line and the device list. The send runs as a `command` transaction (per-device
result, "Retry failed", audit). "Devices at a time" is 1, 3 (default) or 5. There is no configurable
allowlist: `bulk.DefaultAllowlist` is a leftover that nothing uses (design change; the policy of
the console decides).

### 14.2 Fleet settings templates

Planned as named command sets applied to tagged devices. Not built: the policy rules (§14.4) and
"Make compliant" cover the use case, and the policy editor offers rule templates ("Use template").

### 14.3 Restart

`Restart 1` as a bulk action ("Restart" in the selection bar, admin) with a confirmation page, run
as a `command` transaction with the same "Devices at a time" choice. A single device has its own
Restart (device page).

### 14.4 Policy and drift view

Declared rules (`policy.Rule`) with exceptions per device, tag or family. Values are read
from decoded backups (preferred) or live `Status` replies. The view lists drift per device.
Fixing it is an explicit bulk action. Example rules: PowerOnState 0,
SetOption0 0 (ESP32 exception: 1), SaveData 0, Timezone 99.

A rule's command is free text with the curated settings as suggestions: any Tasmota command that
answers a plain query (`Sleep`, `LedState`, ...) can be a rule. It is read live by its bare name,
one command at a time, and set with `<Command> <want>`. Never allowed: `Status` in any spelling
(Status 0 is never sent automatically), secrets, commands that act when sent bare, and commands
whose set form needs a console confirmation. The checks are `cmdpolicy.PolicyReadable` and
`cmdpolicy.PolicySetText`; the claims are in docs/security/command-policy.md (G1 to G6). A
generic rule has no value validator, is not read from backups and never restarts the device.
`Sleep`, `SerialLog` and `SysLog` reply nested (`{"Sleep":{"50":{"Active":"100"}}}`); the rule
compares the configured value (the object key), not the active one.
`Latitude` and `Longitude` are stored by the device as float32 micro-degrees and read back with float32
noise, so they are compared at four decimals (11 m): `settings.CanonicalWant` reduces a rule's `want`
to that form in `policy.ParseRules` (a non-number or an out-of-range value is an invalid want), and
`policy.Rule.Want` carries the canonical form, which is what the check compares and Make compliant sends.
The stored `RuleSpec.Want` keeps what the admin typed. Every other command's `want` is used as typed.

**Rule ids.** Each rule has a stable `id` (`^r[a-z0-9]{1,15}$`, optional in the JSON format: `policy.Rule.ID`,
`sysconfig.RuleSpec.ID`) that survives edits, reordering and export/import, so a link names a rule and
not a slot. New rules get `r` plus 10 random base32 characters; ids are unique within a list (an import
with a duplicate or malformed id is refused). A list without ids (every version stored before 0.19, and
`POLICY_RULES` files without ids) gets `r1`...`rN` by position (`policy.LegacyID`) when it is read; stored
rows are never rewritten, the next save persists the ids. Opening and saving such a version unedited is
no change. Ids that were `r1`...`rN` at the upgrade keep that form for good and count as positions in the
version diff: it matches rules by id only for real (non-position) ids and by name otherwise, so a rename
of such a rule shows as removed plus added.

One evaluation: `policy.Outcomes` judges every enabled rule against every device in its scope and
returns one outcome per pair: Match, Deviate, Unknown, Shadowed (a higher rule on the same command
is the one evaluated; the outcome names it) or Excepted (the device is exempt; an excepted rule does
not claim the command). `policy.Check` is the Deviate and Unknown outcomes as findings, so Make
compliant and the page counts cannot disagree. `policy.Tally` and `policy.FleetTally` derive the
per-rule and fleet numbers: a device is Deviating with at least one Deviate, Not judged with an
Unknown and no Deviate, Compliant with no Required rule deviating or unknown. `PolicyService.Evaluate`
judges a draft rule list from cached values (no device call). `PolicyService.ReadValue` reads one
setting of one device to start a rule from it: a command refused by `policy.ReadProblem` or
`settings.Queryable` is never sent; a live read falls back to the newest decoded backup only for
commands a backup holds (`policy.BackupReadable`); a value with credentials or one that cannot be an
expected value is refused; the audit entry `policy.copy` names device and command, never the value.

**Overview (`/policy`).** Five tiles: Rules (enabled and disabled), Compliant devices, Deviating
(required and advisory), Not judged (all `policy.FleetTally`) and the live-check tile (state, last
run, Run now, Pause or Resume for admins, Retry now after a failure). Tabs follow (Rules, Deviating devices, History; History only with managed rules, not with `POLICY_RULES`;
Import or export has its own page, `/policy/io`), then the rule table in stored order: a drag handle (admins), name (a link to
`/policy/rules/{id}`, Disabled chip), setting, applies to with the device count, Required or Advisory, a
compliance bar from `policy.Tally` read by rule id, and the row menu (Edit, Duplicate, Enable or Disable,
Move up, Move down, Delete). A disabled rule is dimmed and is not checked. A banner names the devices that
break a Required rule, with Show and Make compliant. Every list operation names its rule by id and the
version the person saw (stale: 409); the drag posts positions plus the version. Unknown query
parameters are ignored: the old `?edit=`, `?new=` and `?v=` no longer open an editor or a diff.
The empty page offers New rule and the first templates (admins), or says an admin can add rules.

**Rule editor** (`/policy/rules/new`, `/policy/rules/{id}`, admins; operators get the read-only rule page).
"Cancel" and "Save rule" both leave the editor for the overview: Cancel changes nothing, Save shows
the saved rule's name and marks its row (`?ok=saved&rule=<id>`). Duplicate (row menu) opens the editor with
an unsaved copy (`/policy/rules/duplicate`); nothing is stored until Save rule, and the copy lands right
below its source. "Copy from a device" (`/policy/rules/new/copy`) starts a draft from one device's value
the same way. Saving checks the version the editor was opened on (409 when someone else saved first).
The compliance column of the table has a fixed width of 230 px (bar and numbers).

The Deviating devices tab (`GET /policy/deviations`, `?show=required|advisory|unknown`, `?q=`) lists every
deviation grouped by device. Admins get a box per row read live (a backup-only value has none): Required rows
start ticked, Advisory rows never do. The boxes post `pick=<deviceID>:<ruleID>` to `/policy/fix`: with only
`pick` the plan is exactly the picked deviations. With `only`, `all` or `device` (device tile, overview banner) the
plan is every Required deviation of those devices, and an Advisory deviation joins only when its pick is
posted; the preview offers each as an unticked box ("Update plan"), the dialog a "Choose Advisory rows…"
button. A pick for a pair that is not a finding of the report, or of a device outside the selection, is dropped, and
the apply digest covers the picks. Devices that cannot be judged sit in a fold: those with an unknown value a
backup decodes (`policy.BackupReadable`) get "Back up these N" (POST `/devices/bulk/backup`, admin), the others a
hint and Run now.

### 14.5 Notifications

Planned, **not built**: the `notify.Notifier` interface exists (webhook and Resend mail were meant as
implementations; triggers batch finished with failures, device offline longer than X, backup failed),
but nothing in `app` creates or calls a notifier. Transactions, the audit log and `/metrics` are the
ways to see failures today.

---

## 15. Authentication, authorisation and security

### 15.1 Modes

tasmofleet runs in three deployment shapes, all first-class and covered by e2e tests:
(a) behind a reverse proxy with an IdP (e.g. Traefik + Authentik), (b) behind a
forward-auth proxy, and (c) **standalone**: a plain Docker host on a LAN with no proxy
and no IdP. `AUTH_MODE` selects the primary method. Local accounts (`LOCAL_LOGIN`,
default on) can be combined with every mode and are the only method in `local` mode.

**OIDC (`AUTH_MODE=oidc`; the default mode is `local`)**

- Discovery from `OIDC_ISSUER` at start, retried with backoff. Readiness reports not ready
  until it succeeds.
- Authorization code flow with PKCE S256, `state` (32 random bytes) and `nonce`, kept in a
  short-lived (10 min) HttpOnly cookie bound to the login attempt.
- `redirect_uri = PUBLIC_URL + BASE_PATH + "/oidc/callback"`. It is never derived from
  request headers. `PUBLIC_URL` is required in this mode.
- The ID token is verified by go-oidc (issuer, audience, expiry, nonce, signature, JWKS
  rotation). Claims: `sub`, `OIDC_USERNAME_CLAIM` (`preferred_username`), `email`, `name`,
  `OIDC_GROUPS_CLAIM` (`groups`, a list of names in Authentik).
- Roles from groups (`ADMIN_GROUPS`, `OPERATOR_GROUPS`), highest wins. At
  least one of these lists must be configured; there is **no implicit "any authenticated
  user"** (config validation fails). No matching group: 403 with a clear page, no session.
- **Group revocation.** The refresh token is stored server-side with the session, sealed
  with secretbox. On a request at least `OIDC_RECHECK` (15 min) after the last check, the
  session refreshes at the IdP and re-evaluates the groups. A role change applies at once.
  Losing all mapped groups, or a failed refresh, ends the session. Defaults: idle timeout
  `SESSION_IDLE` 2 h, absolute `SESSION_MAX` 12 h. The role is also recomputed on every
  request from the stored groups and the current mapping, so a config change applies
  immediately.
- Logout clears the session, then does an RP-initiated logout: redirect to
  `end_session_endpoint` with `id_token_hint` and `post_logout_redirect_uri`
  (`OIDC_POST_LOGOUT_URL`, default `PUBLIC_URL + BASE_PATH + "/"`).
- Post-login redirect targets are local paths only (must start with `/`, not `//` or `/\`,
  no scheme).

**Trusted header (`AUTH_MODE=header`)**, e.g. Authentik outpost forward auth in Traefik:

- Headers `HEADER_USER` (`X-authentik-username`), `HEADER_GROUPS` (`X-authentik-groups`,
  separator `|`), `HEADER_EMAIL`, `HEADER_NAME`. The same group-to-role mapping applies and
  is equally mandatory.
- Accepted only if `RemoteAddr` is in `TRUSTED_PROXIES`. `X-Forwarded-For` is never used
  for this decision. From an untrusted peer the headers are ignored.
- No session; every request is authenticated by its headers. Logout redirects to
  `HEADER_LOGOUT_URL`. The CSRF token is an HMAC of the user, keyed with a persisted key
  (`<DATA_DIR>/csrf.key`, 0600, 32 random bytes), so forms survive restarts.
- Sessions created in another mode are rejected after a mode switch.

**Local accounts (`AUTH_MODE=local`, or alongside oidc/header with `LOCAL_LOGIN=true`)**

- Multiple users, each with role `operator` or `admin`, and a
  `disabled` flag. Login on the one sign-in page `/login` (form POST to `/login/local`).
- **One login page** (decision 2026-10-19). `/login` shows, side by side, a button
  "Sign in with `<OIDC_DISPLAY_NAME>`" (when OIDC is configured; the name is a sign-in
  setting, default "Single sign-on", works for any provider) and the username/password
  form (when local login is on and a local account exists). The browser is redirected to the
  IdP automatically only when OIDC is the only method. The button is `GET /login/sso`;
  `GET /login/local` redirects to `/login`. Header mode is unchanged (the page appears
  only when the proxy did not identify the visitor and a local account exists).
- **Break-glass** is a local admin on this page plus `tasmofleet user passwd` on the
  host (runbook `docs/runbooks/emergency-access.md`). The direct admin listener
  (`ADMIN_LISTEN`, 2026-10-18) was removed on 2026-10-19; there is no protection beyond
  the login rate limiting.
- Management:
  - CLI: `tasmofleet user add|passwd|role|disable|enable|delete|list` (password from stdin
    or a TTY prompt, never as an argument).
  - Web UI (`/users`), **admins only**. The list of all principals (local accounts plus the
    SSO and proxy users seen at sign-in: name, method, role, last login, live sessions)
    needs no step-up. Every change needs a step-up valid for 5 min: local admins re-enter
    the password, OIDC admins re-authenticate at the IdP (`POST /oidc/stepup`). Changes:
    create users, set roles, disable, reset passwords, delete; for SSO users only "End
    sessions" (their roles come from the IdP groups). An operator can never create users
    or raise roles. Users change their own password only with the current one. Any
    password change or disable revokes that user's sessions.
  - Header principals have no session and nothing to confirm with: the page is view only
    for them.
- First run: if no local user exists and local login is on, an `admin` account is created when
  `AUTH_MODE=local` or `INITIAL_ADMIN_PASSWORD` is set (an OIDC install is not seeded with a surprise
  account). Without `INITIAL_ADMIN_PASSWORD` a random password is generated and written to
  `<DATA_DIR>/initial-admin-password` (0600, created exclusively). It is **never logged**; the log
  names the file and prints a one-time **setup link** (`/setup/account`, valid until the account has a
  password of its own) with which the admin sets the password. Signing in with the file's password
  works too. The account has `must_change_password`; the first login forces a change before any other
  page. The file is deleted after the change, and at the next start. The first-run wizard (`/setup`)
  follows: device networks, optional MQTT, optional SSO, first scan.
- Account invariants, attackers and tests: `docs/security/account-lifecycle.md`
  (password generations revoke sessions and device cookies atomically; actor-checked
  mutations; last-admin guard).
- Password hashing: PBKDF2-HMAC-SHA256, 600 000 iterations, 16-byte salt, PHC-like string
  `$pbkdf2-sha256$i=600000$<salt>$<hash>`, constant-time compare. Decision: PBKDF2 over
  argon2id because it is in the standard library (`crypto/pbkdf2`, FIPS-approved),
  already implemented and tested, and meets the OWASP 2023 parameters. argon2id would add
  `golang.org/x/crypto` for a marginal gain at this scale. At most 2 hash checks run
  concurrently (CPU bound); a waiting client that disconnects gives up its slot.
- Rate limiting, without account lockout. **Authoritative:**
  `docs/security/login-rate-limiting.md` (attacker models, exact numbers, claims with
  tests, accepted residual risks). Summary: the network is an IPv4 address or IPv6 /64,
  taken from `httpx.ClientIP` (X-Forwarded-For only from trusted proxies). There are
  budgets per (network, user) with exponential delay capped at 15 min, per network across
  users, and per user across networks. Unknown users go through the same limiter and a
  dummy hash, and every attempt is counted at admission. _Recognised clients_ skip the
  shared network and user tiers and have their own small per-user tier: clients with a
  valid **device cookie** (HMAC-bound to the username, key derived from the persisted
  secret, max age 180 days, invalid after a password change). Memory is bounded: hashed keys, a fixed
  maximum per table, and an overflow bucket per tier instead of eviction, so a flood
  cannot flush an attacker's own counters. Accepted residual risk: a saturating botnet
  can slow a login from a new device (R1). Mitigations are the
  device cookie, the host CLI, and OIDC as the primary login. Every
  attempt is audited. Per-cookie revocation (a `device_cookies` table) is WP6b.
- Keys for CSRF tokens, device cookies and session signing are derived per purpose from
  `<DATA_DIR>/csrf.key` (32 random bytes, 0600, created on first start). They are separate
  from `secret.key`, which only encrypts data at rest, and they survive restarts.

### 15.2 Sessions

Server-side (`sessions` table). The cookie holds 32 random bytes; the store holds their
SHA-256. Sessions carry the sealed OIDC refresh token and the time of the last recheck
(OIDC only). Flags: `HttpOnly`, `SameSite=Lax` (needed for the top-level OIDC callback),
`Secure` when the request is HTTPS. The name is `__Host-tasmofleet` when Secure and the
path is `/`, otherwise `tasmofleet`. `Path` = base path, or `/` for `__Host-`. The ID is
rotated at login. Idle and absolute expiry apply, and expired sessions are cleaned hourly.
Every request re-validates the session: local user still exists, is enabled, password
unchanged since login, and `LOCAL_LOGIN` still on; OIDC session in OIDC mode only. Health
endpoints never create sessions (TasmoAdmin lesson A4). CLI `tasmofleet sessions revoke
[--user <name>]` ends sessions.

### 15.3 Roles

Two roles. Every authenticated principal has one of them; no mapped role means no access.

| Role     | May                                                                                                                                                           |
| -------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| operator | read everything except backup downloads and backup values (diffs show the keys that changed, values hidden); OTA batches, backups, power toggle (single and bulk), read and harmless console commands (`cmdpolicy` classes read and operator), cancel jobs, refresh a device's identity, change own local password |
| admin | + inventory edits, discovery, import, firmware management, any other console or bulk command (`cmdpolicy` class admin), restore, backup download and delete, device settings, system settings, policy rule editing, audit log, About, local user management (with step-up) |

Example: `Tasmofleet Users` -> operator, `Tasmofleet Admins` -> admin.

### 15.3a TLS and standalone operation

- `TLS_CERT_FILE` + `TLS_KEY_FILE`: the UI listener serves HTTPS. Files are re-read on
  SIGHUP.
- Neither certificate files nor a proxy nor the opt-in below (the zero-configuration mode; `TLS_SELF_SIGNED` was removed in 0.17):
  an ECDSA P-256 certificate is generated once and persisted in
  `<DATA_DIR>/tls/` (0600 key). It is valid 1 year and renewed 30 days before expiry. The
  startup warns that browsers will show a warning.
- Plain HTTP is allowed only behind `TRUSTED_PROXIES` (TLS ends at the proxy) or with an
  explicit `ALLOW_INSECURE_HTTP=true` (trusted LAN). Otherwise the process refuses to start
  and names the options.
- `PUBLIC_URL` is optional outside OIDC mode. Without it, absolute URLs for display only
  (e.g. on the About page) come from the request, and only when no trusted proxy is
  configured. They are never used for redirects that matter for security.
- The firmware listener stays plain HTTP in every mode (ESP8266 constraint).

### 15.4 Reverse proxy and external URL handling (package `httpx`)

- `PUBLIC_URL` (e.g. `https://tasmofleet.example.com`) is the only source for absolute
  URLs (OIDC redirect, logout redirect, links in notifications).
- `X-Forwarded-Proto`, `X-Forwarded-Host`, `X-Forwarded-Prefix` and `X-Ingress-Path` are
  honoured only from `TRUSTED_PROXIES`. They affect the base path for links and the
  scheme seen by logging, never authentication or redirect targets.
- Base path: `BASE_PATH` (static), or per request `X-Forwarded-Prefix` / `X-Ingress-Path`
  from a trusted proxy. Templates build every URL from it. HTMX requests use the same
  prefix. Static files and SSE live under it.

### 15.5 CSRF and browser hardening

- `http.CrossOriginProtection` (Sec-Fetch-Site / Origin) on every non-safe method, with
  `PUBLIC_URL`'s origin added as trusted origin, plus a per-session synchronizer token
  (form field `csrf` or header `X-CSRF-Token`, which HTMX sends via `hx-headers`).
  Login POST (`/login/local`) uses a pre-session token cookie (double submit).
- CSP: `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:;
connect-src 'self'; frame-ancestors 'self'; form-action 'self' <issuer origin>`. `frame-ancestors` is not configurable (the code passes no extra sources).
  No inline scripts (HTMX config via meta/attributes). `X-Content-Type-Options: nosniff`,
  `Referrer-Policy: same-origin`.
- All output goes through templ, which escapes every value. Device-provided strings are untrusted.

### 15.6 Threat model

Assets: control over devices (power, firmware), device credentials, Wi-Fi/MQTT secrets
in backups, the OIDC client secret, firmware integrity, the audit trail.

| Threat                                                                | Mitigation                                                                                                                                                                                                                                                          |
| --------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Wrong-family or corrupted firmware bricks a device                    | Guard (§7.5), header inspection, dual-source SHA-256, size limits, mutation-tested                                                                                                                                                                                  |
| Interrupted update leaves the device on minimal (TasmoAdmin A1)       | server-side persisted jobs, resume, probe-before-retry                                                                                                                                                                                                              |
| Duplicate `Upgrade` from blind retries (A1)                           | write-ahead state, at most 2 attempts per stage, probe first                                                                                                                                                                                                        |
| SSRF via device address, scan range, firmware URL, MQTT host (B4, A6) | Targets only from inventory, dial-time CIDR check, scans only inside `DEVICE_NETWORKS`, firmware hosts allowlisted per source, broker only from config                                                                                                              |
| Forged identity headers                                               | trusted only from `TRUSTED_PROXIES` by RemoteAddr                                                                                                                                                                                                                   |
| CSRF / cross-site actions                                             | CrossOriginProtection + token + SameSite                                                                                                                                                                                                                            |
| XSS via device-supplied names/replies (A6, B3)                        | templ escaping, CSP without inline script                                                                                                                                                                                                                           |
| Credential takeover through the web session (A6, B5)                  | no passkeys; local user management only for admins with step-up (current password), own password change needs the current one, password change/disable revokes sessions, no open registration (first admin from the environment or a generated password in a 0600 file, with a one-time setup link and forced change) |
| Session theft / fixation (A4)                                         | server-side sessions, rotation, HttpOnly/Secure/`__Host-`, no files                                                                                                                                                                                                 |
| Brute force / lockout abuse on local login (B5)                       | three-tier rate limit without account lockout, max 2 concurrent hash checks, audit, min. 12 characters, can be disabled                                                                                                                                             |
| Open redirect after login                                             | local-path check                                                                                                                                                                                                                                                    |
| Device password leakage to the browser or logs (B2)                   | never rendered; Basic auth header; redaction; sealed at rest with key in `secret.key`                                                                                                                                                                               |
| Backup secret leakage (§12.3)                                         | 0600, admin-only download, redacted views, audited                                                                                                                                                                                                                  |
| Mass assignment / Backlog injection in settings (B3)                  | typed fields, per-field commands, `;` rejected                                                                                                                                                                                                                      |
| Restore blob behind auth (B8)                                         | one-time token on the firmware listener, bound to device IP                                                                                                                                                                                                         |
| Malicious upstream release                                            | cannot be ruled out; dual-source compare, cache by SHA-256, releases only on explicit refresh, never auto-update devices                                                                                                                                            |
| IoT VLAN client abusing the firmware port                             | read-only, only indexed files, no secrets, rate-limited per IP                                                                                                                                                                                                      |
| Loss of the database                                                  | WAL, pre-migration copies, everything in `/data` for host backups                                                                                                                                                                                                   |
| Lost updates from concurrent writers (B1)                             | SQLite transactions, unique indexes, optimistic state check in `SaveTransition`                                                                                                                                                                                     |

---

## 16. Configuration

**Principle: a fresh install needs no configuration.** `docker run -p 8443:8080 -v data:/data tasmofleet`
(`8443` is the host port you map, the UI inside the container is always on `8080`; pick any free host port)
serves a self-signed certificate (no proxy, no certificate and no `ALLOW_INSECURE_HTTP`
set), creates the first local admin (random password in `<DATA_DIR>/initial-admin-password`, a one-time setup link in the log,
changed at the first sign-in) and shows the setup (device networks, optional MQTT, optional
single sign-on, first scan). Everything else is adjusted in the web UI.

Two tiers:

1. **Bootstrap and security settings: environment variables `TASMOFLEET_<KEY>`** or the same keys
   (prefix optional) in `/data/tasmofleet.env` (`KEY=value`, `#` comments). The environment
   wins. Secret keys also accept `<KEY>_FILE`. They are needed before the database exists or must
   never be changeable from the web; the System settings page lists them read-only (secrets
   masked). The authoritative list is the `env` tags of `internal/config.Config` without a `ui` tag; the operator-facing reference is `docs/configuration.md` (checked against the tags by `configdoc_test.go`).
2. **Operational settings: the System settings page** (`/settings/system`, admin), stored in SQLite
   (`internal/sysconfig`, `docs/security/system-settings.md`). They are no longer environment
   variables. Sign-in settings (`ui:"lock"`) are the exception: stored in the database unless
   the environment sets them, then the environment wins and the field is read-only.

| Key (bootstrap, environment only)                                                                             | Default                                                    | Meaning                                                                                |
| ------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------- | -------------------------------------------------------------------------------------- |
| `DATA_DIR` | `/data` | |
| `CONFIG_FILE` | `<DATA_DIR>/tasmofleet.env` | the env file to read |
| `LISTEN`                                                                                                      | `:8080` (one port in every mode; containers do not set it) | UI/API listener                                                                        |
| `FIRMWARE_LISTEN`                                                                                             | `:80`                                                      | device-facing firmware listener                                                        |
| `PUBLIC_URL`                                                                                                  | —                                                          | external URL; required for oidc, optional otherwise                                    |
| `BASE_PATH`                                                                                                   | `""`                                                       | static prefix, e.g. `/tasmofleet`                                                      |
| `TRUSTED_PROXIES`                                                                                             | —                                                          | CIDRs; required for header mode; with a proxy the listener stays plain HTTP            |
| `HEADER_USER`, `HEADER_NAME`, `HEADER_EMAIL`, `HEADER_GROUPS`, `HEADER_GROUPS_SEPARATOR`, `HEADER_LOGOUT_URL` | Authentik names, `\|`                                      | header mode (`AUTH_MODE=header` is environment only)                                   |
| `OIDC_POST_LOGOUT_URL`                                                                                        | `PUBLIC_URL`+base                                          |                                                                                        |
| `LOCAL_LOGIN`                                                                                                 | `true`                                                     | local accounts on the login page (always on in `local` mode); break-glass              |
| `SESSION_IDLE`, `SESSION_MAX`, `OIDC_RECHECK`                                                                 | `2h`, `12h`, `15m`                                         |                                                                                        |
| `INITIAL_ADMIN_PASSWORD(_FILE)`                                                                               | random, logged once                                        | first local admin, must change at first login                                          |
| `TLS_CERT_FILE`, `TLS_KEY_FILE`, `ALLOW_INSECURE_HTTP`                                                        | —, —, `false`                                              | §15.3a; with none of them and no `TRUSTED_PROXIES` a self-signed certificate is served |
| `TLS_SANS`                                                                                                    | —                                                          | extra DNS names/IPs for the self-signed certificate                                    |
| `DECODER_PATH`                                                                                                | `/usr/local/bin/decode-config`                             | empty disables backup decoding                                                         |
| `FIRMWARE_UPSTREAM_OVERRIDE`                                                                                  | —                                                          | tests and development only                                                             |
| `LOG_FORMAT`                                                                                                  | `json`                                                     |                                                                                        |

| Setting (System settings page)                                                                                                                                                                          | Default                                                                              | Applies                                                                                 |
| ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------- |
| `AUTH_MODE` (`local`, `oidc`), `OIDC_ISSUER`, `OIDC_CLIENT_ID`, `OIDC_CLIENT_SECRET`, `OIDC_SCOPES`, `OIDC_GROUPS_CLAIM`, `OIDC_USERNAME_CLAIM`, `OIDC_DISPLAY_NAME`, `ADMIN_GROUPS`, `OPERATOR_GROUPS` | `local`, …, `openid,profile,email`, `groups`, `preferred_username`, `Single sign-on` | restart; step-up for all but the button name and scopes (2026-10-20); environment locks |
| `DEVICE_NETWORKS` | — (no device is contacted until set) | live |
| `FIRMWARE_BASE_URL`, `FIRMWARE_SOURCES`, `FIRMWARE_ALLOW_SINGLE_SOURCE`, `FIRMWARE_REFRESH_INTERVAL`                                                                                                    | —, `github,ota.tasmota.com`, `false`, `6h` | restart (the firmware cache, the device-facing listener and the OTA routes are built at start) |
| `OTA_PARALLEL`, `OTA_RESTORE_OTAURL`, `OTA_POWER_MAX_AGE`                                                                                                                                               | `2`, `true`, `20m` | live |
| `BACKUP_KEEP`, `BACKUP_MAX_AGE`, `BACKUP_INTERVAL`                                                                                                                                                      | `10`, `720h`, `0s` (manual) | live |
| `POLL_INTERVAL`, `TELEPERIOD`                                                                                                                                                                           | `5m`, `300s` | live |
| `MQTT_URL`, `MQTT_USER`, `MQTT_PASSWORD`, `MQTT_FULLTOPIC`                                                                                                                                              | —, —, —, `%prefix%/%topic%/` | live (the broker connection is switched in place) |
| `DEVICE_DEFAULT_PASSWORD` (secret; prefill for add device / accept proposal, user `admin`)                                                                                                              | —                                                                                    | live                                                                                    |
| `LOG_LEVEL` (`debug`, `info`, `warn`, `error`; the environment locks it)                                                                                                                                | `info`                                                                               | live                                                                                    |
| `DISCOVERY_MQTT`, `DISCOVERY_SCAN`, `DISCOVERY_SCAN_INTERVAL`, `DISCOVERY_SCAN_CIDRS`                                                                                                                   | `true`, `false`, `24h`, —                                                            | live                                                                                    |
| `METRICS_ENABLED`, `METRICS_DEVICE_LABELS`, `AUDIT_RETENTION`                                                                                                                                           | `true` (off: `/metrics` is 404), `false`, `8760h`                                    | live                                                                                    |
| `METRICS_TOKEN(_FILE)` (secret, write-only; the environment locks it)                                                                                                                                   | empty = only local scrapes                                                           | live                                                                                    |
| `POLICY_LIVE_INTERVAL` (policy check background reads)                                                                                                                                                  | `1h` (0 = off)                                                                       | live                                                                                    |
| policy rules (editor, versioned)                                                                                                                                                                        | none                                                                                 | live                                                                                    |

**Upgrade from environment-based setups.** A removed operational variable that is still set
(environment or env file) is imported once on the first start of the new version: only into keys
the database does not hold yet, with a warning per key ("imported … from the environment; remove it
from the container configuration"); credentials in a legacy `MQTT_URL` move to the sealed
password; `POLICY_RULES` (inline JSON or a file) becomes the first managed rule version. After that
the variables are ignored (warning). `tasmofleet settings export|import <file>` moves the
settings and the policy rules as JSON for automated setups (secrets are never exported).

**Policy history.** Every save of the policy rules is a stored version (append-only). The history
view is derived: consecutive versions of one actor with each gap at most 2 minutes fold into one
entry (a revert, an import and the current version never fold; nothing is stored), days are grouped
in the server's zone, and each entry carries a summary from the diff against the version before it
(rules matched by id, or by name for versions stored without ids). `sysconfig.Rules.History` pages
it with a `before` cursor; `sysconfig.RuleHistory` lists the versions that touched one rule.

Validation errors at start name the key and stop the process. Settings saved on the page use the
same validators; a stored value that stops validating is skipped with a log line and shown on the page.

---

## 17. HTTP routes

UI listener (all paths relative to the base path; `R` = role needed; non-GET = CSRF checked). The
authoritative lists are `internal/web/web.go` (and the `register*` functions it calls),
`internal/auth/service.go`, `internal/auth/accountweb.go` and `internal/app/app.go`. Pages that exist
only when their feature is wired (updates, firmware, policy, ...) are listed with it.

| Method | Path | R | Purpose |
| --- | --- | --- | --- |
| GET | `/healthz` | none | liveness, no session, no DB write |
| GET | `/readyz` | none | DB ping, OIDC discovery done, firmware listener up |
| GET | `/metrics` | token or local scraper | Prometheus; 404 when `METRICS_ENABLED` is off |
| GET | `/static/*`, `/favicon.ico` | none | embedded assets |
| GET | `/login` | none | sign-in page (IdP button and local form); redirects to the IdP if OIDC is the only method |
| GET | `/login/sso` | none | start the OIDC flow (the button) |
| GET | `/oidc/callback` | none | OIDC redirect target |
| GET, POST | `/login/local` | none | GET redirects to `/login`; POST checks the local login |
| POST | `/logout` | any | logout (+ RP-initiated) |
| POST | `/oidc/stepup` | admin | step-up by re-authentication at the IdP |
| GET | `/` | operator | dashboard: six stat tiles, fleet health bar, "Needs attention", update transactions |
| GET | `/devices`, `/devices/table` | operator | device list (filters `q`, `health`, `family`, `tag`, `chip`; rows live via SSE) |
| GET, POST | `/devices/new` | admin | add a device (with probe) |
| GET | `/devices/{id}`, `/devices/{id}/history`, `/devices/{id}/live` | operator | overview tab, health history, live values |
| GET, POST | `/devices/{id}/edit`, `/devices/{id}` | admin | edit |
| GET, POST | `/devices/{id}/delete`, `/devices/{id}/restart` | admin | confirm and delete / restart one device |
| POST | `/devices/{id}/probe` | operator | refresh identity |
| POST | `/devices/{id}/name/adopt`, `/devices/{id}/name/keep` | admin | answer the "Name on the device" notice (§5) |
| POST | `/devices/{id}/fwreach/forget` | admin | forget what is known about the device reaching the firmware server (back to unknown; audit `device.fwreach`) |
| GET, POST | `/devices/{id}/replace`, `/devices/{id}/replace/preview` | admin | Replace device (new hardware, same identity) |
| GET, POST | `/devices/{id}/settings` | admin | settings editor |
| GET, POST | `/devices/{id}/console` | operator | console; `cmdpolicy` raises the needed role to admin per command |
| POST | `/devices/{id}/power/{n}` | operator | power toggle |
| POST | `/devices/bulk/power` | operator | power for the selection |
| POST | `/devices/bulk/command`, `/devices/bulk/command/run`, `/devices/bulk/restart`, `/devices/bulk/restart/run`, `/devices/bulk/backup` | admin | bulk Send command, Restart, Back up (§14.1) |
| GET | `/devices/bulk/result/{batch}` | admin | result of a bulk run |
| POST | `/devices/{id}/backup` | operator | backup now (1 minute cooldown per device, §12.4) |
| GET | `/devices/{id}/backups`, `/devices/{id}/backups/diff`, `/devices/{id}/drift` | operator | Backups tab, diff (values hidden for operators), Drift tab |
| POST | `/backups/{id}/keep` | operator | pin or unpin a backup run |
| GET | `/backups/{id}` | admin | download (audited) |
| GET, POST | `/backups/{id}/delete`, `/devices/{id}/backups/delete`; GET `/devices/{id}/backups/prune` | admin | delete backups |
| GET, POST | `/backups/{id}/restore` | admin | restore (confirm, start) |
| GET | `/restores/{id}`, `/restores/{id}/status` | admin | restore progress |
| GET | `/firmware`, `/firmware/releases/{version}` | operator | releases, cache, release notes |
| POST | `/firmware/refresh`, `/firmware/fetch` | admin | index refresh / fetch one file |
| GET, POST | `/firmware/artifacts/{id}/delete`; POST `/firmware/delete-selected` | admin | confirm / delete cached files (§8.2a) |
| POST, GET | `/firmware/prefetch`, `/firmware/prefetch/{id}`, `/firmware/prefetch/{id}/stream` | admin | prefetch the fleet's files of a release, progress (§8.2a) |
| POST | `/firmware/custom` | admin | upload custom firmware (§8.6) |
| GET, POST | `/updates/new` | operator | select devices, release, policy |
| POST | `/updates/preview` | operator | dry run table |
| POST | `/updates` | operator | submit batch |
| GET | `/transactions` | operator | transaction history (updates, policy fixes, commands, backups) |
| GET | `/transactions/{batch}`, `/transactions/{batch}/live` | operator | batch progress (SSE) |
| GET, POST | `/transactions/{batch}/cancel`, `/transactions/{batch}/retry`, `/jobs/{id}/cancel` | operator | cancel a batch or job; retry the failed devices |
| GET | `/policy`, `/policy/deviations`, `/policy/history`, `/policy/io`, `/policy/io/export` | operator | overview, deviating devices, version history, import or export page, export |
| GET | `/policy/rules/{id}` | operator | rule page (read-only for operators) |
| POST | `/policy/live` | operator | run the live check now |
| POST | `/policy/live/pause`, `/policy/live/resume` | admin | pause or resume the background reads |
| GET, POST | `/policy/rules/new`, `/policy/rules/duplicate`, `/policy/rules/new/copy`, `/policy/rules/preview`, `/policy/rules/save`, `/policy/rules/delete`, `/policy/rules/value-control`, `/policy/rules/order`, `/policy/rules/enable`, `/policy/rules/disable`, `/policy/rules/revert/{id}`, `/policy/io/import` | admin | rule editor and list operations (§14.4) |
| POST | `/policy/fix`, `/policy/fix/apply` | admin | Make compliant: preview, apply |
| GET, POST | `/discovery`, `/discovery/scan`, `/discovery/scan/{id}`, `/discovery/scan/{id}/stream`, `/discovery/scan/{id}/cancel`, `/discovery/proposals/...` (accept, dismiss, undismiss, one or many) | admin | "Add devices": proposals and scans |
| GET, POST | `/import/tasmoadmin`, `/import/tasmoadmin/apply`, `/import/tasmoadmin/report.csv` | admin | web import |
| GET, POST | `/settings/system` (+ `/stepup`, `/mqtt-test`, `/sso-test`, `/reset`) | admin | System settings (§16) |
| GET, POST | `/setup`, `/setup/mqtt-test`, `/setup/finish` | admin | first-run wizard |
| GET, POST | `/setup/account` | setup link | set the initial admin password |
| GET | `/audit` | admin | audit log |
| GET | `/about` | admin | version, decoder version, effective config (secrets masked) |
| GET | `/whats-new`, `/nav`, `/styleguide`, `/events` | operator | What's new, navigation page, style guide (sample data), SSE stream (health, job, device, backup) |
| POST | `/prefs/{key}` | operator | per-user UI preference |
| GET | `/account` | signed-in user | own account: profile, how they sign in |
| POST | `/account/password`, `/account/profile` | local user | change own password (current one required), profile |
| GET | `/users`, `/users/new`, `/users/{name}`, `/users/principal/{subject...}` | admin | all principals and their detail; no step-up for viewing (header admins: view only) |
| POST | `/users`, `/users/stepup`, `/users/local/{name}`, `/users/sessions` | admin + step-up (password or IdP) | local user management, End sessions for SSO users |

Every other path answers with the app's own not-found page. (`GET /jobs` and `/updates/{batch}`
no longer exist; the transactions pages replaced them.)

Firmware listener: `GET|HEAD /fw/<release>/<filename>`, `GET|HEAD /c/<key>/fw/<release>/<filename>` (uploaded
firmware) and `GET /restore/<token>`. Everything else is 404.

A JSON API for automation (with API tokens) is out of scope until there is a consumer.
Home Assistant already gets device data from Tasmota.

---

## 18. UI

Server-rendered pages and HTMX partials in templ (committed generated code, no build step). No inline
scripts. One embedded stylesheet with a light, a dark and an automatic theme (the user's choice is a
stored preference). Usable at phone width. Every page renders inside the shell with a top bar,
a group menu and a sub-navigation (`internal/web/ui/nav.go`; every role sees the same menu, each
page enforces its own permission). Groups and pages:

- **Dashboard** (Overview): six stat tiles (Online, Offline, Unknown, Firmware behind, Policy
  deviations, New devices found; each links to a filtered device list), the fleet health bar,
  "Needs attention" and the newest update transactions.
- **Devices**: *All devices*, *Add devices* (discovery, scans, manual add, TasmoAdmin import; admin) and
  *Policy*. The device list is a sortable table with filter chips and a search box (`q`, health,
  family, tag, chip); the selection bar feeds Update, Back up, Send command, Restart and power.
- **Device page** (tabs): Overview (tiles, identity, network facts), Settings and Console where the
  device allows them, Backups, Drift and Replace device (admin).
- **Updates**: *Transactions* (batches of every job kind, with progress, cancel and retry) and *Firmware*
  (releases, cache, prefetch, upload, release notes). The update flow is: select release + policy
  (+ variant override; the preview shows it, no confirmation checkbox since 2026-10-20), a preview
  table, submit, then the live transaction page (per device: state, stage, attempt, last event,
  elapsed time).
- **System**: *Settings* (System settings, admin), *Audit log* (admin), *Users* (admin), *About*
  (admin: versions, decoder, effective configuration with secrets masked, links to docs) and
  *Styleguide* (sample data). *What's new* (parsed from `CHANGELOG.md`) and the account page open from
  the avatar menu; the first-run wizard `/setup` appears for admins until it is finished.

Accessibility: semantic tables and forms, labels, focus styles, no colour-only status
(each badge has text).

---

## 19. Observability

- **Logs**: `log/slog` JSON to stdout (`LOG_FORMAT=json`), fields `time, level, msg,
component, device, job, batch, request_id, remote, user`. No secrets (redactor), no
  device passwords, no backup content. Access logs at info for state-changing requests,
  debug for GETs.
- **Metrics** (`/metrics`, contract with `deploy/grafana/tasmofleet.json`): see the package
  doc of `internal/metrics`. Labels have low cardinality: device names only on
  `tasmofleet_backup_last_success_timestamp_seconds` and the optional
  `tasmofleet_device_*` gauges (fleet of tens; `docs/metrics.md`).
- **Health**: `/healthz` = process alive (blackbox probe). `/readyz` = DB, OIDC discovery,
  listeners.
- **Audit**: database table plus the UI page. Each audit write is also logged at info.

---

## 20. Packaging and deployment

- **Image**: multi-stage `Dockerfile`; Go build with `CGO_ENABLED=0`; final
  `gcr.io/distroless/cc-debian13:nonroot` (glibc for the decoder), user 65532. Includes the
  decoder (§12.2). Size about 100 MB with decoder, about 35 MB without. `HEALTHCHECK` uses
  `tasmofleet healthcheck` (no curl in distroless). `VOLUME /data`; ports 8080 (UI, always this container port: map any free host port to it) and 80
  (firmware). Binding port 80 as non-root relies on Docker's default
  `net.ipv4.ip_unprivileged_port_start=0` in the container namespace. If a runtime
  doesn't set it, use `FIRMWARE_LISTEN=:8081` and a port mapping.
- **Registry**: `ghcr.io/nofuturekid/tasmofleet` and, when the repository's Docker Hub secrets are set,
  `docker.io/nofuturekid/tasmofleet` (overview text `deploy/dockerhub/README.md`), with the same tags
  `<version>` (X.Y.Z) and `latest`. Built and published by GitHub Actions.
- **Unraid**: `deploy/unraid/tasmofleet.xml` (br0, fixed IP, `/data` ->
  a host data directory, env vars, WebUI link to `PUBLIC_URL`). Unraid users take the Community
  Applications template, see CLAUDE.md "Releases".
- **Traefik**: example dynamic config in `deploy/traefik/tasmofleet.yml` (router on
  `websecure`, service `http://172.31.12.61:8080`). No forward-auth middleware in OIDC mode;
  an Authentik outpost middleware in header mode.
- **Authentik**: `deploy/authentik/README.md` lists provider settings (confidential client,
  redirect URI, signing key RS256, grant types authorization_code + refresh_token, scopes,
  groups).
- **Grafana**: `deploy/grafana/tasmofleet.json`, 2 graphs per row (layout rule).
- **Data**: back up the data directory like any application data. `secret.key` must be in
  the same backup, or the device passwords cannot be decrypted.

### 20.1 Home Assistant add-on path (M3)

The design is ingress-ready from M1: base path from `X-Ingress-Path` (trusted proxy
`172.30.32.2`), relative links everywhere, SSE under the base path. The CSP allows `frame-ancestors 'self'` only, so
the ingress frame may need a configurable source when M3 is built. M3 adds:

- an add-on repository with `config.yaml` (`ingress: true`, `ingress_port: 8080`, port 80
  mapped for firmware, `map: [addon_config]`. The add-on options are bootstrap settings only
  (`TASMOFLEET_*` by a tiny entry script or `/data/options.json`): trusted proxy, listeners, log
  level. Operational settings (device networks, MQTT, firmware, backups, policy) are set on the
  System settings page; the first start can be preseeded with `tasmofleet settings import`
  from the add-on config directory;
- auth mode `ingress`: trust `X-Remote-User-Id`/`X-Remote-User-Name`/`X-Remote-User-Display-Name`
  from the supervisor proxy only; HA admins get admin, other HA users operator;
- MQTT broker and credentials from the Supervisor services API (`mqtt` service discovery): offered
  as the suggestion in the setup instead of being typed.

---

## 21. Testing strategy and gates

| Layer                           | What                                                                                                                                                                                                                                                                         | Where                                  | Gate                                                                                                                                      |
| ------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| Unit                            | pure logic, parsers (fixture-driven), handlers with fakes via `httptest`                                                                                                                                                                                                     | `*_test.go` next to code               | `make test`, `make race`                                                                                                                  |
| Fuzz | `fwselect.Detect`, `InspectImage`, `tasmoadmin` parsers, `cmdpolicy`, `mqttbus` topic decoding, firmware notes and server paths, settings URL credentials | `Fuzz*` with seed corpus from fixtures | the seed corpora run as ordinary tests in `make test`; no timed fuzzing in CI |
| Mutation | `fwselect`, `ota`, `policy`, `cmdpolicy` | gremlins | `make mutation` (`MUTATION` in the Makefile): efficacy **≥ 90 % fwselect, ≥ 85 % ota, ≥ 85 % policy, ≥ 90 % cmdpolicy**; PRs touching a `SAFETY` package run `make mutation-changed` |
| Integration (tag `integration`) | engine + simulator fleet (all profiles and faults), store on real SQLite, mqttbus + embedded broker, auth against oidcmock, decoder against the real binary (skipped with a reason only if `DECODER_PATH` is missing; CI sets `REQUIRE_DECODER=1`, so there a missing decoder fails)                                       | `*_integration_test.go`                | `make integration`                                                                                                                        |
| e2e                             | real binary + `tasmofleet-devenv` (mixed fleet: MQTT and HTTP-only devices, ESP8266 1 MB staged, ESP32 direct, unknown chip refused), login via mock OIDC with Authentik claims, update flow end to end, header mode via a test proxy, base path `/tasmofleet`, no-MQTT mode | `e2e/tests/*.spec.ts` | `make e2e` (all but quarantined), `make e2e-core` (tag `@core`) |
| Static                          | gofmt, go vet, staticcheck, golangci-lint (incl. depguard, exhaustive, gosec, bodyclose, noctx)                                                                                                                                                                              |                                        | `make check`                                                                                                                              |
| Supply chain                    | govulncheck                                                                                                                                                                                                                                                                  |                                        | `make vuln`                                                                                                                               |

Simulator scenarios (minimum set, each an integration test):
ESP8266 4 MB direct; ESP8266 1 MB staged (with OtaUrl ending as minimal and being restored);
Tasmota auto-fallback (`ota_loader`) continuing on its own; ESP32 direct; ESP32-C3 and S3
selection; unknown hardware refused; wrong-family artifact refused (header) ; safeboot
artifact refused; variant change refused without flag; reboot gap longer than
UpgradeAccept; download failure ("Failed File Not Found (404)") with retry then fail;
device never comes back; process restart in every non-terminal state (resume never
re-sends Upgrade); lost Upgrade response; another tool changed OtaUrl; auth failure;
HTTP-only device without MQTT; flapping LWT; `{"Command":"Unknown"}` for Status on
minimal.

CI (`.github/workflows/ci.yml`): two tiers. FAST on every PR and push to `main` (lint, unit and integration tests
from the cache, build, `@core` e2e in two shards, safety race and mutation when a `SAFETY` package changed;
the `required` job gates the merge). FULL nightly, on tags and by dispatch (every e2e test, race on all
packages, uncached tests, full mutation); a tag's image is published only after FULL passed. Details:
CLAUDE.md "Test tiers in CI".

---

## 22. Migration from TasmoAdmin

1. Deploy tasmofleet next to TasmoAdmin (new IP). If a firewall restricts devices to the
   OTA server, allow the new address (IoT -> TCP 80) or plan step 6.
2. `tasmofleet import tasmoadmin /path/to/tasmoadmin/data --dry-run`, then run it for real.
   Check probes and MAC merge.
3. Run backups of all devices (manual batch), then compare a few decoded backups with
   the fleet policy.
4. Do the first OTA on one ESP8266EX plug and one ESP8285 lamp (both staged), then one
   ESP32 (direct). Check that `OtaUrl` is restored.
5. Run both in parallel for a while (TasmoAdmin read-only).
6. Retire TasmoAdmin. Optionally move tasmofleet to its address so a firewall rule stays
   unchanged.

Known leftover: devices updated by TasmoAdmin store
`http://tasmoadmin.example.internal:80/data/firmwares/tasmota-minimal.bin.gz` as OtaUrl
(OBS). tasmofleet records whatever it finds as `OriginalOtaURL`. The M2 policy view can
offer to set a clean value fleet-wide.

---

## 23. Milestones and exit criteria

**M1 core**:
Exit criteria: `make ci` green; e2e covers login (OIDC mock + header mode +
local accounts incl. first run, standalone mode without proxy/IdP), import, staged and direct OTA on simulators, backup with decoder, no-MQTT
mode; mutation thresholds met; image published to the registry; a manual staged update
on a real fleet.

**M2 parity**: device list live state + toggle, console, settings editor, bulk commands

- templates, restart, autoscan + MQTT discovery, policy/drift view, backup diff +
  restore, custom firmware upload, web import. Planned and not built: the development channel, notifications,
  fleet settings templates (§8.5, §14.5, §14.2).
  Exit criterion: TasmoAdmin can be retired.

**M3**: Home Assistant add-on with ingress auth, public GitHub repository
(`github.com/nofuturekid/tasmofleet`, the module path already matches), GHCR image,
`.github/workflows` copy, fixtures re-checked for publication.

---

## 24. Risks and open points

- **Minimal behaviour is derived from source, not observed** (no device was on minimal
  during capture). The simulator follows the source analysis. The first real staged update
  must be watched, and the simulator adjusted if needed.
- **Image tags of ESP32 variant and chip builds** (`tasmota32c3`, `tasmota32-sensors`, ...)
  are partly SRC/assumed. Unknown tags refuse (safe), and each newly seen tag gets a
  fixture.
- **solo1 hardware strings** need confirmation. Until then, unknown ESP32 single-core
  strings refuse.
- **decode-config CLI flags** are verified only in the integration test against the
  pinned binary.
- **Port 80 as non-root** depends on the container runtime's unprivileged port setting
  (§20).
- **Free flash on ESP8266** (`Status 4 Free`) semantics for gzip OTA: the staged/direct
  threshold is conservative, and a real update verifies it.
