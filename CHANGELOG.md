# Changelog

All notable changes. Breaking changes are marked **breaking**. tasmofleet has no
compatibility shims (see CLAUDE.md), so read this before upgrading. Database migrations
always upgrade automatically.

## Unreleased

## 0.11.3 (2026-10-24)

### Added

- Device page: State changes and Transactions show the newest 10 (was 20), each with a link to the complete
  list: a new per-device history page with paging (`/devices/{id}/history`), and `/transactions?device=ID`,
  which lists every transaction that device took part in.
- Retention for transactions: finished transactions and their logs older than the audit retention
  (`AUDIT_RETENTION`, default 365 days) are deleted daily. A running transaction, or a transaction with one
  unfinished job, is never touched; a transaction that a kept retry points at stays too.

### Changed

- A device's first observation after the app started is a baseline, not a state change: a restart no longer
  logs "Unknown to Online" for every device. The cost: a change that happened while the app was down is not
  recorded.
- Power & energy card: a device without readings says "No readings yet", and one that drew 0 W the whole time
  says "No load in the last 2 h", instead of an empty or dashed chart.

### Fixed

- Device header: the Power On/Off control was an inverted (light in the dark theme, dark in the light theme)
  pill while the switch was on; it now keeps the look of Restart and Back up in every state and theme, and the
  switch alone shows On. A power switch outside the table and card cells no longer inverts either.
- Device overview: the State changes and Transactions row touched the cards above it (a column box drops its
  last card's bottom margin). All gaps between cards use the new `--card-gap` token, and an e2e check fails
  when two sibling cards come closer than that.
- Device overview: the Columns button of the State changes and Transactions tables sits in the card head, on the
  title's line, instead of a row of its own.
- e2e: the devices toolbar check measures in one in-page step (the table part reloads 0.5 s after the first health
  reports and left a detached frame behind), and the phone audit budget covers the spread of the audit page's
  system rows (3370 to 3414 px on the same seed).
- Device page: Hostname, Gateway, SSID, BSSID and Channel no longer depend on a backup snapshot. Every probe (first
  contact after an import or add, "Refresh identity") reads Status 5 and Status 11, one at a time and never
  Status 0, and stores the facts on the device (migration 0026). The newest snapshot stays the fallback for
  devices not probed since.
- Network card: the row that showed the configured host as "IP address" is now "Address"; the device's real IP
  (Status 5) is its own "IP address" row.

## 0.11.2 (2026-10-23)

### Changed

- The project moved to GitHub (github.com/nofuturekid/tasmofleet) with a fresh history;
  container images are published to `ghcr.io/nofuturekid/tasmofleet`.
- **License: AGPL-3.0-or-later** (was MIT). The About page links to the source code.

### Fixed

- Dark theme: the popups of native selects (the Chip and Tag filters and the Sort menu on the
  device list) showed light option text on the browser's white popup, unreadable. Every option
  now paints the theme's surface and text colours. A new e2e guard checks every page in light,
  dark, Auto and an explicit choice over the system setting: each form control resolves
  `color-scheme` to the theme in force, and every option has an opaque theme-coloured ground
  with readable text (screenshots never show native popups).
- Device list: the "Not compliant" chip read "0+" while part of the fleet was not checked; it
  now shows the known count, with "n not checked yet" in its tooltip (as the dashboard tile
  says it). It still shows "–" while nothing is checked.
- Device list: "All" kept a health filter from a dashboard tile (`?health=online`), so the
  list stayed narrowed. All (and every other chip) now drops the health filter: from any
  dashboard tile, All shows the whole fleet and the URL carries no status filter.
- Behind the HTTPS proxy every live-update stream ended after 20 s (since 0.11.1), so /devices
  kept showing "Live updates interrupted". The stream's periodic session re-check looked for
  the plain-HTTP session cookie; it now sees the request's scheme and base path. The same
  re-check in the discovery and firmware-download streams is fixed too.
- The connection banners wait: a stream that comes back within 15 s (and fewer than three
  failed reconnects) no longer shows "Live updates interrupted" or "Reconnecting" in the top
  bar. When a banner shows, it floats below the header instead of pushing the page down.

## 0.11.1 (2026-10-23)

### Fixed

- Break-glass sign-in over plain HTTP on the container's own address (e.g.
  `http://tasmofleet.example.internal:8080`) failed with "missing or invalid CSRF token" when
  `PUBLIC_URL` was `https://...`: every cookie was Secure and `__Host-` prefixed, so the
  browser dropped them. The Secure flag and the `__Host-` name now follow the scheme of each
  request (own TLS listener, or `X-Forwarded-Proto` from a `TRUSTED_PROXIES` address), never
  `PUBLIC_URL`. The OIDC redirect still uses `PUBLIC_URL`.

### Security

- Behind a reverse proxy, cookies are Secure only when the proxy is listed in
  `TRUSTED_PROXIES` and sends `X-Forwarded-Proto: https`. Without `TRUSTED_PROXIES` the
  public HTTPS site now gets non-Secure cookies; set it for the proxy (the reference
  deployment already does). A plain-HTTP session cookie is sent in clear text on the LAN (accepted for
  break-glass in a homelab).

## 0.11.0 (2026-10-23)

### Added

- Settings → Backups shows the backup storage (design v2 N9, package W5): number of backups,
  total size, kept backups and devices without a backup, the ten largest devices with a link
  to each device's backups, and the backups of deleted devices (kept with "keep the backups")
  with Download, Keep and Delete. Deleting such a backup now returns to Settings → Backups
  instead of a missing `/devices/0/backups` page.
- "Reset settings…" on Settings → Advanced puts every setting stored in the UI back to its
  default after one confirmation and the step-up (`POST /settings/system/reset`, audited as
  "Settings reset"). Settings from the environment, the first-run setup state and a paused
  policy live check are kept; devices, backups and users are untouched.
- Design v2 components (port package F4): `internal/web/ui` gets the design system's components
  with typed props (page header, buttons, badges, status dots, avatar, cards and key/value rows,
  form fields with the server's error, radio cards, segmented controls, switches, stat tiles, the
  fleet health bar, the attention list, transaction card and rows, tabs, row menus with disabled
  reasons, confirmation and step-up dialogs, confirmation pages, banners, toasts, empty / no match
  / loading states, the error page, the sign-in card, the console, the backup comparison and
  release notes). Pages adopt them as they are ported.
- `make styleguide-docs` writes a screenshot of every `/styleguide` section (desktop and phone,
  light and dark) into `docs/design/styleguide/`; `docs/spec/design-system.md` is rewritten for v2
  and embeds them. A test fails when a section has no doc entry or no screenshot.
- Buttons with `data-toast` show that toast (`toast.js`).
- Backups can be kept (design v2, N1): `POST <base>/backups/{id}/keep` with `kept=1|0` (operators
  and admins) keeps or releases a whole backup run. Kept backups are never pruned and do not count
  against `BACKUP_KEEP`. Each backup records what started it (manual, scheduled, before an update
  with the target release, restore, replace, bulk). A failed backup is recorded as an attempt with a
  reason that never contains the device URL or credentials; attempts are pruned with the audit
  retention. "Delete selected": `POST <base>/devices/{id}/backups/delete` (admin, `id` repeated,
  `confirm=1`) deletes whole runs of that device only. A bulk backup takes `keep=1` ("Keep these
  backups") and `label` (stored with each dump). The pages show these with the backups page port.
- Delete a device but keep its backups (design v2, N2): the device delete takes `backups=keep|delete`
  and the acknowledgement `ack=1` (checked on the server). Kept backups stay with the device's name
  and MAC and are listed as backups of a deleted device (Settings, with N9).
- Users v2 backend (design v2, N4; the pages follow with the users page package): local accounts
  get a display name, an e-mail address and "created at / by" (migration 0023). Admins can create
  an account or reset a password with a generated **one-time password** (form field `otp` on
  `POST <base>/users`, action `otp` on `POST <base>/users/local/{name}`): it is shown once, stored
  hashed, never logged or audited, must be changed at the sign-in and is refused after 24 hours.
  `GET <base>/users/{name}` and `GET <base>/users/principal/{subject}` show a user's details, live
  sessions and last 10 audit entries; `POST <base>/users/sessions` with a `session` field ends one
  single session; `POST <base>/account/profile` sets the own profile; the action `profile` sets
  another account's. SSO and proxy users keep the e-mail of their last sign-in.
- Sign-in extras (N11): the sign-in page data carries the attempts left before the rate limiter
  delays sign-in (a count only), a "session expired" flag (the login redirect gets `expired=1`
  when the request's session ran out) and the "Keep me signed in on this device" choice (form
  field `remember`).
- Setup step 1 (N12): at first start the log line that reports the created admin account also
  carries a one-time setup link `<base>/setup/account?token=...`. It sets the admin's password,
  signs the admin in and continues with the setup. The token is valid only while the admin still
  has its first-start password (every start prints a fresh one until then) and is never stored,
  shown in the UI or audited.
- User detail, one-time password and setup step 1 pages (design v2, N4 + N12 UI, package W2): a
  user's page (`/users/{name}`, `/users/principal/{subject}`) shows who, role and state, the
  actions as buttons (each confirmed in a dialog and answered on the same page; a forbidden one
  stays disabled with the reason; while a step-up is due they open "Confirm it's you"), the
  deactivated banner with Reactivate, the SSO / proxy note, Details (with "Edit name and email"
  and "Set a password instead" for local accounts), the live sessions with End session (never on
  your own) and the last 10 audit entries with a link to the audit log. A generated password has
  its own page with Copy, the expiry and Done; it is in the page exactly once and never cached.
  The setup link's page shows step 1 of 3 ("Open the setup link": where the link is, no form)
  and, with the token, the first admin's account form (step 2); the wizard is step 3. Copy
  buttons can read their value from an element (`data-copy-from`, `copy.js`).
- System settings (design v2, N17-N20; the v2 settings page renders them, P8):
  - **Log level** (`LOG_LEVEL`: debug, info, warn, error) is a settings-page field in the
    Logging group (General tab) and applies live through one shared `slog.LevelVar`; every change
    is audited. Set in the environment, it locks the field.
  - **MQTT broker as Host / Port / TLS**: `config.SplitMQTTURL` / `config.JoinMQTTURL` map
    `MQTT_URL` to the three parts and back (round-trip tested). The settings form and the
    connection test accept `mqtt.host`, `mqtt.port`, `mqtt.tls` and compose `MQTT_URL`
    (`tcp://` or `ssl://`); `MQTT_URL` stays the stored value.
  - **`/metrics` master switch** (`METRICS_ENABLED`, default on): off, `/metrics` answers 404;
    applies live.
  - **Fleet default device password** (`DEVICE_DEFAULT_PASSWORD`, write-only secret, sealed like
    device passwords, never shown or audited in clear): adding a device and accepting a discovery
    proposal take it when the form sends `use_default_password=1` and no password was typed; the
    web user stays Tasmota's `admin`.
- Backend for the design v2 counts and options (core group g5; the pages render them later):
  - N7: one-pass fleet counts for the dashboard tiles and the device chips (Unknown apart from
    Offline, devices behind the newest cached release, devices and rules with policy deviations,
    discovery proposals found but not adopted and address changes for admins) and the device list
    filter `?chip=down|outdated|noncompliant`.
  - N8: the health tracker keeps a two-hour, one-sample-per-minute power history per device from
    the ENERGY telemetry it already receives (in memory only).
  - N6: the firmware cache can list the cached feed without contacting a source
    (`CachedReleases`), and "changes since" helpers list every release between the oldest version
    in the fleet and a selected one, with breaking sections flagged.
  - N14: the audit log filters by action family (`action=device.*`), a search box (`q=`, also
    device and SSO user names) and `since=today`, in the store.
  - N15: bulk commands and restarts take `wait=1` (wait up to 5 s for each response),
    `serial=1` (one device at a time) and `parallel=1|3|5` (devices at a time). A capped
    transaction runs against its own cap instead of the fleet-wide limit; migration 0024 stores
    the options on the jobs so a resume and a retry keep them.
- What's new, backend (design v2, N5; the page and the avatar dot follow with the shell's
  user menu): the binary embeds `CHANGELOG.md`, `internal/changelog` parses it into releases,
  sections and items (code, bold and safe links only, everything else stays text), and the user
  preference `news.seen` keeps the newest release a user has seen. Only released versions of the
  running binary are accepted for it; a user who never opened What's new counts one new release.
- About lists more components (design v2, N13): the release feed (last check, stale or failing
  without the fetch error, which can contain a source URL), the database (SQLite version and
  size) and background jobs (running and queued), after the MQTT broker and the backup decoder.
- Theme choice per user (design v2, D11): Light, Dark or Auto (follow the system, the default),
  stored as the user preference `theme` (`POST <base>/prefs/theme`) and rendered by the server,
  so pages open in the chosen theme without a flash.
- `VISUAL_REGION=shell make visual`: compares only the header and the crumbs of every reference
  at the foundation tolerance (summary and `e2e/test-results/visual-shell/report.html`); the
  `logo` references are compared as 6x crops of the top-bar brand.
- `make visual-parity BASE=<commit>`: renders every page without clicks with binaries built from
  BASE and with the current ones and compares the screenshots with tolerance 0, for packages that
  must not change how pages look.
- `make visual`: pixel comparison of the app's pages against the 1312 reference screenshots (prototype render v6) of the
  second Claude Design prototype (design v2 port, work package V0). It renders each page with the
  design fixture, masks the values the app cannot reproduce, compares with `pixelmatch` and writes
  `e2e/test-results/visual/report.html`. The prototype, the SHA-256 manifest of the references and
  the fixture generator are in `design/v2/` and `design/tools/v2-fixture.mjs`. Not part of CI; the
  screens of later packages fail until they land.
- `tasmofleet-devenv -design-fixture`: starts the prototype's 60 devices (names, chips, firmware,
  power state, TelePeriod, PowerOnState) and its release list; `POST /fixture/offline` on the
  control server takes the devices the prototype shows as offline away after the app has seen them.
  `simulator.DeviceSpec` gets `Power` and `Settings` (initial relay and flash state), and a device can be
  `PowerOff`ed (will published, HTTP port closed).
- Add devices, design v2 (port package P6): "Test connection" on the Add manually tab probes the
  address with the typed password and says what answered, without adding the device; "Add…" on a
  discovered device opens the form filled from it (`/devices/new?proposal=<id>`, a scan result
  `?address=&port=`). The Discovered tab selects several new devices: `POST
  <base>/discovery/proposals/accept` and `.../dismiss` with `id=<id>:<version>` repeated (admins;
  each accept is still tied to the version shown; an id without its version is refused as out of
  date and nothing is added). The import result has "Download report"
  (`GET <base>/import/tasmoadmin/report.csv`, the last result of the session, for 10 minutes).
- Add a local user on its own page (design v2, P9): `GET <base>/users/new` (admins) with Name,
  Username, Email, Role, and the choice between a generated one-time password and a typed initial
  password (typed twice; a mismatch is refused). A refused form comes back with what was typed.
- Delete a device on its own page (design v2, N2, package W4): `GET <base>/devices/{id}/delete`
  lists the device's backups (count, total size, newest, kept ones) and asks whether to keep them
  (preselected) or delete them too, with one acknowledgement; it posts `backups` and `ack`. The
  device header's Delete and the list's Delete… lead to it; a refused post shows the page again
  with the field error. Works without JavaScript.
- The device overview draws the power of the last 2 hours as a bar sparkline next to "Watts now"
  (design v2, N8, package W4): one bar per 5 minutes, a gap where no reading came in.
- What's new page (design v2, N5): `GET <base>/whats-new` (every signed-in user) shows the
  changelog of the running binary per release, with the installed release and the releases new
  since the last visit marked; opening it records the visit. The avatar has a dot while releases
  are unseen, the user menu says "N new since last visit" and the phone drawer lists What's new.
  About's "What's new in x.y.z" and its Changelog link open this page instead of GitHub.
- Release page (design v2, N6): `GET <base>/firmware/releases/{version}` (operators) shows a
  release's whole notes with the breaking section in a box, the files this fleet needs with their
  cache state and Prefetch / Retry, and how many devices are on each older version; `?view=since`
  lists every release between the oldest version in the fleet and this one, newest first, with
  breaking changes flagged. It reads only the release list already in memory (an empty state
  before the first check, never a request upstream); an unknown version is a 404. The firmware
  page's selected release links to it ("Release page", "Changes since <oldest>").

### Changed

- The last html/template pages are gone (design v2, Z1): result and refusal notices ("Nothing
  to retry", "Command started", ...) and the transaction retry page are templ pages built from the
  design system's page header, card, banner and buttons; the device list's rows, phone-card cells
  and the cells the event stream pushes are templ components (`cells.templ`), so a live update
  renders exactly like the first paint. `internal/web/templates/`, the `legacyContent` bridge,
  the unused v1 device form, `jump.js` and the `.jump` rules are removed.
- Design v2, the Updates pages (port package P4): `/transactions` shows the running transactions
  as cards with their progress and the history newest first (id `TX-0143`, what it did, devices,
  result, who and when); it is no longer a sortable table with a Columns chooser. A transaction's
  page has the state, the progress bar with done / in progress / queued / failed, one row per
  device with its versions, state and progress, and the log of one device beside it (`?job=`; a
  failed device opens first). "Cancel remaining" and "Retry failed (N)" are the header buttons
  ("Retry failed" waits until no device runs); cancel and retry of one device and "Copy log" sit
  in the log's head (without JavaScript, "Cancel remaining" opens a page with the dialog's
  words). A "Make compliant" transaction shows each device with its changes and their
  state. `/updates/new` is the prototype's "New firmware update": 1 · Devices (opened without a
  selection it ticks every outdated device that can be updated; a "Select" menu replaces the
  quick-selection buttons), 2 · Release as cards (uploads included, older releases folded),
  3 · Options (all visible: "Back up each device before flashing", policy, power after update,
  delivery, variant) and a sticky Plan with the devices, firmware files and an estimated time.
  "Review update" opens step 4, the preview with "What happens", "Start update" and "Change"
  (back to the picker with the same choice).
- **breaking** The transaction page refreshes through one route, `GET <base>/transactions/{batch}/live`
  (`?job=` keeps the open log); `GET <base>/transactions/{batch}/summary`, `/table` and
  `/jobs/{job}/row` are gone. The picker posts `backup=1` with `backup_shown=1` ("Back up each
  device"); `skip_backup=1` is still the preview's field. `POST <base>/updates/new` also takes the
  release and the options (the preview's "Change"). Saved column layouts and sort orders of the
  transactions, update picker and update preview tables are ignored (the tables are gone).
- Add devices, design v2 (port package P6): the screen has the prototype's four tabs, Add manually
  (`/devices/new`), Discovered (`/discovery`), Network scan (`/discovery/scan`, new route) and
  Import from TasmoAdmin; the old "Scan and discovered" page is split. **Breaking** for scripts
  that read the pages: the add form shows the address, the fixed web user `admin` and the web
  password; name, port, tags, notes and the OTA exclusion moved under "More options" (still
  saved). The fleet default password (N20) prefills only a form whose address was typed, never one
  filled from a discovered device. Discovered lists address changes (Dismiss / Apply) above the new
  devices; "Show ignored" lists ignored devices in the same table with "Restore". The scan tab
  shows the latest scan with All / New / Known / Ignored filters and a next step per row; the scan
  history list is gone. The import has one "Choose files" (or drop) for devices.csv, MyConfig.json
  and the key file (form field `files`, sorted by file name; the three named fields still work),
  uploads on choosing, and shows the plan as Create / Merge / Reject rows. The Add devices
  tables have fixed columns: no Columns chooser and no sort links any more. The "Add devices"
  badge counts address changes too.
- Discovery proposals carry the first sighting (`FirstSeen`, the "First seen" column).

- Design v2, system pages (port package P8). **System → Settings** is the prototype's screen:
  "Settings" with its ten tabs (General, Network, MQTT, Discovery, Devices, Backups, Firmware
  source, Metrics, Sign-in, Advanced; `?tab=`) and one card with Discard and Save settings. The
  fields are the real settings in the prototype's components: log level as Error / Warn / Info /
  Debug, the broker as host, port and Use TLS (N18), on/off settings as checkboxes (the metrics
  switch as a switch), the sign-in method as radio cards, the default device password under
  Devices next to the fixed web user admin; the Discovery tab starts with the state of MQTT
  discovery (listening, off, waiting for or missing a broker) and "Open Discovered". Test
  connection and Test discovery show their result next to the button. A sign-in change that needs
  a confirmation opens "Confirm it's you" over the tab: the password for a local account, the
  identity provider for a single sign-on account, back on the same tab with what was typed.
  **Audit log** gets the prototype's header ("kept for N days" from the audit retention) and filter
  row: search, actor, action family, period and result, applied as soon as a select changes (a
  Filter button without JavaScript); the events stay a table with sorting, Columns chooser, inline
  details and the pager. **The first-run setup** is the prototype's last step, "Basic settings",
  on a page of its own: the steps indicator, device networks, broker host, port, user, password and
  TLS with Test connection, and "Scan these networks for Tasmota devices when I finish"; Finish
  setup saves, ends the setup and starts that scan.
- **breaking**: `GET <base>/settings/system` takes `?tab=` (general, network, mqtt, discovery,
  devices, backups, fwsource, metrics, signin, advanced) instead of `?section=`; on/off settings
  are posted as a checkbox `v.KEY=true` with the marker `b.KEY=1` (without `v.KEY` the setting is
  saved as off). `POST <base>/setup/save` is gone: `POST <base>/setup/finish` takes the setup's
  settings (`v.DEVICE_NETWORKS`, `mqtt.host`, `mqtt.port`, `mqtt.tls`, `v.MQTT_USER`,
  `v.MQTT_PASSWORD`) and `scan=1`.
- Design v2, the style guide (port package F4): `/styleguide` is the prototype's UI-states screen
  (inline banners, toasts, the live connection indicator) followed by the gallery of every
  component, and it is open to every signed-in user (it shows sample data only), no longer to
  admins only.
- The navigation is the same for every role: operators see the whole menu (System included) and
  each page enforces its own permission; a group button leads to the first page the role may open
  (an operator's System button opens the style guide).
- The confirmation pages without JavaScript (delete or restart a device, delete a backup or a
  policy rule, cancel a transaction or an update) use the design v2 confirmation look.
- Row menus built with `ui.RowMenu` close on Escape and on a click elsewhere, one open at a time;
  opening a dialog closes the user menu as well as row menus.
- Database migrations 0021 and 0022 (design v2, N1/N2): `backups` gains `kept`, `trigger`,
  `trigger_ref`, `device_name` and `device_mac`, and `backups.device_id` becomes nullable with
  `ON DELETE SET NULL` (the table is rebuilt; the rows keep their ids). New table `backup_attempts`.
  Deleting a device no longer removes its backup rows by cascade: the caller decides. A device
  delete without a choice (the v0.10 dialogs) still deletes its backups, and now also their files,
  which used to stay on disk.
- **breaking** Policy rule order is precedence (design v2, N3): when two enabled rules check the
  same setting on the same device, only the higher one is evaluated there, so a device is no longer
  flagged for two contradicting values. Rules can be disabled (`"disabled": true` in the
  POLICY_RULES format): a disabled rule is kept and versioned but flags nothing. The editor
  preview counts each rule with precedence and returns the overlaps (conflicts with the number of
  shared devices). New form routes (admin, CSRF, version-checked like save):
  `POST <base>/policy/rules/order` (`order=2,0,1` or `idx` + `move=up|down`),
  `/policy/rules/duplicate` (a disabled copy below), `/policy/rules/enable`, `/policy/rules/disable`
  and `/policy/rules/template` (the four starter templates as Advisory rules). The editor form
  accepts `state=enabled|disabled`; without it a saved rule keeps its state. The policy page UI
  for these comes with the page port.
- **breaking** Design v2, the policy page (port package W1, N3 + N21): `<base>/policy` is the
  prototype's page and holds the rules editor. Rules are cards in their stored order (the order is
  the precedence) with a drag handle, Disabled badge, Required / Advisory, the devices they flag now
  and a menu (Duplicate, Enable / Disable, Move up / Move down, History, Delete…; selecting a card
  opens its editor, so the menu has no Edit); the editor
  has the Enabled switch, "Applies to" (chip families, tags, devices), Setting and Expected value,
  Exceptions, Note, "When a device deviates" (Advisory / Required) and the change note; the live
  preview names each device the open rule flags with its value and where the value came from; the
  version history compares a version with the current rules and reverts after a confirmation. A
  banner names every conflict ("… wins for N devices they share"). Without rules the page offers
  the four templates. The live-check status line shows the interval and the last run, "Paused by
  … at …" with Resume (N21), or a failure with Retry now, and keeps Run now. "Make compliant (N)…"
  opens a dialog with the exact commands of every device with a Required deviation read live;
  without JavaScript it posts to the plan page. **Advisory rules are no longer part of Make
  compliant.** The device table of the old page (status chips, Source column, per-device Make
  compliant) is gone: a rule's flagged devices are in its live preview. Import and export moved to
  `<base>/policy/io` (Export tab with Copy and Download .json, Import tab with a preview of the
  changes before anything is saved).
- A local sign-in without "Keep me signed in on this device" now sets a browser-session cookie
  (no `Max-Age`): the session ends when the browser closes. The server-side idle and absolute
  limits are unchanged. Re-issued sessions after a password change are browser-session cookies too.
- **breaking**: an unknown `TASMOFLEET_LOG_LEVEL` now stops the start (it used to fall back to
  info silently). A `LOG_LEVEL` in the environment now locks the settings-page field; the Unraid
  template and the env example leave it empty, so remove `TASMOFLEET_LOG_LEVEL=info` from an
  existing container to choose the level on the page.
- Design v2, the shell (port package F3): a new top bar with the TasmoFleet mark and name, the
  four groups Dashboard, Devices, Updates and System as links, and below it the group's pages
  (Overview; All devices, Add devices with the number of discovered devices, Policy; Transactions,
  Firmware; Settings, Audit log, Users, About, Styleguide). The bar and the sub-navigation keep
  their content in the same 1640 px column as the page, their backgrounds span the window.
  "New transaction" left the menu (the Transactions page has the button). Every page has
  breadcrumbs. On phones a header with the page title opens a navigation drawer; without
  JavaScript the menu button leads to the new page `/nav`. The user menu (avatar) holds the name
  and role, a Light / Dark / Auto theme switch, "Account & password" for local accounts and Sign
  out. Pages with live updates show the connection in the top bar (Live, Reconnecting…, Offline)
  and a banner while the stream is down, with "Retry now" once it is offline. Confirmations after
  a redirect ("Saved.") are toasts bottom right that fade after 4.5 s (an inline banner without
  JavaScript). The About page is the first page built in templ: version card, components (MQTT,
  backup decoder) with their state, and the effective configuration as before; the four summary
  cards (sign-in mode, public URL, base path, poll interval) are gone, the values stay in the
  configuration table.
- The product is written "TasmoFleet" in the UI, the page titles, the README header and the web
  app manifest; identifiers (repository, binary, image, `TASMOFLEET_*`) stay lower case. The logo is
  the design v2 mark, a white ring on a blue tile: favicon, app and home-screen icons, Home
  Assistant add-on icon and logo and the README wordmark are regenerated from
  `assets/branding/logo.svg` by `design/tools/brand-icons.sh`; `logo-dark.svg` is gone (the mark is
  the same in both themes). `internal/web/static/SHA256SUMS` now also lists the icons, and a test
  checks every listed checksum.
- The event stream asks browsers to reconnect after 4 s (`retry:` field), the delay the
  "Reconnecting" banner shows.
- Design v2, templ toolchain (port package F1): every page now renders through a templ page shell
  (`internal/web/ui`); pages that are not ported yet run inside it through a bridge that executes
  their `html/template` content block. No visual change: the page markup is the same as before
  and the pages render pixel-identical to the previous build at 1280 and 375 px in light and dark
  (`make visual-parity`), apart from antialiasing noise at the corners of one select.
  New runtime dependency `github.com/a-h/templ`; the generator is pinned in `tools/go.mod`, its
  output is committed, `make templ` regenerates and `make templ-check` (in `make check` and the CI
  lint job) fails on stale generated files. Inline script and style stay forbidden (lint rule, source
  test, render test over every page family).
- Design v2, step 1 of the port (tokens and fonts): the colours, shadows, radii and fonts are now
  the Claude Design v2 prototype's, in light and dark. The accent turns from teal to blue, surfaces
  and controls get rounded corners, section rules become hairlines, and the text is set in Geist
  (Geist Mono for addresses and versions) instead of Archivo, self-hosted, versioned and preloaded
  like Archivo was. Layout and markup of the pages are unchanged; the shell, components and tables
  follow in the next steps.
- The phone layout now applies below 720 px (45rem) instead of 768 px, as in the prototype.
- Design v2, tables and bulk bar (port package F5). Tables show from 720 px (was 1152 px) with all
  their columns; a table wider than the page scrolls sideways inside its frame instead of hiding
  columns. Columns can be pinned to the left or the right edge ("Pin left" / "Pin right" in the
  Columns chooser, saved per user with the layout) and pinned columns stay in view while the table
  scrolls. In the device list Name is pinned left and Actions right for good (a saved order that
  moved a column in front of Name now starts with Name), Power is pinned right by default (it can
  now be unpinned and moved); in every other table the row actions stay at the right edge. A pinned column is never narrower than its content, and an open row menu is never cut
  off by the scrolling table. The Columns chooser is the prototype's: drag a row by its grip or use
  ↑ ↓, Pin left, Pin right, "Reset to default". On a phone the device list shows cards (name with
  status dot, address, chip and firmware, status, uptime and signal, power and row actions) with
  "Select all N"; ticking a card ticks its table row and the other way round. The bulk bar of the
  device list is the prototype's: a dark bar centred at the bottom on a wide screen, a panel with
  the actions in a grid of three on a phone, without the folded "Actions" menu. It offers Update
  firmware (new: opens New transaction with the selection; `POST <base>/updates/new` takes the
  selection as a form post), Send command, Back up, Restart, Power on and Power off (were "Switch
  on" / "Switch off") and Clear (was "Clear selection").
- Design v2, device list (port package P2). The page is rendered with templ (`devices.templ`;
  `templates/devices.html` is gone) and follows the prototype: the header counts "N online · N
  offline · N unknown" with Scan network and Add devices; one toolbar with the search ("Filter by
  name, IP, chip or firmware"), the filter chips with their counts (All, Offline / unknown,
  Outdated, Not compliant), and Chip and Tag selects listing what the fleet has with a count each
  (they replace the Filters fold; the Health select is gone, a `health=` link still filters); on a
  phone a Sort select. The table's columns are the prototype's: Status (badge with dot and "N
  deviations"), the new MQTT · HTTP column (live, event `reach-<id>`), Firmware with an "update"
  badge, Chip (written ESP32-C3), Address, Uptime, Wi-Fi as a signal bar with dBm, Last restart
  (now shown by default), Power as a switch per relay (disabled while the device does not answer)
  and the "⋯" row menu: Open, Open console, Back up now, Restart…, Delete…. The menu's Settings
  entry is gone (the device's Settings tab), Delete… leads to the delete page instead of a dialog,
  and Back up now answers with a toast ("Backup of Attic Dimmer saved") instead of a line above the
  table. "Select all shown" is gone (the header box selects). Phone cards show "Update available",
  "N deviations" and "MQTT down". Empty fleet: "No devices yet" with Scan network, Add manually and
  Import from TasmoAdmin; no match: "No devices match these filters" with Clear filters. The
  Uptime of an offline device reads "seen 3h ago" (the Status column no longer shows the age). The
  device table keeps the prototype's pixel widths: it scrolls sideways when they need more room,
  and a saved layout keeps the table's size, so widening a column no longer squeezes the others.
  A row menu entry that acts in place (Back up now) closes its menu. The list reloads 0.5 s after the
  last device change of a burst, so a change within 2 s of another is no longer dropped.
- A device that never answered (no identity yet) is listed and counted as Unknown, also after its
  polls failed; Offline means it answered once and stopped (device list, its counts, the dashboard
  and the live updates; the health tracker, alerts and bulk skips are unchanged).
- Design v2, the device pages (port package P3). Every device tab shares the prototype's header:
  the name with its state, the address / chip / firmware line ("Update to x.y.z" when a newer
  release is cached; it opens New transaction with the device), the buttons Power (devices with one
  relay; it asks for the opposite state, never a blind toggle), Restart (a confirmation dialog), Back
  up, Edit and Delete (admins), the offline banner with "last seen", and the tabs Overview,
  Settings, Console, Backups, **Drift** (a tab now; was "Changes since the last backup" on the
  Backups page) and **Replace device** (a tab; was a button). The tabs no longer show the backup
  count; the trail names the device on every tab. The **overview** has the cards Identity,
  Power & energy (watts now, today / yesterday / total, a switch per relay), Network, Wi-Fi (with a
  signal quality bar) and Policy compliance (the device's deviations and "Make compliant…" for
  admins); hostname, gateway, SSID, BSSID and channel come from the newest backup snapshot. The
  state changes and transactions follow below the cards; the backup list is on the Backups tab
  only. The **console** is the prototype's dark panel: the history from the audit log, the reply
  of the command just sent as a line, quick commands Status 5, Power TOGGLE, Status 11,
  TelePeriod 300 and Restart 1 (sent at once and checked like a typed command), and Clear, which
  hides the lines shown so far (`?since=`). **Settings** is one form card with Reset and "Save to
  device"; a field whose value deviates from a policy rule says what the rule expects. **Drift**
  says how many settings changed since the last backup, "Show all" (`?all=1`) lists the unchanged
  ones too, with "Back up live state" and, for admins, "Restore backup…"; a reload within 15 s
  shows the settings read last instead of refusing. **Edit device** is one form card with Delete
  device…, Cancel and Save changes; title and trail keep the saved name while a refused form is
  corrected, and the failing field gets the focus. **Breaking:** `POST <base>/devices/{id}/backup`
  without `from` (the header's Back up) answers with a redirect to the device tab named by `back`
  and the flash "Backup taken." instead of the backup card fragment.
- Design v2, the firmware page (port package P5). The releases are cards (version, "Latest",
  release date, how many devices run it); the newest three are listed, the others behind "Show N
  older releases". The selected release (`?release=`, the newest by default) shows its notes as an
  excerpt, "Release notes" (the whole notes and where the fleet stands, in a dialog; `?notes=1`
  without JavaScript), "Update N devices…" (devices that can take it) and the files this fleet
  needs with chip, devices, size and state: Cached, Prefetch per file (new on this page:
  `POST <base>/firmware/fetch`), Fetching… while a prefetch runs, "Download failed" with Retry.
  "Prefetch for my fleet" is a link above the files. The cached files are a list (file, release
  and size, short checksum, date, Delete in a dialog that says who still needs the file; a file a
  transaction needs links that transaction instead). Uploads are listed among them by their label;
  "Upload custom firmware" and the drop zone below the list open the upload form (a dropped file
  lands in it). The header says where releases come from and when they were last checked, with
  **"Check for new releases now"** (was "Refresh index"); its answer is a toast ("No release newer
  than x", "New release x found"), a failed check shows the reason and that cached files still
  work. The delete confirmation and the prefetch progress pages are restyled the same way.
  **Breaking:** the releases table, its Columns chooser and sort order, the "Prefetch a file" form
  and the cached-files table with checkboxes are gone; "Delete N unused…" replaces "Select unused"
  and the selection (same confirmation page), and the saved layouts and sorts of the firmware tables
  are no longer used.
- Design v2, backups and replace (port package P10). The Backups tab lists one row per backup
  run (dump, snapshot and decoded copy together; the tab counts runs, not files) with the failed
  attempts among them (reason, Retry), the retention line ("Keeping the last 5 backups and anything
  kept · 3 will be pruned at the next run", "Change retention" for admins), the total, Back up now,
  and per row Newest / Kept / Pruned at next run, Keep, Download, Restore… and Delete (one dialog,
  confirmation page without JavaScript). Ticking rows shows the selection bar: Compare (two rows)
  and Delete selected (a confirmation page naming the count and whether the newest goes, `GET
  <base>/devices/{id}/backups/delete`). "Recent restores" stays below the list. The comparison
  (`<base>/devices/{id}/backups/diff`) has Older / Newer pickers, Side by side / Unified, "Show all
  N settings" (unchanged settings too, secrets never shown) and "Restore this backup…" (the older
  one); without parameters it compares the two newest backups, and two ticked rows (`id` twice)
  compare their runs. The restore confirmation shows what will change, the three steps and the one
  acknowledgement ("Restore and restart"); the progress page shows the four steps (backed up,
  uploaded, restarted, verified) with the failed one marked, and after a failure "Retry restore"
  plus, when it failed after the upload, "Restore the HH:MM backup" (the backup taken before). Replace device is a device tab: the new
  device from the discovered list (or typed in), what is always copied, a preview of what moves and
  what does not, then "Start replacement…" with the one acknowledgement. The device tabs gain Drift
  and Replace device (the header button is gone).
- **breaking**: "Delete backups older than a date" is replaced by the prototype's "Prune now…": it
  deletes the runs the next pruning would remove (the ones marked "Pruned at next run"), from a
  dialog or `GET <base>/devices/{id}/backups/prune` (now without a date); `POST
  <base>/devices/{id}/backups/prune` is gone, the deletion goes through `POST
  <base>/devices/{id}/backups/delete`.
- Spacing uses pixel-named tokens (`--sp-N`); the old `--color-success`/`-warning`/`-info`/`-divider`
  names, the generated colour ramps and `--space-*`/`--radius-*` are gone (no aliases).
- Page padding follows the container width (`clamp(12px, 2.4cqw, 32px)`), 12 px on a phone.
- Design v2, the dashboard (port package P1): "Fleet overview" with six stat tiles (Online,
  Offline, Unknown, Firmware behind, Policy deviations, New devices found), each a link to the
  filtered device list or to discovery, the fleet health bar, "Needs attention" (discovery
  proposals with Ignore / Add and Dismiss / Apply, then offline, never answered, refusing the login,
  silent and non-compliant devices, at most eight) and "Update transactions" (the newest as a card
  with its progress, the next three as rows, transactions named "TX-0001"). Buttons Add devices and
  New update; without devices the page shows only the empty state with Scan network, Add manually
  and Import from TasmoAdmin. The dashboard is live: its body reloads when a device changes state, a
  device is added or removed, or a transaction moves (the event stream sends a new `state` event only
  on a real state change).
- The fleet counts (dashboard tiles, health bar and "Needs attention") call a device that never
  answered "Unknown" also after its first polls failed; "Offline" there means seen once, then lost.
- Design v2, bulk actions (port package P7). Every action of the bulk bar (Send command, Back up,
  Restart, Power on, Power off) opens the prototype's scope page: the selected devices in the order
  of the list, each with its address and state, "Will be skipped" for the ones that are not
  online, "×" to take one out and "Change selection"; the options of the action in an aside with
  "N devices run · M skipped" and the run button. Send command: the command box, "Wait up to 5 s
  for each response" (checked by default) and "One device at a time", then the preview as before
  (the token binds command and devices). Back up: "Label (optional)" and "Keep these backups
  (never pruned)"; it starts at once. Restart: the scope page is the confirmation ("Relays switch
  off for a few seconds"), with "Devices at a time" (3, 1, 5) and "Save settings before
  restarting" (always on: `Restart 1` saves). Power on / off: "All relays" or "Relay 1 only
  (POWER1)", relays already in the wanted state are left alone, and Power on can stagger the
  devices by 1 s. The result is a new page, `GET <base>/devices/bulk/result/{batch}` (admin), with
  Succeeded / Failed / Skipped tiles, a row per device with its answer, "Retry failed (N)", "Audit
  log" and "Back to devices"; it follows a running transaction live. Power on / off shows the same
  result page directly and a toast. **Breaking:** the bulk forms no longer take `scope`: the posted
  devices are the scope and "All devices" is gone from these pages (select all in the list
  instead); Power on / off asks first for a single device too; the restart scope page posts to
  `/devices/bulk/restart/run` directly (no "Continue" step); the backup form starts with `run=1`.
- Design v2, sign-in, account and users (port package P9): the sign-in page, the forced password
  change and the short messages are the prototype's sign-in card (brand above, "TasmoFleet x.y.z ·
  host" below); sign-in shows "session expired" and the attempts left after a wrong password, and
  "Keep me signed in on this device" is checked by default. The account page shows the profile of
  every signed-in user (name, e-mail, role, how they sign in) and, for local accounts, Change
  password and the profile form; the user menu and the phone drawer link it for everybody. The
  users page is the prototype's list (you first, then by name) with a "⋯" menu per user: every
  action is confirmed in a dialog, a forbidden one stays visible, disabled, with the reason, and
  while a step-up is due an action opens "Confirm it's you" first. Reset password generates a
  one-time password. The users list has no column chooser or sort links any more.
- Error pages (design v2, P9): not found, not allowed and switched-off features are the
  prototype's error page in the app shell (code line, title, explanation, the address or setting,
  two ways on). A page above your role says which role it needs and what yours allows.

### Fixed

- A retry page with nothing to retry offers "Back to the transaction" instead of ending in a
  warning, and its breadcrumb is "Updates / TX-0007 / Retry" like the transaction's own page.
- The device console wraps a long reply line (Tasmota JSON) inside the panel instead of running
  past its edge, where the end was cut off.
- A partly read policy fleet no longer shows a bare 0: the dashboard's "Policy deviations" tile
  adds "n not checked yet" to the known count, and the device list's "Not compliant" chip shows
  "–" while nothing was read and "3+" (with the number of unchecked devices as its tooltip) while
  part of the fleet was.
- The device list's MQTT · HTTP column is wide enough for both channel states by default (150 px,
  so Uptime still shows in full beside the pinned Power column at 1280).
- `/devices?health=offline` marks its own "Offline" chip as the active filter (it removes the
  filter again) instead of showing "All" as selected over a narrowed list.
- New update: the "Select" quick selection menu opens as a popover over the device list instead of
  in the flow of the "1 · Devices" head, which grew to the menu's height.
- Text no longer breaks in the middle of a word when there is room: the stylesheet used
  `word-break: break-all` in 36 places and now uses `overflow-wrap: anywhere` (hyphens, spaces and
  slashes first, a cut inside a token only when it does not fit). File names, URLs, host:port and
  device JSON replies get break points of their own (`<wbr>`), so "tasmota-minimal.bin.gz",
  "mqtt.example.internal:1883" and a bulk or console reply no longer split inside "bin", the port or
  "Boot". A device name with no break point at all still wraps inside its box. A new layout guard
  (`w`) fails when a word of up to 200 px is cut over two lines.
- Phone: the bulk scope rows have their padding back (the backups page's `.bk-row` rule had
  overridden them), the backup actions line up with the text above, the device card meta breaks
  between address, chip and version, discovery cards no longer show bare dashes, the sign-in
  footer is centred, the audit filters are two selects per row, the transaction list reads id,
  result, title, devices and actor in one order, the device picker of a new update gives names a
  line of their own, the style guide shows two stat tiles per row, the backup comparison fits
  without scrolling, a banner in a narrow card stays inside its border, replace-device checkboxes
  keep their size beside two-line labels, and a dialog that scrolls inside itself shows a shadow
  on the cut edge.
- Phone: a scrolling tab row (device pages, Add devices, system settings) opens with the current tab in view (`tabs.js`) instead of cutting it off.
- Replace device: the locked, checked boxes are readable in the light theme.
- Audit log on a phone: rows have left padding and show the user and the target again.
- Phone touch targets: the older-release chips on the firmware page and the theme switch in the drawer are 40 px.
- Users: a user known only by a subject hash shows the address as its label; the hash is in a tooltip, not page text.
- Back up / Send command / Restart scope pages: the option checkboxes ("Keep these backups", "Wait up
  to 5 s", "Save settings before restarting") no longer collapse into a one-word column that
  overlapped the "devices run" line (a backup-list class `.bk-check` was shadowing them). New layout
  guard `o` fails any text that runs out of its box or is drawn over other text.
- Release-notes dialog on a phone keeps its title and Close in view and scrolls only the notes, with
  edge shadows showing that there is more; device settings' Save bar no longer squeezes its hint into
  a narrow column; New update's device names and the style guide's toasts break at hyphens and spaces
  instead of in the middle of words.
- The dashboard's "Policy deviations" tile shows "–" ("not checked yet") instead of 0 while no
  policy value is known: live values are kept in memory only, so after a restart nothing is known
  until the first background read (spread over up to 10 minutes, none while the live check is
  paused) unless a decoded backup has the setting.
- CI: `TestEditorFieldsAreStateful` (simulator, integration lane) no longer flakes with
  "connection refused" on `/dl`. Hostname, FullTopic and the MQTT settings queue a restart ~2 s
  after they change; when one of them came last in the (random) loop, the backup download could
  meet that reboot gap. The download now waits for the device to run again, as the commands
  already did. Test-only; no production change.
- The progress bar of a transaction (`ui.SegBar`) leaves the devices still waiting empty instead
  of stretching the done, failed and running parts over the whole width.

- System settings: "Reset to default on save" works with JavaScript on; the field's own value no
  longer makes the save fail with "a setting cannot be set and cleared at once" (P8).
- An open dialog dims the whole window: the reserved scrollbar gutter stayed light (P8).
- A typed choice between radio buttons (log level, sign-in method) survives a step-up: the draft
  kept the last radio of a group instead of the chosen one (P8).
- The "device networks" link under a network scan's ranges opens the Network tab of the settings,
  where they are edited, instead of the first tab (`?section=` no longer exists; P8 review).
- "Confirm it's you" still names the change after a wrong password ("Changing the sign-in
  settings") instead of an empty "Changing" (P8 review).
- The "changed field" accent in the Settings forms was never drawn (it used an undefined colour name).
- Dragging a column edge in the device list while a device event reloads the list no longer
  drops the resize: the drag goes on in the new table and the width is saved.
- A push upload (`/u2`) whose image source ends early no longer sends the multipart closing
  boundary: the device answers on that boundary without waiting for the announced length and
  could have taken the cut image.
- Simulator: a rebooting device stops accepting connections before it reports the reboot phase,
  as Tasmota does, and lets requests already in flight finish; before, a request in that gap was
  accepted and then cut.
- UX polish from the design-v2 review (desktop): the audit log describes bulk backup and command
  starts in words ("2 devices · 1 left out") instead of `keep=false label=`; a subject nobody
  recorded reads "Unknown user" everywhere, not "user 9f86d081"; retention periods are written
  as "30 days" (the audit retention hint says what 8760h is); the "Release notes" heading no
  longer doubles the notes' own "Changelog"; the upstream link of a release is "Upstream release
  page" (the card has an own "Release page" link); "Newer than this release (0): none" has its
  space; the artifact delete page explains why it lists more devices than "still need this
  file"; the device settings selects use one "0 (off)" style; "Web user" everywhere.
- Breadcrumbs and navigation: the delete-backup page names the device, "Nothing to prune"
  leads back to the backups, a missing device or transaction keeps Devices or Updates
  highlighted, and the answer to "Run now" no longer shows "Policy / Policy".
- A bulk run page says "running for" (not "took") while it runs, and a device row that is still
  running shows its step, not the finished reply.
- Layout: the device overview cards stack in columns without holes, the drift action bar reaches
  the page edge and Restore is primary only when a setting changed, the edit device form is as
  wide as the add form, the policy rule editor stacks Setting and Expected value, the discovery
  table gives the MAC column its width, the transactions history has column headers, the
  Options of a new update line up in one column, the console's input placeholder is readable
  in light mode, and a view-only Users list shows a Details link instead of a menu of disabled
  actions.

### Breaking
- The policy rules page `<base>/policy/rules` is gone (no redirect): the rules are edited on
  `<base>/policy`. The export moved to `<base>/policy/io/export`, the import to
  `POST <base>/policy/io/import`; `<base>/policy/rules/diff` is replaced by `<base>/policy?v=<id>`.
  The `?status=` filter of the policy page is gone with its device table. (W1)
- The account page moved from `<base>/account/password` to `<base>/account` (no redirect); the
  password form still posts to `<base>/account/password`. A user who must change the password is
  sent to `<base>/account`.

### Security

- Accepting a discovery proposal with the fleet default device password needs a host that proves
  the proposal's MAC without credentials; otherwise the accept is refused (accept without the
  default and set the password in the device edit form). The default password no longer goes to
  an address only a sighting vouched for.
- The setup token (setup step 1) is checked and spent in one step, so two concurrent submits with
  the same link cannot both pass; a form error gives the link back. The setup page sends
  `Referrer-Policy: no-referrer`, so the token in its URL is not passed on in Referer headers.
- The setup link (setup step 1) only works for the `admin` account the first start created. An
  account created later under the same name (by an admin with a one-time password, or by the CLI
  with a forced password change) also waits for its first password; the link already in the
  container log, or a fresh one printed at the next start, could set that account's password and
  sign in as admin. The first-start account now carries a stored mark (migration 0025) that only
  the first start sets and every password change, reset or disable clears; the creator field is
  not used for this, since an SSO or proxy subject can be any string. Accounts that existed before
  this release do not get a setup link; they sign in with the generated password as before.

## 0.10.1 (2026-10-22)

### Fixed

- Row "Actions" menus work again in tables with a saved column layout (desktop). A saved layout
  switches the table to fixed column widths, and its cells cut off what does not fit: the Actions
  button was cut in half and the opened menu was hidden inside its cell, so a click showed nothing.
  The row-actions cell no longer cuts off its menu, and the checkbox, Power and Actions columns
  are never narrower than their controls at any window width, whatever share the saved layout
  gives them (the other columns share the rest). Present since 0.9.0.
- Switching pages no longer makes the page jump or flash. Since 0.10.0 (no reserved scrollbar
  gutter) a page with a scrollbar laid out 15 px narrower than one without, so the top bar and the
  page moved on every switch between a short and a long page (main pages and device tabs alike).
  The scrollbar gutter is reserved on every page again; the price is the empty strip right of the
  top bar on short pages with classic scrollbars, which 0.10.0 had removed. The
  Archivo fonts were the only static files without the version hash: every page revalidated them
  and painted in the fallback font first. The stylesheet now links them with the hash (cached
  immutable, the stylesheet's own hash follows its fonts), and the page preloads the font.

## 0.10.0 (2026-10-22)

### Changed

- Design system spec, Deviation 7: all pages are ported and `knownLayoutIssues` is empty; the
  deviation now lists the pages that still use the legacy CSS block.
- Stylesheet clean-up, no visible change: the old app.css token aliases (`--bg`, `--muted`,
  `--brand`, `--overlay`, ...) are gone and every page rule uses the design-system tokens, and the
  unused `.subnav` rules are removed. The "page rules" block stays: its remaining rules are all in
  use. A test keeps the aliases and `.subnav` from coming back.
- Updates and transactions polish. The update preview counts refused devices as "N refused" (red)
  instead of hiding them in "skipped", its phone rows are labelled lines (Plan, File, Power
  restore, Note) with no bare "-" and no power-restore line for devices that are not updated, and
  the push refusal for an ESP8266 no longer quotes an internal backlog id. New transaction warns,
  as soon as Push upload and an ESP8266 (or unknown) device are both chosen, that those devices
  would be refused. The selected-row highlight and accent bar of the picker are painted by the whole
  row on a phone. A transaction's target, starter, start time and duration are labelled facts in
  the live summary, so the duration keeps counting; logs opened with "Expand all logs" stay open
  while the cards refresh and the button says what the next click does; a job whose cancel is
  pending shows "cancel requested", loses its Cancel button, and the log reads "Cancelled by adam"
  instead of "Cancel cancelled by adam". The bulk Send command preview names the consequence of
  `Restart 1` and its button says "Restart N devices". The retry form uses the full card width.
  Two-column tables (Device, Result) have no Columns chooser.
- One section component: a folded section (Firmware Prefetch a file and Custom images, the Setup
  steps and any other closed card fold) keeps the card frame and its legend title instead of
  turning into a bare 2px rule, so every section on Firmware, Policy and the rest looks alike.
- Without JavaScript, "Delete backup", "Delete rule", "Cancel transaction" and "Cancel update" now
  open a confirmation page (the same wording and the same POST as the dialog) instead of doing
  nothing, like Delete and Restart device. With JavaScript the dialogs open as before.
- Users, System settings and Audit polish. Users: the row Actions menu holds action items only
  ("Make admin" / "Make operator", a labelled new-password field, Disable, Delete); your own row
  says "You" instead of an empty cell; the State chip and its note read as one value; long names
  wrap; the table keeps its Columns chooser and sorting before step-up; the "Proxy"/"SSO" chip no
  longer repeats the Method column; the New local user form has the same width as the other forms;
  the step-up text names creating users. Change password has a breadcrumb trail and no repeated
  card title. System settings: Save returns to the tab that was saved, the Network tab has its
  legend, settings set by the environment use the label pattern of the editable ones, the
  read-only table has no Columns chooser, and "Device names in metric labels" sits in a Metrics
  group instead of Audit. Audit: local users carry a "Local" chip like the other kinds, the MQTT
  and SSO tests have readable names, an actor nobody recorded reads "Unknown user", and "Clear the
  filter" appears once and is a 40 px button on a phone.
- The style guide shows every overlay open and in the flow (top bar group and user menu, phone
  menu, row Actions menu with its danger item and the upward `up` variant, Columns chooser,
  dialog), the device tabs from the real `devtabs` partial with their phone select and a disabled
  tab, and the real bulk bar actions (new `bulkactions` partial, shared with the device list) once
  inline and once folded into the Actions menu. Static examples carry `data-static`, which the menu,
  tooltip and table scripts ignore.
- The dashboard keeps its "Recent transactions" card when there are none: "No transactions yet.",
  with "New transaction" for admins. Its table has no Columns chooser any more.
- Form errors read as sentences and sit at the input they are about: the sentence under the
  input, the input marked invalid (border, `aria-invalid`, `aria-describedby`) and focused, no
  repeated page banner. One shared pattern (`fieldError` / `fieldInvalid` template helpers,
  documented on the style guide and in the design system) now covers the password change and
  forced-change pages, the Users page (create keeps the typed username and role, shows the
  12-character hint; a wrong step-up password marks its input), the policy rule editor (a save
  without a setting says "Choose a setting." once, at the Setting select, instead of
  `policy rules: rule 6: invalid command`), the device form (name, address, port, tags, notes),
  and the System settings values.
- Device pages share one header (name, state badge, Replace device, trail) on Overview, Settings,
  Console and Backups; the tab names the section, so "Settings of ..." and "Backups of ..." headings
  are gone. A device that never answered or is held by a running operation has "Back up now"
  disabled with the reason next to it, the Transactions card is always there (with an empty
  state), and the Overview says why nothing works yet. The Identity card is swapped as a whole
  by "Refresh identity": a failed probe is the error callout (a sentence, not a Go error chain)
  and keeps the button, a good one updates the rows. When the Settings cannot be read the page
  offers Try again and Back to the overview, and the Settings tab keeps the current-tab style.
- Backups tab: with no backup the compare, "Changes since the last backup" and prune controls are
  gone and the empty state has a "Back up now" button; config dumps get a Note column; restore
  jobs show their state as a badge. The delete dialog of the newest backup uses the danger
  callout in sentence case. Restart confirmation uses the primary button (red is for delete).
  An unchecked chip family on Restore and Replace is a warning, not an error. Replace no longer
  shows the "old device still answers" message twice.
- Phone: drift, comparison and replace tables keep their Before / After and Old / New labels;
  the Console reply keeps its tokens whole and scrolls sideways. Replace: select, row headers,
  row rules and card gap fixed; Settings: the General card lines up with Options. The MQTT row no
  longer repeats its own label.
- A failed manual backup says what to try ("The device did not answer ...") instead of showing the
  Go error chain; the details go to the server log. "Select at least one device." starts with a
  capital letter and ends with a full stop.
- Dialogs: opening one from a row's Actions menu closes the menu behind it. The dialog of a
  running transaction or job names its dismiss button "Keep running" instead of a second
  "Cancel". The row menu's "Restart" reads "Restart..." like "Delete...", and the style guide
  shows it. "Expand all logs" is a secondary button, not a link-looking ghost button.

- Add devices, manual tab: a failed probe says what is wrong in a sentence under the field it is about
  (Address, or the web password for a device that asks for one) instead of the Go error chain
  ("Probe failed: tasmota: address outside ..."). "Add without probing" is offered only where it can
  work (not for an address outside the device networks, which the inventory refuses anyway). The button
  reads "Add device", and it is disabled, with its reason, while no device network is configured, like
  Start scan.
- Add devices, scan tab: a refused range ("10.99.0.0/24 is outside the device networks.") sits under the
  Ranges field, which is marked invalid, and the proposals table keeps its sort links after the failed
  request. "Show ignored" is a filter above the table (checkbox plus Filter) instead of a link below it.
  Ignore and Add device stand side by side. A finished scan says "Nothing found.", not "Nothing found so
  far."; with no device network the page no longer says "Start a scan" and names the setup once. The scan
  history is a table instead of a bulleted list.
- Add devices, import: the plan counts rejected lines as skipped and marks a row whose address is outside
  the device networks as skipped, with the reason, instead of listing it as Create and failing on apply.
  With nothing to apply the page says "Nothing to import." and offers no "Apply import". The result uses
  the plan's badges (failed in the danger tone), keeps host:port in Address and gives each failure one
  sentence instead of the inventory error chain. The info button of the key file sits inline after its
  label, and so does the size hint of devices.csv. A row that only has a warning (unusable port,
  password without key) stays planned and is no longer counted as skipped as well; a failed import line
  no longer shows the update hint. The scan Ranges help links to the device networks only when one is set.

### Fixed

- Long unbreakable text (a device name like `AtticHeaterPlugWith...NeverHasABreakAtAll`, an error chain) wraps inside its box instead of widening the page. One shared rule covers the device header and its back link, the retry form's "Last result" text, the bulk back up / restart scope list, the row dialogs (title, text and Close stay inside the frame), and the firmware release dialog. Device names on phone cards and in firmware lists wrap in full instead of being cut or ellipsized. An e2e test opens the key pages at 375 px with such a name and fails on any sideways scroll.
- Pre-v0.10.0 recheck regressions. The rule preview callout in the policy editor keeps room for
  its icon on a phone instead of drawing it over the first word. A row Actions menu opens up or
  down by the space in the viewport when it opens (a short Users list no longer opens upward over
  the top bar); the server's "last two rows up" stays as the fallback without JavaScript. The
  bulk Back up and Restart scope choice has a legend ("Devices") like Send command. The
  transaction header (state, "n of m done", counts, duration, Cancel) re-reads itself every
  3 seconds while the transaction is active, like the job cards, instead of waiting for a throttled
  event. Logs opened with "Expand all logs" stay open when one card is swapped twice at once.
- Policy pages (QA v0.9.0): on a phone a rule card has the rule name as title, the setting as
  subtitle and labelled lines (Expected, Applies to, Severity, Findings); the Note of the rule
  editor has a visible label at every width; the import check result (or refusal) sits inside the
  Import JSON section; the rule preview is green only when every device was judged and none
  would be flagged; "Add rule" is not a second primary button while the editor is open; Policy
  rules uses the same breadcrumb root as Make compliant (Devices > Policy > ...); the version
  diff heading is short enough for a phone; an empty rules or versions list shows no table
  header, "0 deviating" is neutral and Check now is disabled without rules or devices; the
  Source column stays on one line and "checked ... ago (live)" is shown once on a desktop;
  Make compliant labels "Commands" and "Not changed" on a phone and its footer stays inside the card.
- Phone folds on a desktop (QA v0.9.0): section titles that are phone folds (Setup steps 2 to 4,
  Policy Rules, Import JSON) are plain framed card titles from 48rem up, without a chevron and
  without the unframed look of a closed fold; closed card folds on a phone are at least 40 px apart.
- Standalone pages (sign-in, forced password change, access denied) follow the system dark
  mode: their `<html>` lacked the `data-theme-auto` marker. The sign-in button of the SSO
  link now has the width of the form buttons, and the forced password change names the
  signed-in account and aligns its "Sign out" button.
- The sign-in page says why it appeared ("You have been signed out." / "Sign in to continue.").
- "Access denied" names the role the page needs and the one the account has; an account
  without any role gets a standalone page instead of an app shell whose links lead back
  to the denial; that page carries a "Sign out" button so the account can be switched.
- A missing device or transaction shows "Device not found" / "Transaction not found" with a
  link to the list; the "switched off" pages make "Open the system settings" the primary
  action for admins and show `FIRMWARE_BASE_URL` as code. Only a missing device or transaction says so: a missing
  backup of an existing device gets the generic "Not found" page.
- CI: the mqttbus tests no longer flake ("timeout waiting for message", e.g.
  `TestNonDefaultFullTopic`). The test helper returned once the connection was open, before
  the broker had processed the bus's SUBSCRIBE, so messages published right after were lost
  on a loaded runner. It now waits for the broker to hold the subscriptions. Test-only; no
  production change.
- CI: the phone height check of the policy page no longer fails by a few pixels on a slow runner.
  The background policy reads start at random times after the app starts, and each deviating
  device they found made the page taller; the check now runs with those reads off and measures
  the unchecked page its budget is set for. Test-only; no production change.
- Tables keep their sort links and layout on the page a POST answers with (Firmware after
  "Refresh index" or a failed fetch, the policy rules after a refused save, discovery after a
  bad scan range). A table with no row shows no header, select-all box, sort links or Columns
  chooser; the confirmation table of "Delete selected files" and the key/value tables have no
  Columns chooser, and the confirmation row leads with the file name.
- Firmware: the folded "Older releases" table sorts and is laid out like the newest releases
  (one Columns chooser); its meta counts the releases a device can still be updated to ("7,
  none needed by this fleet" instead of "7, 7 needed"). Device names in the release dialog,
  the release detail and "Used by" never break inside a name (a name longer than the line is
  cut with an ellipsis and has its full text as a title) and look like the links in the
  tables. The "recommended" badge is the same in the table and the dialog, a file held by a
  transaction says "Transaction #N" and its disabled checkbox stays visible, and the release
  and chip line of a cached file stays on one run.
- The top bar, its rule and the dialog backdrop reached only 15 px short of the right edge on
  pages without a scrollbar (reserved scrollbar gutter, removed).
- The flash message no longer covers the top bar and the menu button.
- Phone menu: "Sign out" is left-aligned like the other rows, the signed-in user's name keeps its
  case, and the last row has room below it.
- Text such as "HTTP scan" no longer loses the space after a capital letter (font kerning).
- Dashboard zero tiles and the sort icons in table headers are readable (contrast); the
  select-all box lines up with the row boxes; a stat card's title no longer draws a card-coloured
  box above its rule.
- The divider above the buttons of a confirmation page ended 4 px outside the card on phones.
- An action row or form right below a table has a gap; a checkbox label that wraps keeps its box
  on the first line; the device health line no longer breaks inside "wifi 36%".
- Native controls follow the flat design: progress bars (scan, prefetch, restore) are full width
  with the accent colour, job progress bars and rule chips are square, job cards have no shadow,
  a failed job's bar is a full danger bar, file inputs have a button like the others, and an
  unticked confirmation checkbox is outlined instead of struck by a red bar.

## 0.9.0 (2026-10-21)

### Added

- Devices: bulk "Back up" and "Restart" (admin) for the selected devices, or all (chosen
  explicitly, never implied). Offline devices are left out and listed. Both run as a
  transaction like "Send command": a result per device, "Retry failed", audit entries
  (`backup.bulk.start`, `command.bulk.start`). A backup starts at once; a restart sends
  `Restart 1` through the command service (policy, device lock, audit) after one
  confirmation that says the devices go offline briefly.
- **Experimental: not yet tested on a real device; default delivery stays pull; ESP8266
  refused.** OTA push upload as an optional delivery per transaction (update form, "More options",
  "Delivery"): tasmofleet uploads the image to the device's `/u2` instead of the device
  downloading it. ESP32 only; ESP8266 is refused with the reason until it was tested on real
  hardware (F-9). Guard runs before the upload, a lost reply is probed and never resent, and
  the device's OtaUrl is not touched. Default stays the download. New migration 0020.
- New design (the nav-patterns port of the Claude Design system): flat Modernist look with
  local Archivo fonts, light and dark themes that follow the system ("Auto"), a phone menu
  panel, breadcrumb trail, device tabs (Overview, Settings, Console, Backups), one Actions menu
  per table row, a fixed bulk bar with a "Clear selection" button, and a "Columns" button label.
  Applies to the Dashboard, Devices, Device detail and Firmware pages, and to System settings,
  Audit, Users, About, Setup, Login, Change password and the error pages; the other pages follow
  in the same release. Binding reference: `docs/spec/design-system.md` and the admin page
  `/styleguide` (every component and state, light and dark side by side).
- Devices: "Restart" per device (admin) in the row menu. It sends `Restart 1` through the same
  command service as the console (policy, device lock, audit) after one confirmation.
- Devices: a confirmation page for "Delete..." (`/devices/{id}/delete`), the no-JavaScript
  fallback of the dialog. Firmware: "Start update..." in a release's menu opens the update form
  with that release selected.
- Per-device Prometheus gauges while "Device labels in metrics" is on: `tasmofleet_device_up`,
  `_uptime_seconds`, `_wifi_rssi_percent`, `_wifi_signal_dbm`, `_power_watts`,
  `_energy_total_kwh`, `_energy_today_kwh`, `_energy_yesterday_kwh`, `_relay_on` and
  `_last_seen_timestamp_seconds`, labelled `device` and `id` (plus `channel` or `relay`). They
  come from the health cache, never from a device query during a scrape. The Grafana dashboard
  has a panel row for them; see `docs/metrics.md`.
- Devices: bulk "Send command..." (admin) for the selected devices, or all when none is
  selected. One command line, a Backlog included; the preview lists who gets it and who
  is left out (offline), names the consequence of destructive commands, and the send runs
  as a transaction with a result per device, "Retry failed" and an audit trail. The scope
  is chosen explicitly ("Selected devices" or "All devices", nothing preselected). Credential
  commands are not sent in bulk.
- Settings: "Keep the stored password if the broker changes" for the MQTT password.

### Changed

- System settings: seven tabs (Network, MQTT, Firmware, Discovery, Backups, Sign-in, Advanced)
  instead of a side list; "Test MQTT" and "Test discovery" are buttons of the settings form
  (they use only the broker or issuer fields and never save anything); the container-only
  settings are a fold in Advanced. Old `?section=` slugs fall back to the first tab.
- Audit: details open inline in a log box instead of a tooltip, "Clear the filter" sits in the
  filter bar, an empty result shows an empty state, and the pager is the same at every width
  (Previous, position, Next, page sizes; no First/Last/page numbers).
- Users: "Manage" is now "Actions"; proxy-header accounts are shown as display-only rows
  ("Managed by the proxy"); the step-up text mentions ending sessions only where it is offered.
- Setup: step 4 explains the prefilled networks and "Finish the setup" follows the last step.
- The forced password change has a "Sign out" button.
- Sign out moved into the account menu of the top bar. Proxy-header accounts show their name
  as plain text with a Sign out button. Updates menu: Transactions, New transaction, Firmware.
- Firmware page: "Refresh index" is the page action; the three newest releases are listed, the
  rest sit in an "Older releases" fold; the cached-files table drops the Release, Chip and
  SHA-256 columns (release and chip are under the file name). Delete confirmations use dialogs.
- The device list no longer reloads while a row menu or dialog is open or devices are selected.
- Design port, second part: the device pages (Settings, Console, Backups, Restore, Replace, Delete old
  backups, Changes), Add devices (scan, import), Policy, Policy rules, Make compliant, New
  transaction, Preview, Transactions, Transaction detail, Retry, Switch on/off and Send command use
  the new patterns: breadcrumb trail, device tabs, one Actions menu per row, the shared bulk bar,
  confirmation cards, dialogs instead of browser confirms (cancel transaction, cancel update, delete
  backup, delete rule). Policy: "Check now" is the page action, "Select all deviating" plus the bulk
  bar's "Make compliant" replace "Make all compliant" and "Make selected compliant". The update picker's
  "Select none" is the bulk bar's "Clear selection". The console history is a log box.
- Folds that hold what people come for (backups, state changes, transactions, cached files, policy
  rules) are open on a desktop and closed on a phone; secondary ones stay closed.
- Tables with fewer than two rows get no Columns button.
- Console and command policy ("admins may do everything", 2026-10-21): an admin can send
  any Tasmota command: no allowlist, no commands that are refused for everybody
  (Backlog, Rule, Timer, Script, sensor and driver commands, credential commands,
  SetOption85, Reset, WifiConfig, Upgrade, OtaUrl, ...). Destructive and lock-out commands
  (Reset, WifiConfig, firmware, Wi-Fi off, WebServer other than 2, IPAddress, Ethernet,
  Ap, Module, Template, credential commands, Restart other than 1) ask once and name the
  consequence; routine commands no longer ask. The confirmation does not need the command
  typed again. A manual `Status 0` is allowed with a warning; tasmofleet's own code still
  never sends it. Operators stay at read, power and Restart 1.
- TasmoAdmin web import needs no step-up and is available in header mode; the file names
  and counts of every check are audited.
- Changing the address or port of a device with a stored password is allowed for admins:
  the edit form warns that the password goes to the new address. Discovery offers "Update
  address" for such devices. An answer without the password is no proof of identity for a
  password-protected device, so the move always needs a second confirmation for exactly
  that address, which is audited as "MAC unverified"; another MAC stops it. The scan, MQTT
  announcements and imports still only propose.

### Breaking

- `TASMOFLEET_IMPORT_WEB_HEADER_MODE` is removed (no shim); the web import is always on.
- `Backlog`, executors and credential commands are no longer refused: rely on the admin
  role, not on the policy, to keep them away from people.

### Security

- Credential arguments are never logged: audit, history, transaction pages and errors show
  `Name ***`; a Backlog is logged part by part with each part redacted.
- A command transaction is sent as its creator's current role, checked before each step:
  a demoted, disabled or removed creator fails the step (audited); a retry is admin-only
  and runs as the retrying admin. A step found mid-send after a restart is not sent again.

### Fixed

- Devices: bulk "Back up" and "Restart..." are now buttons in the bulk bar and its Actions menu
  (admin, with the feature flags). The scope page pre-selects the picked devices and names
  them, and the backup result links "Transaction #N" as the primary action and lists the
  left-out devices with their reason (they are not part of the transaction).
- Policy: with a device's Details open the list no longer scrolls sideways on a phone; the
  findings wrap inside the card.
- Device page: a running backup says "A backup is running" instead of "being updated"; the
  retry page of a backup transaction has its own text and keeps the failure reason visible on a
  phone; long device names wrap on the bulk restart confirmation.
- Health: devices added by another process (for example `tasmofleet import` while the server
  runs) now show live state within one 5 s tick instead of after up to 30 s; the tracker checks
  SQLite `PRAGMA data_version` per tick and reloads the inventory only when it changed.
- Tables: a live list update no longer undoes a column width set a moment earlier (the save
  was still in flight) and keeps the keyboard focus on the resize handle.
- Tables: a live list update that arrives right after a resize handle got the keyboard
  focus, before the first arrow key, no longer drops the focus.
- Devices: the list no longer freezes while devices are selected: a live update is applied and
  the selection and bulk bar count are restored; with a row menu or dialog open the update
  waits until it closes.
- System settings and Setup: a connection test that ran and failed (MQTT, SSO discovery) is
  shown in the error notice, not the green success one.
- Device settings: Apply without a change shows "Nothing changed." as a neutral notice (HTTP
  200, not 422). The form that comes back is read from the device again, so fields the device
  did not report stay "not reported" and the change counter starts at zero.
- Device console history: the "Proxy" / "SSO" tag no longer overlaps the actor name (the
  hanging indent of the log was inherited into the tag).
- Device Backups tab: the page lists the snapshots (new "Snapshots" section) and the tab
  count is config dumps plus snapshots, so count and list agree.
- Phone row cards no longer hide what identifies a row: a discovery proposal keeps its name on the
  first line and shows MAC, topic, source and seen as labelled lines with Ignore and Add device on
  their own line; policy device names wrap instead of being clipped; cached firmware files show
  "Installed on" and "Needed by" before you delete one.
- Transaction pages: a job card no longer sets the page width. The one-line log summary
  (a whole `Status 2` reply) and the status line are ellipsized inside the card, so Cancel,
  Retry and the badges stay on screen at 1280 and 375 px.
- Devices list: a long device name wraps inside its cell instead of pushing the Actions column
  past the content edge; device chips in the rule editor wrap long names too.

## 0.8.1 (2026-10-21)

### Fixed

- Visual QA of v0.8.x, shared components and the dashboard, firmware, update and
  transaction pages:
  - Menus, row menus, the Columns panel, tooltips and dialogs have one border (3:1
    against the page) and one shadow that shows in the dark theme as well.
  - The automatic tooltip no longer copies the text of a menu or a button row into a
    bubble over the open menu, and the "Show or hide columns" bubble stays away from the
    menu it opened.
  - Cards keep their content 0.75rem below the title on the frame. On a phone a collapsed
    section is one header row with the chevron at the right, not an empty frame under a
    title; the chevron sits on the middle of the line (folds, About, transaction logs).
  - Row actions and the "Show older releases" switch stay inside their rows and centred;
    "Delete..." no longer covers the size of a cached file; checkboxes sit on the middle of
    their row; the Dashboard link of the phone top bar is centred like the other items.
  - Phone cards print no label line for an empty cell. The page scrollbar is always painted,
    so bars and backdrops reach the right edge.
  - Dashboard "Recent transactions": one row per transaction (target, device summary,
    state, started by) instead of one row per device; the target is the release alone.
  - Firmware: fleet counts say "N of M known devices can be updated (K unknown)"; unknown
    hardware is never "to update"; every release shows a figure that is true for it ("22 to
    update", "3 on this release", "5 older", "all 26 newer"); long changelog links wrap
    instead of widening the page; the Cached files table fits its card; lists of devices
    never start a line with a comma.
  - Update preview: a skipped device is neutral and not an error, "0 to update" has no
    success tick, and the power restore line no longer contradicts itself. The retry page
    words "nothing to retry" as a warning. Breadcrumbs sit under the title on every page.
  - Transaction page: the meta line breaks between its parts, queued jobs have a label on
    their log fold, device names are no longer cut in the middle of a word, the Cancel
    buttons of the phone cards line up, a log box shows whole lines.
- e2e: layout guards for the whole signed-in UI (controls inside their row, no overlaps,
  centred labels, no empty collapsed frames, equal button heights, legend spacing,
  delimited overlays) run in `ui-pages.spec.ts` at 375 and 1280, light and dark.
- Visual QA of v0.8.x, part B: the device, policy, audit, user, system and setup pages:
  - A path that does not exist, a 403 and the update pages of an installation without a
    firmware base URL answer with the app's own page (navigation, "Go to the dashboard",
    an explanation) instead of Go's plain "404 page not found". The "Import from TasmoAdmin"
    tab is hidden when the web import is off (header mode).
  - Devices: long names are cut with an ellipsis and never run under the power toggle at
    375; an empty inventory says "No devices yet" and offers Add devices; a filter without
    a match offers "Clear the filters"; an empty list is no table, so the header no longer
    stops short of the card edge; "Select all" is disabled with no rows.
  - Device detail: the Backups table has a real Actions column; on a phone a backup card
    keeps its note and puts Download, Restore and Delete on a line of its own; the header
    buttons share the width; "Compare and restore" is a button. "decoder outdated" is
    explained, the "Pick a date" page has a form, and the drift page offers "Try again".
  - Restore, replace and delete-old-backups: the confirmation box shares a line with its
    sentence, a missing tick is shown at the box, advisories start with a capital, a running
    job is no green success box and has a progress bar, the "Open the device" link no
    longer wraps in a column.
  - Policy and rules: Note folds and status folds no longer overlap their neighbours at
    375, rule cards show their findings, names are cut with an ellipsis, a device with
    unknown values says so next to "N to fix", the empty rule table says "No rules yet", the
    doubled parentheses are gone, the editor's device list shows that it scrolls.
  - Audit: full timestamps and device names fit at 1280; the phone cards keep their user and
    target readable; an empty filter result offers "Clear the filter" and has no pager.
  - Users: the table uses the page width, "Last login" says "ago", the Actions column and its
    label appear only when actions are unlocked, and the step-up text lists what it unlocks.
  - System settings, setup, About: the save hint has room beside the button, the setup
    steps say "(optional)", the Finish button is outside step 4, and the effective
    configuration prints lists and durations for people and tells unset from masked values.
  - Second QA pass: disabled buttons no longer show an endless busy spinner (only a running
    request does); "Switch on/off selected" is off without a selection; the per-release Fleet
    cell on Firmware names unknown devices ("3 to update · 7 unknown"); the drift throttle
    notice is info, not a success box; policy rule rows no longer jump when their menu opens
    and version notes stay readable at 375; menu entries are 40px touch targets; "Old
    credentials come back" and "The old device must stay switched off" are warnings.
  - Users: an admin can no longer disable, delete or lower their own local account (the
    server refuses, audited as denied; the own row has no Manage menu). Demoting the last
    admin stays forbidden.

## 0.8.0 (2026-10-20)

### Added

- Users page (`/users`, System menu): every principal in one list. Local accounts and the
  SSO and proxy-header users seen at sign-in, each with display name (with the
  anti-spoofing suffix when names clash), method, role (for SSO the role of the last
  sign-in, which still comes from the IdP groups), last login and live sessions. The list
  needs no step-up; only changes do. Local accounts are managed on the same page (create,
  role, reset password, disable, enable, delete). SSO users get "End sessions", which
  signs out exactly that user. OIDC admins confirm with the IdP step-up and may now manage
  local accounts; header admins have no step-up and see the page view only (migration
  0016 `principal_logins`: role and last login per principal).
- Replace device: after a hardware swap, a wizard starting from the dead entry puts its
  newest (or a chosen) backup onto new hardware and lets the entry take over the new
  MACs in one transaction. Candidates are devices that are online, inside the device
  networks and bound to no entry; the chip family must match (final), a firmware major
  change needs a tick, the preview shows what the dump sets (hostname, static IP, MQTT
  topic and client, Wi-Fi names, template), and it refuses while the old device still
  answers. The new hardware is backed up first; name, tags, notes and history stay, the old
  MAC goes into the audit (migration 0017 `replace_device`). See docs/security/restore.md (R18-R22).
- Backups: delete a single backup (admin, confirmation, audited; the device's newest backup
  gets an extra warning) and "Delete older than..." per device with a preview of what goes.
  Restore is a button in the backup list and on the device page.
- Policy page: "Make compliant" per device and "Make all / selected compliant" (admins only).
  A preview lists the exact commands per device. Only deviations whose value was read live
  become commands; unknown and backup-only values are listed and skipped, and the ESP32
  exception of the rules applies as on the page. No confirmation tick: preview, then Apply.
  It runs as a transaction like an update (progress per device, "Retry failed", the
  parallelism setting): the commands go to each device batched with Backlog, followed by
  exactly one SaveData (none extra when the batch contains SaveData or a setting that
  restarts the device, which saves by itself), no restart otherwise. Afterwards the values
  are read back and the policy page shows the new state. Devices that are locked or have
  a running job are skipped with the reason. Every change is audited
  (`policy.fix.start`, `policy.fix`). Needs the same job engine as updates, so it is
  available when the firmware server is configured. Migration 0018 `policy_jobs` (job kind
  and commands).

### Changed

- Restore needs no password step-up any more. One confirmation names every risk (restart,
  replaced configuration, old credentials, firmware major change, unchecked chip family);
  the live checks run before the page is shown, and a refused restore shows no form.
  **breaking** for scripts: `POST /backups/{id}/restore/stepup` is gone.
- Restoring a device whose MAC was never stored reads the live MAC and stores it instead of
  refusing.

### Breaking

- `/admin/users` moved to `/users`, the account actions to `/users/local/{name}` and
  `/users/stepup`; the old routes are gone without a redirect.

### Security

- Local account management is no longer restricted to local admins: an OIDC admin who
  re-authenticated at the IdP (5 minute step-up) may manage local accounts. Every change
  still needs a step-up, the "last admin" rule and the forced password change are
  unchanged, and header principals cannot change anything.

## 0.7.0 (2026-10-20)

### Added

- Custom firmware: admins upload their own `.bin` or `.bin.gz` on the Firmware page (label,
  numeric version, build name). The chip family comes from the image header; safeboot,
  factory and full-flash images, unknown chips, corrupt files and files over 4 MiB (ESP8266)
  or 8 MiB (ESP32) are refused. An upload is identified by its SHA-256, so it may carry the
  same version as an upstream release (devices fetch it from its own URL); the same file
  twice is refused. Uploads appear on the update page as "custom: label (version)", never
  preselected, and pass the same chip-family guard; ESP8266 uploads are staged through a
  minimal image borrowed from upstream. Upload, refusal, use and delete are audited
  (migration 0014 `firmware_custom`, `target_custom` on jobs). See
  docs/security/custom-firmware.md.
- Sorting for the remaining tables: transactions, firmware releases and cached files,
  policy devices, settings, rules and versions, discovery and users. Typed keys (versions
  as versions, addresses as addresses, times as times), a header link per column, the
  choice saved per user like on devices and audit.
- System menu: "Users" entry for admins.
- (i) help: long explanations move into a tooltip next to a short label or intro line.

### Changed

- One page header on every signed-in page: the title row is as tall as a button, so the
  title no longer moves between pages; the way back (crumb) sits under the title.
- One card component: the title sits in the frame (like a fieldset legend) on every
  card; plain sections of the dashboard, setup, backups and users are cards now.
- Badges: the icon has its own space before the text everywhere.
- Tooltips explain and never list data: at most 160 characters (also enforced on the
  server by the `tip` template function for titles that carry data), one or two short sentences; the "Refresh identity"
  button lost its tooltip.
- Shorter intro lines and labels with the rest in an (i) tooltip (transactions, console,
  import, firmware prefetch and delete, retry, setup, device form, about).
- Device settings page: full width, one card per group in two columns from 72rem, a compact
  label/value grid with the help in an (i) tooltip, sticky jump links and Apply bar. The intro is
  "Applied immediately and saved to the device once."
- Update options: one select "Power after update" (Restore lights, Restore lights and relays,
  Leave as is) replaces the two power checkboxes, so the meaningless combination cannot be sent.
  The variant override and the discovery address change no longer need a confirmation checkbox
  (the preview shows the variant, the server probes the MAC itself).
- Cached files: row checkboxes with "Select unused" and "Delete selected" replace "Delete all
  unused"; the device column is now "Installed on" (informational) and the new "Needed by" names
  the pending or running transactions. A needed file cannot be selected, and the server refuses it
  too. Uploads are never part of a bulk delete. **Breaking:** the routes `GET/POST
/firmware/unused/delete` are gone (`POST /firmware/delete-selected` replaces them).
- Audit log: Result is a status badge only (ok, failed, refused); the new Summary column (at most
  40 characters, full text in the tooltip) is written per action type and never repeats Action or
  Target; actions and targets read as names. Starting a transaction is audited (`transaction.start`).
- Policy page: summary chips (deviating, compliant, not checked) at the top filter the device
  table; then Rules, then Devices. A device whose only issue is a value that could not be read counts
  as "not checked", not "to fix".
- Page intros are one line and labels at most six words on the settings, update, firmware, audit,
  policy, system settings and discovery pages; the explanation moved into (i) tooltips.
- System settings: the button name and the scopes no longer need a step-up; everything that decides
  who signs in or with which rights still does. Values typed before a step-up come back afterwards.
- Principals: users are shown by the display name captured at sign-in ("alice" with an SSO badge,
  "admin (local)", system actors with their own badge, the full subject in the tooltip) in the audit
  log, the console history, transactions, restores, scans, prefetch runs, custom uploads and policy
  versions. Names that two users share, or that look like a system actor or a local account, are
  shown with the start of the subject; the audit user filter accepts the name and asks when it is
  ambiguous (migration 0015 `principals`).

### Security

- A user cannot pass as "System", "Discovery" or "admin (local)" in the audit log by choosing that
  display name: such names are always shown with a subject suffix (docs/security/system-settings.md,
  docs/security/discovery.md mark the superseded rules).

### Fixed

- Import page: the key-file label points at its file field again, not at the (i) button.

## 0.6.2 (2026-10-20)

### Added

- Discovered list: "Show ignored" with "Un-ignore" for dismissed devices.

### Changed

- Identity rule (spec 11.1): a device is identified by its set of MACs (Wi-Fi plus
  Ethernet where present; migration 0013 `device_macs`, seeded from the existing MAC).
  Discovery, the MQTT known-device index, import, restore and OTA checks match any MAC of
  the set. No automatic path changes the set.

### Fixed

- Discovery no longer treats inventory devices as changed: a stored hostname that resolves
  to the discovered IP is a no-op, a hostname is never replaced by an IP, and an address
  change of a device with a password is a suggestion ("Known device - address changed" with
  "Update address" for admins, or "Dismiss"), not an update. The scheduled scan wrote 255
  refused `device.upsert` audit rows; no-ops now write none and a refused attempt is audited
  at most once per device and day (in memory, so a restart may add one).
- Credentials mean a non-empty password. A web user without a password (all TasmoAdmin
  imports) is loaded as "no credentials", dropped on TasmoAdmin import and when a device is
  saved; the stored `http_user` values are not deleted by the migration. Such devices follow
  DHCP changes again and no longer block moves.
- The Discovered list never lists a device whose MAC is in the inventory as new.

## 0.6.1 (2026-10-20)

### Fixed

- Tooltips: hovering a page no longer copies the whole page text into a tooltip (only cut-off
  table cells get their full text), the browser no longer shows a second, native bubble next
  to the tooltip, and every tooltip is capped at 160 characters. The resize handle is gone
  from the last column and from trailing pinned columns.

## 0.6.0 (2026-10-20)

### Added

- Devices: separate **Uptime** and **Wi-Fi** columns (compact "12h 55m"; signal bars with
  the percentage, dBm on the tooltip) and a hidden-by-default **Last restart** column,
  all in the Columns menu. The Status cell shows MQTT and HTTP per channel ("MQTT ✓ / HTTP –")
  with the last contact of each on the tooltip (the health tracker now keeps the last MQTT
  and HTTP contact separately; the health state itself is unchanged).
- Sortable columns in the shared table component: a header click sorts ascending, the next
  descending (arrow, `aria-sort`). Server-side through `?sort=&dir=` (works without script,
  keeps filters and paging), typed keys (numbers, durations, times, semver-aware firmware
  versions, IP addresses, status by severity), the last sort is saved per user and table.
  On the Devices and Audit tables.
- Columns: double-click (or Enter on) a resize handle fits the column to its widest
  content, capped at 40 characters; saved like a manual resize.
- Columns: reorder by dragging a header or with the move buttons of the Columns menu; the
  order is saved per user and rendered by the server; the selection column stays first and
  Power and row actions stay last. "Reset to defaults" restores the order.

### Changed

- Device settings: the "flash wears out" confirmation and the "Write to flash now" box are
  gone. Applying always ends with exactly one `SaveData` write (so it works with SaveData 0),
  skipped only when a restart or a SaveData change saves anyway; the page says "Saved to the
  device once". The old confirmation decision is marked superseded in the security docs.
- Header tooltips of every table are one short sentence saying what the column shows; the
  other tooltips are shortened. Tooltips have their own layer: at most 32 characters wide,
  normal line height, contrast of at least 7:1 in the light and the dark theme.
- Relative times are compact everywhere ("now", "45s", "5m", "2h", "3d"), the full
  timestamp is on the tooltip; the "seen" prefix is gone where the column says it.
- The Columns button sits in the table toolbar (right end, same height as its buttons), or in
  a bar of its own above tables without a toolbar.
- The Devices "Vitals" column is split into Uptime and Wi-Fi (saved layouts of the old column
  fall back to the defaults for it).

### Fixed

- Phone: on the Devices list the power toggle and the selection box lay partly outside their
  row (a negative margin grew the 40 px target beyond the row). They are centred on the whole
  two-line row now and always inside it; no row action uses a negative margin any more.

## 0.5.0 (2026-10-20)

### Added

- Table columns on every data table: resize by dragging the header edge or with the
  keyboard (arrow keys, Home resets a column), show or hide columns in a Columns menu,
  Reset to defaults. Saved per user on the server (migration 0012 `user_prefs`, endpoint
  `POST /prefs/{key}`), so they follow the user across browsers and sign-in modes; the
  layout is rendered into the HTML, so the page does not jump. The page size of paged
  tables (25/50/100) is saved with them. Desktop layouts only; phones keep the dense list.
- Tooltips: status badges, family and scope chips, relay buttons, update options and
  counters explain themselves (the Stale hint names the configured threshold); a value a
  cell cuts off shows its full text on hover or focus; every system setting has a help text.

### Changed

- UX sweep: one vocabulary ("Transaction", "prefetch",
  "Users"; audit targets read "transaction #3" and device names, as links); plurals agree
  ("1 deviation"); setting labels no longer show raw backticks; durations read "6h", not "6h0m0s".
  Phones: every button, summary, link in a row, toggle and checkbox is a 40 px touch target.
  Users and Change password are part of the app (navigation, sign-out); only sign-in and the
  forced first password change stay centred. Device page: Console, Settings and Edit in the header,
  Delete in its own section. Device settings: jump links, changed fields marked, sticky Apply bar.
  Firmware releases: the changelog is the release link, "Prefetch for my fleet" shows only where
  files are missing. Policy: devices first (those to fix on top, unchecked ones folded), rules below.
  Rules: "Add rule" opens the editor above the table with the name focused. Update: sticky
  "N selected / Preview" bar, shorter policy options, the variant confirmation only with a variant.
  Transaction page: target, starter, start and duration; only non-zero counters. Setup: the
  networks banner is a warning and no longer links to the setup from the setup.
- Phone: tabs (add devices) and the settings sections are one select instead of wrapped
  chips (the links stay without script); a transaction card opens as a whole; cached
  firmware files show a short hash with a copy button; sizes ("6.5 KiB") never wrap.
- The streamed tables (prefetch files, scan results) use the saved column layout too.

### Fixed

- The audit target filter treats `batch:N` and `transaction:N` as one subject, so the
  history from before the rename is found (the rows themselves are not rewritten).
- Setup uses the same device-networks field as System settings; the users table and the
  new-user card have one width; a device's state-change list no longer repeats the source.
- Bulk "Switch on/off selected" were clickable with nothing selected; they stay disabled until a device is
  selected. A failed local sign-in keeps the username and puts the cursor in the password field. A
  backup whose snapshot is incomplete shows a warning badge. Deleting a local user also deletes its
  interface preferences (`user_prefs`).

## 0.4.0 (2026-10-19)

### Changed

- One sign-in page: `/login` shows a "Sign in with <name>" button (OIDC) and the local username/password
  form side by side. The browser goes straight to the IdP only when OIDC is the only method (local
  sign-in off or no local account). The button name is the new sign-in setting `OIDC_DISPLAY_NAME`
  (default "Single sign-on"; any provider). `GET /login/sso` starts the OIDC flow; `GET /login/local`
  redirects to `/login`. In header mode, visitors the proxy did not identify get the local form too.
- Phone density (below 48rem): tables become compact list rows of at most two lines with one
  actions menu per row instead of stacked label/value cards, on devices, device detail, firmware,
  policy, the rules editor, transactions, the update picker, audit, the system settings and about
  pages. Long sections are folded by default (cached files, state changes, update history, backups,
  rule notes, the scope groups of the rule editor, per-device findings, import, setup steps 2 to 4,
  filters, more update options); the audit log shows 25 events with a "show more" toggle. Primary
  actions sit in a bar fixed to the bottom of the screen (device, rule editor, update preview,
  setup). With the realistic seed every page's main view is about two screens or less (was up to
  15). Wide screens are unchanged except as listed under Fixed.
- One pager for paged tables (the audit log now): first, previous, a window of page numbers,
  next, last, "Page X of Y, N events" and a page size of 25, 50 or 100 (`per` parameter, default 25) on wide screens; previous, "X / Y", next on phones. Plain links, filters stay in the URL.
- Audit log: the Details column is a small button; hovering or focusing it shows the JSON as a
  tooltip, clicking expands the full row (the whole Result text and the JSON). Result is the wide
  column and wraps (three lines at most, full text in the title).
- Devices table (1280 px and wider): Name is a fixed column of about 22 characters (longer names
  are cut, the full name is in the title), Status, Family and Firmware are compact, Address is cut
  with a title, Vitals takes the rest and does not wrap, Power has a fixed width.
- `/policy/rules` uses two columns only from 88rem; narrower, the rules and versions tables stack
  (side by side they were wider than a 1280 px window).

### Fixed

- `/policy/rules` was 668 px wide on a phone and 1807 px wide on a desktop: the versions table
  showed a full 40-character identifier (the user of a version, or one quoted in its note) that
  cannot wrap. Such tokens now show their first 8 characters with the full value on hover.
- The navigation row was 5 to 15 px wider than the screen on every phone page (the last group's
  menu button ended at 381 px of 375). It uses less padding and wraps instead of overflowing.
- An opened audit entry could not be closed again: its only child was hidden, so the summary had
  no height. The summary now always shows "Details".
- Fixed action bars on phones no longer cover the last control of the page (bottom padding).
- `/devices` between 768 and 1152 px was up to 280 px too wide (Power column): the Family and
  Vitals columns give way and cells wrap there.
- The overflow e2e check never saw any of this: it compared `scrollWidth` with `innerWidth`, which
  includes the scrollbar (`html` reserves its gutter), so 9 to 15 px of real overflow passed; it
  measured before the fonts loaded, had no 40-character identifiers in its data, and never looked
  inside the navigation row. It now measures against the visible width with zero tolerance after
  `document.fonts.ready`, seeds policy versions with real 40-character hashes, opens the
  navigation menus and fails when the navigation row scrolls inside itself. A new e2e spec
  asserts every page's height budget on the realistic seed.

### Breaking

- **breaking**: the direct admin listener is removed (decision 2026-10-19): `ADMIN_LISTEN`, `ADMIN_NETWORKS`,
  `ADMIN_TLS_CERT_FILE`, `ADMIN_TLS_KEY_FILE` and `ADMIN_TLS_SANS` no longer exist, nor does port 8443 of the
  Unraid template, the `tasmofleet tls fingerprint` command, the `__Host-tasmofleet-admin*` cookies and the
  audit `via: admin-listener` tag. A variable that is still set is ignored with one warning per key at start
  (no crash, data untouched). Break-glass is a local admin on the normal login page plus
  `tasmofleet user passwd`; the only protection is the existing login rate limiting, which no longer has
  an `ADMIN_NETWORKS` exemption. Remove the variables and any firewall rule for 8443. Sessions of the
  removed listener are deleted by a migration; `<DATA_DIR>/tls/admin/` (its certificate) can be deleted. See `docs/runbooks/emergency-access.md`.

## 0.3.2 (2026-10-19)

### Fixed

- With a stored MQTT broker, `/`, `/devices` and `/about` could hang forever after start (v0.3.1): the
  health service asked `mqttbus.Switch.Enabled()` under its own lock while `Switch.Replace` held the
  switch lock across the new connection's retained-message replay into health. `Enabled` is now
  lock-free and the switch no longer holds a lock while calling the connection or handlers.

## 0.3.1 (2026-10-19)

### Fixed

- After a restart, MQTT devices showed Online but without uptime, Wi-Fi and power until their next
  tele/STATE (up to one TelePeriod): a retained LWT made them look healthy, so no poll ran. A device
  that is alive but has no state snapshot now gets one `Status 11` fetch at once (paced to the poll
  concurrency per tick, retries spaced by a minute; never `Status 0`).
- Phone layout: `/policy` (the "Applies to" chips did not wrap), `/firmware` (device link lists in
  the cached-files table made the page twice as wide) and `/policy/rules` no longer scroll
  sideways. A scope that covers a whole chip family reads "ESP32 (all variants)" / "ESP8266" on the
  policy page and in the rules editor. On phones the release list shows the newest three releases
  with one actions menu per row and a "Show older releases" toggle; desktop is unchanged. The
  policy page has a prominent button to the rules editor.
- The navigation dropdown of the last group opens to the left, so it cannot leave the screen; the transactions table scrolls inside its card when it is wider than the space.
- The overflow e2e checks now run on realistic data (27 devices, 10 releases, cached files, the ESP32
  exception rule, audit entries, a retried transaction) and report the widest elements.
- The one-time legacy import now also imports `TASMOFLEET_MQTT_USER` and `TASMOFLEET_MQTT_PASSWORD`
  when they are separate variables (they were dropped, so MQTT connected without credentials);
  the password stays sealed and bound to the broker. `tasmofleet settings import` help now says
  that a running server applies the file only after a restart.

## 0.3.0 (2026-10-19)

### Added

- First-run setup: device networks, MQTT, SSO pointer and a scan; a banner while no device networks are set.

### Changed

- Settings apply live: device networks, MQTT broker, backup retention, update engine and health intervals need no restart.
- The live-check poll interval is the system setting `POLICY_LIVE_INTERVAL` (default 1h, 0 = off).
- Step-up on the System settings page is limited to the sign-in settings; routine settings (device networks,
  MQTT, firmware, retention) no longer ask (proportionate security, docs/security/system-settings.md).

### Fixed

- The one-time import of legacy environment settings is performed by `serve` only; CLI commands
  (`settings`, `user`, ...) no longer set the import marker, so a CLI run before the first start
  (possibly with another environment) cannot silently drop the legacy settings.

### Breaking

- **breaking**: operational environment variables are removed: `DEVICE_NETWORKS`, `FIRMWARE_BASE_URL`,
  `FIRMWARE_SOURCES`, `FIRMWARE_ALLOW_SINGLE_SOURCE`, `FIRMWARE_REFRESH_INTERVAL`,
  `FIRMWARE_KEEP_RELEASES` (was unused), `OTA_*`, `BACKUP_*`, `POLL_INTERVAL`, `TELEPERIOD`, `MQTT_*`,
  `DISCOVERY_*`, `METRICS_DEVICE_LABELS`, `AUDIT_RETENTION`, `POLICY_RULES`. They are settings of the System
  settings page now. On the first start of this version a variable that is still set is imported once into
  the database (only if the database has no value for the key, with a warning per key; credentials in
  `MQTT_URL` move to the sealed password; `POLICY_RULES` becomes the first managed rule version) and
  ignored afterwards, so remove them from the container configuration. `tasmofleet settings export|import <file>`
  moves settings and policy rules as JSON.
- **breaking**: `AUTH_MODE` defaults to `local`, and without `TRUSTED_PROXIES`, certificate or
  `ALLOW_INSECURE_HTTP` the server serves a self-signed certificate (`LISTEN` defaults to `:8443` then). A fresh
  container needs no configuration. The sign-in settings (`AUTH_MODE`, `OIDC_ISSUER`, `OIDC_CLIENT_ID`,
  `OIDC_CLIENT_SECRET`, `OIDC_SCOPES`, `OIDC_*_CLAIM`, `ADMIN_GROUPS`, `OPERATOR_GROUPS`) can be edited on the
  System settings page (step-up, "test SSO"); the environment still wins and then locks them.
- Without `DEVICE_NETWORKS` the server starts and contacts no device.
- Policy rule editor rebuilt: the rules are a table (setting with its label, expected value, scope chips,
  severity, live findings count) with an editor panel for one rule at a time and the versions beside it.
  The expected value uses the control of the setting (dropdown, on/off, number with range, text), the scope
  uses chips from the known chip families and the inventory's tags and devices, and a live preview says how
  many devices the draft would flag. **breaking**: `POST /policy/rules` (save the whole list) is replaced by
  `POST /policy/rules/save` and `/delete` for one rule; importing JSON now compares with the saved rules and
  needs a confirmation step instead of loading into the editor.

### Security

- MQTT broker URLs with `ws://` or `wss://` are refused (live connection, connection test and
  settings validation): paho dials them outside the address filter, so they bypassed the
  always-blocked ranges. Use `tcp://` or `ssl://`.

## 0.2.0 (2026-10-19)

### Added

- Policy checks read live device values first (side-effect-free queries, hourly jittered
  poll, MQTT stat updates); backups are only a labelled fallback.
- Devices: bulk switch on/off for selected devices (confirmation for more than one,
  per-device results).
- Audit log with filters, paging and expandable details; firmware release dialogs;
  guided update page; system settings in sections.

### Changed

- Navigation grouped (Devices, Updates, System menus); one wide page container; stable
  device table and relay switches; static assets are cache-busted.
- Raw HTML in release notes is dropped instead of being shown as text.

## 0.1.0

From this release on tasmofleet follows SemVer (see CLAUDE.md, "Releases").

### Added

- System settings page (`/settings/system`, admin): operational settings stored in SQLite
  (migration 0010), environment values lock their field, bootstrap/security settings stay
  environment-only, many settings apply live (docs/security/system-settings.md). New keys
  `DISCOVERY_MQTT`, `DISCOVERY_SCAN`, `DISCOVERY_SCAN_INTERVAL`, `DISCOVERY_SCAN_CIDRS`.
- Policy rule editor (`/policy/rules`): structured rules with name, severity, tag/device
  scope, preview, versions, diff, revert, JSON import/export.
- Firmware page: release changelog, fleet coverage per release, recommended release,
  prefetch for the fleet, delete one or all unused cached files.
- Transaction retry: retry all failed/cancelled devices of an update or a selection; the
  new transaction links to the original (migration 0009).
- UI design pass: design system, status badges with text and glyph, stacked tables on
  phones, bulk selection, calm transaction page with step bar, `/transactions` list.

### Changed

- `/jobs` is replaced by `/transactions` and `/transactions/{id}`.
- The policy view and its navigation entry always exist (empty state without rules).

### Fixed

- Power toggles and console commands could run during `restore_power`; it now counts as
  an active OTA state for the device lock.
- A retry restores the device's original OtaUrl, not the tasmofleet URL left by the
  failed attempt.
- Direct ESP8266 plans record the minimal artifact they cache.

### Breaking

- Editing a device so that its address or port changes while the stored device password
  stays is refused: type the password again or remove it. Changing the MQTT broker needs
  its password again; an environment `MQTT_PASSWORD`/`MQTT_URL` locks both.
- `DEVICE_NETWORKS` may now come from the stored settings; the start still fails if the
  effective configuration has none. More keys are validated (`FIRMWARE_SOURCES`,
  `MQTT_URL`, `POLL_INTERVAL`/`TELEPERIOD` >= 1s, `AUDIT_RETENTION` >= 24h,
  `BACKUP_INTERVAL` 1s..1m).

### Security

- Stored MQTT password is bound to its broker address and never sent to another one.
- Release-note links only to an allow-list of hosts; hostile Markdown is rendered as text.

## 0.0.6

- **breaking**: CLI `tasmofleet admin set-password|delete` removed. Use
  `tasmofleet user passwd --create --role admin <name>` (also promotes an existing user
  for recovery) and `tasmofleet user delete <name>`.
- OTA power restore after an update: lights by default, relays opt-in per batch, never
  switches anything OFF, fails closed (migrations 0007, 0008; `OTA_POWER_MAX_AGE`).
- MQTT broker classification per device (migration 0006).
- mqttbus: one canonical device key; discovery topics decode only with the canonical
  upper-case MAC (docs/security/mqtt-ingest.md Q11/Q12).
- Address display unified (`internal/netaddr`).

## 0.0.5

- Discovery: MQTT candidates, HTTP autoscan, confirm new address (migration 0004).
- Console and power toggle; command policy is an allowlist.
- Backup diff and one-time-token restore (migration 0005).
- One device lock across restore, OTA, settings and console/power.
- Hardened MQTT ingest; audit `via` tag for the admin listener.

## 0.0.4

- Direct HTTPS admin listener for emergency access (`ADMIN_LISTEN`, `ADMIN_NETWORKS`;
  migration 0003).
- Firmware index refresh at start and periodically (`FIRMWARE_REFRESH_INTERVAL`).
- Branding, settings editor and policy view, TasmoAdmin web import, OIDC step-up.

## 0.0.3

- Firmware index parses the real ota.tasmota.com listing and GitHub index (OTA in 0.0.2
  found no files); factory images refused; minimal image always cached.
- OTA and backup integration suites; MQTT upgrade hints; batch cancel.

## 0.0.2

- OTA UI, standalone mode (`AUTH_MODE=local`, local accounts, self-signed TLS), OIDC
  group re-check (migration 0002 ends existing sessions).

## 0.0.1

- First walking skeleton: inventory, health, backups, OIDC/header/local login.
