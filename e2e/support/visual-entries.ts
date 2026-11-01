import type { Tier } from "./visual";

// One entry per reference id of design/v2/ref (design/v2/README.md):
// where our page is, what to click to reach the state, what to mask, which package owns it. The
// reference sizes and the widths/themes come from design/v2/ref-manifest.sha256, so an entry never
// repeats them. Every `*-loading` id is excluded on purpose (D12: server-rendered pages never show
// a full-page skeleton; the skeleton component is checked on the `feedback` screen).

/** Which app instance the page is rendered by. */
export type StackKind =
  | "fleet" // header auth, the design fixture's 60 devices, rules, backups, proposals (read-only use)
  | "empty" // header auth, no devices, no rules
  | "local" // local accounts (sign-in, forced password change)
  | "setup"; // a fresh install (first-run setup)

/** One action after the page has loaded, with the semantics of the prototype's renderer (tools/render.mjs). */
export type Step =
  | { click: string; exact?: boolean; nth?: number; optional?: boolean } // visible text, case-insensitive substring unless exact; optional: skipped when not on screen
  | { aria: string; nth?: number } // aria-label, exact
  | { clickSel: string } // CSS selector
  | { wait: number }
  | { preselect: number } // tick the first N online devices (rows, or cards on a phone)
  | { custom: string }; // a named step in visual-steps.ts; unknown until its owning package lands

export interface Entry {
  /** reference id without width and theme */
  id: string;
  /** work package that makes the screen pass (plan section 6) */
  owner: string;
  /** our route; {d2} is the app id of the fixture device d2, {txn} the running transaction, and so on */
  route: string;
  /** "overlay" for dialogs, toasts and results: the capture stays 900 px high (default "full") */
  mode?: "full" | "overlay";
  /** tolerance class, plan section 5.3 (default "page") */
  tier?: Tier;
  stack?: StackKind;
  /** role of the signed-in principal (default admin) */
  as?: "admin" | "operator";
  steps?: Step[];
  /** CSS selectors masked on both images in addition to the default masks */
  masks?: string[];
  /** HTTP status the page must answer with (default 200); "any" for pages whose status the port decides. A page that is missing (404) must never pass by resembling a mostly empty screen. */
  status?: number | "any";
  /** the steps change data (a backup is taken, a transaction starts): the entry runs on its own stack */
  mutates?: boolean;
  note?: string;
}

/** The dashboard's values the app cannot reproduce: transaction ids and start times, proposal
 * addresses ("New device · 192.168.20.88 · …") and how long ago an offline device was last seen. */
const dashboardMasks = [".txn-id", ".txn-when", ".att-prop .att-why", ".att-item:has(.t-danger) .att-why"];

export const entries: Entry[] = [
  { id: "dashboard", owner: "P1", route: "/", masks: dashboardMasks, note: "transaction ids and times, proposal addresses and the offline devices' last-seen times are masked (not reproducible); the running transaction's mix of states is P4's seed" },
  { id: "dashboard-empty", owner: "P1", route: "/", stack: "empty" },
  { id: "dashboard-conn-reconnecting", owner: "F3", route: "/", steps: [{custom: "conn-reconnecting"}], masks: dashboardMasks },
  { id: "dashboard-conn-offline", owner: "F3", route: "/", steps: [{custom: "conn-offline"}], masks: dashboardMasks },
  { id: "devices", owner: "P2", route: "/devices" },
  { id: "devices-empty", owner: "P2", route: "/devices", stack: "empty" },
  { id: "devices-conn-reconnecting", owner: "F3", route: "/devices", steps: [{custom: "conn-reconnecting"}] },
  { id: "devices-conn-offline", owner: "F3", route: "/devices", steps: [{custom: "conn-offline"}] },
  { id: "devices-v-filtered", owner: "P2", route: "/devices", steps: [{custom: "filter-chips"}] },
  { id: "device-overview", owner: "P3", route: "/devices/{d2}", note: "intentional divergence from the v2 prototype: the Overview follows the #145 mockups (docs/design/device-overview): live tiles, relay box, Identity | Connection, Activity" },
  { id: "device-overview-conn-reconnecting", owner: "F3", route: "/devices/{d2}", steps: [{custom: "conn-reconnecting"}] },
  { id: "device-overview-conn-offline", owner: "F3", route: "/devices/{d2}", steps: [{custom: "conn-offline"}] },
  { id: "device-settings", owner: "P3", route: "/devices/{d2}/settings" },
  { id: "device-console", owner: "P3", route: "/devices/{d2}/console" },
  { id: "device-backups", owner: "P10", route: "/devices/{d2}/backups", steps: [{custom: "backups-select-two"}], note: "the prototype shows two rows ticked (the selection bar is part of the screen)" },
  { id: "device-backups-v-empty", owner: "P10", route: "/devices/{d1}/backups", note: "the prototype empties d2; the fixture keeps backups on d2, so d1 (no backups) stands in" },
  { id: "device-drift", owner: "P3", route: "/devices/{d2}/drift" },
  { id: "device-replace", owner: "P10", route: "/devices/{d2}/replace" },
  { id: "device-replace-v-preview", owner: "P10", route: "/devices/{d2}/replace", steps: [{custom: "replace-preview"}] },
  { id: "device-offline-overview", owner: "P3", route: "/devices/{d5}" },
  { id: "device-unknown-overview", owner: "P3", route: "/devices/{d31}" },
  { id: "add-manual", owner: "P6", route: "/devices/new", steps: [{custom: "manual-address-error"}], note: "the prototype opens the tab with its mistyped address and the field error" },
  { id: "add-disc", owner: "P6", route: "/devices/new" },
  { id: "add-disc-v-empty", owner: "P6", route: "/devices/new", stack: "empty", note: "the empty stack has no MQTT broker, so the tab says MQTT discovery is off (accepted exceedance, P6); emptying the fleet stack instead would race the other mutating entries on their shared stack" },
  { id: "add-disc-v-ignored", owner: "P6", route: "/devices/new?ignored=1" },
  { id: "add-disc-v-discoff", owner: "P6", route: "/devices/new", steps: [{custom: "discovery-off"}], mutates: true },
  { id: "add-scan", owner: "P6", route: "/discovery/scan", steps: [{custom: "scan-finished"}] },
  { id: "add-scan-v-running", owner: "P6", route: "/discovery/scan", steps: [{custom: "scan-running"}], mutates: true },
  { id: "add-import", owner: "P6", route: "/devices/new", steps: [{custom: "import-plan"}] },
  { id: "add-import-v-result", owner: "P6", route: "/devices/new", steps: [{custom: "import-result"}], mutates: true },
  { id: "policy", owner: "N3", route: "/policy" },
  { id: "policy-empty", owner: "N3", route: "/policy", stack: "empty" },
  { id: "policy-v-empty", owner: "N3", route: "/policy", stack: "empty" },
  { id: "policy-v-failed", owner: "N3", route: "/policy", steps: [{custom: "live-check-failed"}] },
  { id: "policy-v-paused", owner: "N21", route: "/policy", steps: [{custom: "live-check-paused"}], mutates: true },
  { id: "compliant", owner: "P4", route: "/policy", steps: [{click: "Make compliant…"}, {clickSel: "button:has-text(\"changes\")"}, {wait: 2500}], mutates: true },
  { id: "updates", owner: "P4", route: "/transactions" },
  { id: "updates-conn-reconnecting", owner: "F3", route: "/transactions", steps: [{custom: "conn-reconnecting"}] },
  { id: "updates-conn-offline", owner: "F3", route: "/transactions", steps: [{custom: "conn-offline"}] },
  { id: "txn", owner: "P4", route: "/transactions/{txn}" },
  { id: "newupdate", owner: "P4", route: "/updates/new" },
  { id: "newupdate-v-preview", owner: "P4", route: "/updates/new", steps: [{custom: "update-preview"}] },
  { id: "firmware", owner: "P5", route: "/firmware" },
  { id: "firmware-v-checking", owner: "P5", route: "/firmware", steps: [{custom: "firmware-checking"}] },
  { id: "firmware-v-ghdown", owner: "P5", route: "/firmware", steps: [{custom: "github-down"}], mutates: true },
  { id: "firmware-v-prefetch", owner: "P5", route: "/firmware", steps: [{custom: "prefetch-progress"}], mutates: true },
  { id: "firmware-v-pffail", owner: "P5", route: "/firmware", steps: [{custom: "prefetch-failed"}], mutates: true },
  { id: "release", owner: "N6", route: "/firmware/releases/14.3.0", masks: [".rel-notes > *"], note: "the notes are masked: the prototype's are illustrative, the stack's feed has its own (the box for a breaking section and the notes markup are covered by handler tests); the fleet also needs tasmota-minimal.bin.gz (P5 (a)), so the aside is one row longer and the page is shorter than the prototype's long notes" },
  { id: "release-v-since", owner: "N6", route: "/firmware/releases/14.3.0?view=since", masks: [".rel-stxt", ".rel-slbl"], note: "the stack's feed has five releases between 13.4.0 and 14.3.0 with its own notes (no section headings, so no labels); the rows' text and labels are masked, the cards and their heads compared" },
  { id: "release-v-prefetch", owner: "N6", route: "/firmware/releases/14.3.0", steps: [{custom: "release-prefetch-progress"}], masks: [".rel-notes > *"], note: "as release; the running prefetch is the Fetching… badge linking its progress page (P5) instead of an inline bar" },
  { id: "release-v-pffail", owner: "N6", route: "/firmware/releases/14.3.0", steps: [{custom: "release-prefetch-failed"}], masks: [".rel-notes > *"], note: "as release" },
  { id: "settings-general", owner: "P8", route: "/settings/system?tab=general", tier: "structural", masks: [".sys-tab:not([hidden])"] },
  { id: "settings-general-v-stepup", owner: "P8", route: "/settings/system?tab=general", tier: "structural", stack: "local", steps: [{custom: "stepup"}], masks: [".sys-tab:not([hidden])", "dialog#stepup .udlg-in"] },
  { id: "settings-network", owner: "P8", route: "/settings/system?tab=network", tier: "structural", masks: [".sys-tab:not([hidden])"] },
  { id: "settings-network-v-stepup", owner: "P8", route: "/settings/system?tab=network", tier: "structural", stack: "local", steps: [{custom: "stepup"}], masks: [".sys-tab:not([hidden])", "dialog#stepup .udlg-in"] },
  { id: "settings-mqtt", owner: "P8", route: "/settings/system?tab=mqtt", tier: "structural", masks: [".sys-tab:not([hidden])"] },
  { id: "settings-mqtt-v-stepup", owner: "P8", route: "/settings/system?tab=mqtt", tier: "structural", stack: "local", steps: [{custom: "stepup"}], masks: [".sys-tab:not([hidden])", "dialog#stepup .udlg-in"] },
  { id: "settings-discovery", owner: "P8", route: "/settings/system?tab=discovery", tier: "structural", masks: [".sys-tab:not([hidden])"] },
  { id: "settings-discovery-v-stepup", owner: "P8", route: "/settings/system?tab=discovery", tier: "structural", stack: "local", steps: [{custom: "stepup"}], masks: [".sys-tab:not([hidden])", "dialog#stepup .udlg-in"] },
  { id: "settings-discovery-v-discoff", owner: "P8", route: "/settings/system?tab=discovery", tier: "structural", steps: [{custom: "discovery-off"}], masks: [".sys-tab:not([hidden])"] },
  { id: "settings-devices", owner: "P8", route: "/settings/system?tab=network", tier: "structural", masks: [".sys-tab:not([hidden])"] },
  { id: "settings-devices-v-stepup", owner: "P8", route: "/settings/system?tab=network", tier: "structural", stack: "local", steps: [{custom: "stepup"}], masks: [".sys-tab:not([hidden])", "dialog#stepup .udlg-in"] },
  { id: "settings-backups", owner: "N9", route: "/settings/system?tab=backups", tier: "structural", masks: [".sys-tab:not([hidden]) > .sys-grid"], note: "the Storage summary and table are compared (N9); the schedule fields are the real ones (D10, S22) and masked" },
  { id: "settings-backups-v-stepup", owner: "P8", route: "/settings/system?tab=backups", tier: "structural", stack: "local", steps: [{custom: "stepup"}], masks: [".sys-tab:not([hidden])", "dialog#stepup .udlg-in"] },
  { id: "settings-fwsource", owner: "P8", route: "/settings/system?tab=firmware", tier: "structural", masks: [".sys-tab:not([hidden])"] },
  { id: "settings-fwsource-v-stepup", owner: "P8", route: "/settings/system?tab=firmware", tier: "structural", stack: "local", steps: [{custom: "stepup"}], masks: [".sys-tab:not([hidden])", "dialog#stepup .udlg-in"] },
  { id: "settings-metrics", owner: "P8", route: "/settings/system?tab=general", tier: "structural", masks: [".sys-tab:not([hidden])"] },
  { id: "settings-metrics-v-stepup", owner: "P8", route: "/settings/system?tab=general", tier: "structural", stack: "local", steps: [{custom: "stepup"}], masks: [".sys-tab:not([hidden])", "dialog#stepup .udlg-in"] },
  { id: "settings-signin", owner: "P8", route: "/settings/system?tab=signin", tier: "structural", masks: [".sys-tab:not([hidden])"] },
  { id: "settings-signin-v-stepup", owner: "P8", route: "/settings/system?tab=signin", tier: "structural", stack: "local", steps: [{custom: "stepup"}], masks: [".sys-tab:not([hidden])", "dialog#stepup .udlg-in"] },
  { id: "settings-advanced", owner: "N9", route: "/settings/system?tab=advanced", tier: "structural", masks: [".sys-tab:not([hidden]) > :not(.sys-danger)"], note: "the Reset box is compared (N9); the fields above it are the real ones (D10, S22) and masked" },
  { id: "settings-advanced-v-stepup", owner: "P8", route: "/settings/system?tab=advanced", tier: "structural", stack: "local", steps: [{custom: "stepup"}], masks: [".sys-tab:not([hidden])", "dialog#stepup .udlg-in"] },
  { id: "audit", owner: "P8", route: "/audit", tier: "structural", masks: [".audit-card"] },
  { id: "audit-conn-reconnecting", owner: "F3", route: "/audit", tier: "structural", steps: [{custom: "conn-reconnecting"}] },
  { id: "audit-conn-offline", owner: "F3", route: "/audit", tier: "structural", steps: [{custom: "conn-offline"}] },
  { id: "users", owner: "P9", route: "/users" },
  { id: "users-v-stepup", owner: "P9", route: "/users", stack: "local", steps: [{custom: "signin-maren"}, {custom: "stepup"}], note: "the fleet stack's header principals cannot confirm (view only), so the local stack's admin maren opens the dialog" },
  { id: "user-u1", owner: "N4", route: "/users/principal/maren", note: "the fleet stack's maren is a header principal (the prototype's is SSO): proxy banner instead of the IdP's, view only (actions disabled, banner)" },
  { id: "user-u1-v-stepup", owner: "N4", route: "/users/maren", stack: "local", steps: [{custom: "stepup"}], note: "only a local or SSO admin can confirm: the local stack's maren on her own page" },
  { id: "user-u2", owner: "N4", route: "/users/principal/jonas", note: "jonas is a header principal on the fleet stack (SSO in the prototype)" },
  { id: "user-u2-v-stepup", owner: "N4", route: "/users/ali", stack: "local", steps: [{custom: "stepup"}], note: "the local stack has maren and ali only: the dialog over ali's page" },
  { id: "user-u3", owner: "N4", route: "/users/ali" },
  { id: "user-u3-v-stepup", owner: "N4", route: "/users/ali", stack: "local", steps: [{custom: "stepup"}] },
  { id: "user-u4", owner: "N4", route: "/users/principal/kiosk-hallway" },
  { id: "user-u4-v-stepup", owner: "N4", route: "/users/ali", stack: "local", steps: [{custom: "stepup"}], note: "the local stack has maren and ali only: the dialog over ali's page" },
  { id: "user-u5", owner: "N4", route: "/users/admin", note: "the fixture's local admin stays active (a deactivated local admin needs another enabled local admin; seed.ts)" },
  { id: "user-u5-v-stepup", owner: "N4", route: "/users/ali", stack: "local", steps: [{custom: "stepup"}], note: "the local stack has maren and ali only: the dialog over ali's page" },
  { id: "useradd", owner: "P9", route: "/users/new", stack: "local", steps: [{custom: "signin-maren"}], note: "local admin: behind the header proxy the form is view only" },
  { id: "useradd-v-error", owner: "P9", route: "/users/new", stack: "local", steps: [{custom: "useradd-errors"}], note: "the server reports one field at a time" },
  { id: "userotp", owner: "N4", route: "/users/ali", stack: "local", steps: [{custom: "signin-stepped-up"}, {custom: "user-otp"}], mutates: true, note: "Reset password on ali's page by the local admin maren (a header admin cannot change users); its own stack, ali's password changes" },
  { id: "account", owner: "P9", route: "/account", stack: "local", steps: [{custom: "signin-maren"}], note: "local admin: a header principal has no password form" },
  { id: "about", owner: "F3", route: "/about", tier: "foundation", masks: [".comp-detail"], note: "the component rows are compared (N13, W3); their detail lines are masked (uptime, sizes, versions of this machine). N13 has five of the prototype's seven rows (web server and discovery left out on purpose, plan 3.2 N13), so the card is shorter; the effective configuration fold and the source line stay below the cards (structure deviation S4)" },
  { id: "about-conn-reconnecting", owner: "F3", route: "/about", tier: "foundation", steps: [{custom: "conn-reconnecting"}], masks: [".comp-detail"], note: "as about: the component rows are compared, their detail lines masked; five of the prototype's seven rows (plan 3.2 N13)" },
  { id: "about-conn-offline", owner: "F3", route: "/about", tier: "foundation", steps: [{custom: "conn-offline"}], masks: [".comp-detail"], note: "as about: the component rows are compared, their detail lines masked; five of the prototype's seven rows (plan 3.2 N13)" },
  { id: "whatsnew", owner: "N5", route: "/whats-new", masks: [".pghead-sub", ".wn-sec > ul", ".wn-intro", ".wn-d"], note: "the page renders the real CHANGELOG.md of this binary (long items, many releases) where the prototype has four short illustrative releases: the item lists, dates and the version line are masked, the cards, version heads, chips and section labels compared; the height differs by the changelog's length" },
  { id: "signin", owner: "P9", route: "/login", stack: "local" },
  { id: "signin-v-error", owner: "P9", route: "/login", stack: "local", steps: [{custom: "signin-error"}] },
  { id: "signin-v-expired", owner: "P9", route: "/login", stack: "local", steps: [{custom: "signin-expired"}] },
  { id: "setup", owner: "N12", route: "/setup/account", stack: "setup", note: "step 1 is the setup link's page without a token: where the link is, no token input (the token comes in the link)" },
  { id: "setup-v-error", owner: "N12", route: "/setup/account?token={setuptoken}", stack: "setup", steps: [{custom: "form-errors"}], note: "the server reports one field at a time" },
  { id: "setup-v-basics", owner: "N12", route: "/setup/account?token={setuptoken}", stack: "setup", steps: [{custom: "setup-basics"}], mutates: true, note: "sets the first admin's password through the link, then the wizard's last step (P8); its own stack" },
  { id: "setup-v-tokenerr", owner: "N12", route: "/setup/account?token=not-the-setup-token", stack: "setup", status: 403 },
  { id: "pwchange", owner: "P9", route: "/account", stack: "local", steps: [{custom: "must-change-password"}] },
  { id: "compare", owner: "P10", route: "/devices/{d2}/backups/diff" },
  { id: "compare-v-all", owner: "P10", route: "/devices/{d2}/backups/diff?all=1" },
  { id: "edit", owner: "P3", route: "/devices/{d2}/edit" },
  { id: "edit-v-error", owner: "P3", route: "/devices/{d2}/edit", steps: [{custom: "form-errors"}], mutates: true },
  { id: "devdelete", owner: "N2", route: "/devices/{d2}/delete", masks: [".dd-what", ".dd-page .banner-text"], note: "the backups' sizes, versions and triggers and the banner's count, total and newest date are the seed's data (masked like the row dates); on a phone the acknowledgement row has the app's touch-target padding (P3)" },
  { id: "bulk-cmd", owner: "P7", route: "/devices", steps: [{custom: "bulk-scope-cmd"}] },
  { id: "bulk-cmd-v-result", owner: "P7", route: "/devices", steps: [{custom: "bulk-scope-cmd"}, {custom: "bulk-result"}], masks: [".bk-detail", ".pghead-sub"], mutates: true },
  { id: "bulk-backup", owner: "P7", route: "/devices", steps: [{custom: "bulk-scope-backup"}] },
  { id: "bulk-backup-v-result", owner: "P7", route: "/devices", steps: [{custom: "bulk-scope-backup"}, {custom: "bulk-result"}], masks: [".bk-detail", ".pghead-sub"], mutates: true },
  { id: "bulk-restart", owner: "P7", route: "/devices", steps: [{custom: "bulk-scope-restart"}] },
  { id: "bulk-restart-v-result", owner: "P7", route: "/devices", steps: [{custom: "bulk-scope-restart"}, {custom: "bulk-result"}], masks: [".bk-detail", ".pghead-sub"], mutates: true },
  { id: "bulk-on", owner: "P7", route: "/devices", steps: [{custom: "bulk-scope-on"}] },
  { id: "bulk-on-v-result", owner: "P7", route: "/devices", steps: [{custom: "bulk-scope-on"}, {custom: "bulk-result"}], masks: [".bk-detail", ".pghead-sub"], mutates: true },
  { id: "bulk-off", owner: "P7", route: "/devices", steps: [{custom: "bulk-scope-off"}] },
  { id: "bulk-off-v-result", owner: "P7", route: "/devices", steps: [{custom: "bulk-scope-off"}, {custom: "bulk-result"}], masks: [".bk-detail", ".pghead-sub"], mutates: true },
  { id: "e404", owner: "P9", route: "/this-page-does-not-exist", status: 404 },
  { id: "e403", owner: "P9", route: "/audit", as: "operator", status: 403 },
  { id: "eoff", owner: "P9", route: "/transactions", stack: "empty", status: 404, note: "the reference is the feature-switched-off page (Importing from TasmoAdmin); the empty stack has no firmware base URL, so Updates is switched off" },
  { id: "feedback", owner: "F4", route: "/styleguide", tier: "foundation" },
  { id: "restore", owner: "P10", route: "/backups/{backup}/restore" },
  { id: "restore-v-progress", owner: "P10", route: "/backups/{backup}/restore", steps: [{custom: "restore-progress"}], mutates: true },
  { id: "restore-v-failed", owner: "P10", route: "/backups/{backup}/restore", steps: [{custom: "restore-failed"}] },
  { id: "restore-v-done", owner: "P10", route: "/backups/{backup}/restore", steps: [{custom: "restore-done"}], mutates: true },
  { id: "policyio", owner: "N3", route: "/policy/io" },
  { id: "policyio-v-import", owner: "N3", route: "/policy/io", steps: [{custom: "policy-import"}] },
  { id: "devices-colmenu", owner: "P2", route: "/devices", steps: [{click: "Columns", exact: true, optional: true}], note: "the Columns chooser is offered from 720 px; on a phone the prototype shows the plain list" },
  { id: "devices-selected", owner: "P2", route: "/devices", steps: [{preselect: 3}] },
  { id: "devices-rowmenu", owner: "P2", route: "/devices", steps: [{aria: "Actions", nth: 1}] },
  { id: "devices-restart-dialog", owner: "P2", route: "/devices", mode: "overlay", steps: [{aria: "Actions", nth: 1}, {click: "Restart…", exact: true}], note: "the row menu writes Restart… (it opens a confirmation, wording table)" },
  { id: "devices-delete-dialog", owner: "P2", route: "/devices", mode: "overlay", steps: [{aria: "Actions", nth: 1}, {click: "Delete…"}] },
  { id: "devices-toast", owner: "P2", route: "/devices", mode: "overlay", steps: [{aria: "Actions", nth: 1}, {click: "Back up now"}], mutates: true },
  { id: "devices-bulk-cmd-dialog", owner: "P2", route: "/devices", mode: "overlay", steps: [{preselect: 3}, {click: "Send command"}] },
  { id: "devices-bulk-cmd-dialog-phone", owner: "P2", route: "/devices", mode: "overlay", steps: [{preselect: 3}, {click: "Command", exact: true}] },
  { id: "devices-bulk-restart-dialog", owner: "P2", route: "/devices", mode: "overlay", steps: [{preselect: 3}, {click: "Restart", exact: true}] },
  { id: "devices-usermenu", owner: "F3", route: "/devices", steps: [{click: "Maren"}], note: "the header principal of the visual stack is maren, so the menu trigger carries the prototype's label" },
  { id: "devices-nav-phone", owner: "F3", route: "/devices", steps: [{aria: "Open navigation"}] },
  { id: "devices-usermenu-phone", owner: "F3", route: "/", steps: [{aria: "Open navigation"}] },
  { id: "device-restart-dialog", owner: "P3", route: "/devices/{d2}", mode: "overlay", steps: [{click: "Restart", exact: true}] },
  { id: "device-console-sent", owner: "P3", route: "/devices/{d2}/console", steps: [{clickSel: "button[value=\"Status 11\"]"}, {wait: 900}], masks: ["[data-visual-mask=console-reply]"], mutates: true, note: "the quick command sends at once (no JavaScript), so there is no Send click; the earlier modes' Status 11 lines are in the history, so the chip is clicked by its value, not its text" },
  { id: "policy-rulemenu", owner: "N3", route: "/policy", steps: [{clickSel: "section#rules tbody summary >> nth=1"}] },
  { id: "policy-rule-delete-dialog", owner: "N3", route: "/policy", mode: "overlay", steps: [{clickSel: "section#rules tbody summary >> nth=1"}, {click: "Delete…"}] },
  { id: "policy-rule-edit", owner: "N3", route: "/policy", steps: [{clickSel: "section#rules td.p1 a >> nth=1"}] },
  { id: "policy-rule-new", owner: "N3", route: "/policy", steps: [{click: "New rule"}] },
  { id: "users-menu", owner: "P9", route: "/users", steps: [{aria: "User actions", nth: 1}] },
  { id: "users-menu-sso", owner: "P9", route: "/users", steps: [{aria: "User actions", nth: 2}] },
  { id: "users-deactivate-dialog", owner: "P9", route: "/users", stack: "local", mode: "overlay", steps: [{custom: "signin-stepped-up"}, {aria: "User actions", nth: 1}, {click: "Deactivate", exact: true, nth: 1}], note: "the fleet stack is view only behind the header proxy; Deactivate asks in a dialog before anything changes, so the shared local stack stays as it is" },
  { id: "firmware-older", owner: "P5", route: "/firmware", steps: [{click: "Show 8 older releases"}] },
  { id: "firmware-selected-14.2.0", owner: "P5", route: "/firmware", steps: [{click: "14.2.0"}] },
  { id: "txn-cancelling-dialog", owner: "P4", route: "/transactions/{txn}", mode: "overlay", steps: [{click: "Cancel remaining"}] },
  { id: "firmware-check-toast", owner: "P5", route: "/firmware", mode: "overlay", steps: [{custom: "check-now"}, {wait: 2000}], mutates: true },
  { id: "policy-livecheck-pause-toast", owner: "N21", route: "/policy", mode: "overlay", steps: [{custom: "live-check-running"}, {click: "Pause", exact: true}], mutates: true },
  { id: "policy-livecheck-resume-toast", owner: "N21", route: "/policy", mode: "overlay", steps: [{custom: "live-check-paused"}, {click: "Resume", exact: true}], mutates: true },
  { id: "policy-livecheck-retry-toast", owner: "N3", route: "/policy", mode: "overlay", steps: [{custom: "live-check-failed"}, {click: "Retry now"}], mutates: true },
  { id: "bulk-on-run-toast", owner: "P7", route: "/devices", mode: "overlay", steps: [{custom: "bulk-scope-on"}, {custom: "bulk-result"}], masks: [".bk-detail", ".pghead-sub", ".toast-text"], mutates: true },
  { id: "device-backups-prune-dialog", owner: "P10", route: "/devices/{d2}/backups", mode: "overlay", steps: [{custom: "prune-dialog"}] },
  { id: "firmware-delete-dialog", owner: "P5", route: "/firmware", mode: "overlay", steps: [{custom: "delete-cached-file"}] },
  { id: "device-drift-all", owner: "P3", route: "/devices/{d2}/drift", steps: [{click: "Show all"}] },
  { id: "policy-rule-revert-dialog", owner: "N3", route: "/policy/history", mode: "overlay", steps: [{clickSel: "button[data-dialog=dlg-revert]"}] },
  { id: "settings-mqtt-test", owner: "P8", route: "/settings/system?tab=mqtt", tier: "structural", steps: [{click: "Test connection"}, {wait: 1500}], masks: [".sys-tab:not([hidden])"] },
  { id: "policy-makecompliant-dialog", owner: "N3", route: "/policy", mode: "overlay", steps: [{click: "Make compliant…"}] },
  { id: "policy-makecompliant-result", owner: "N3", route: "/policy", mode: "overlay", steps: [{custom: "live-check-running"}, {click: "Make compliant…"}, {clickSel: "button:has-text(\"changes\")"}, {wait: 2500}], mutates: true },
  { id: "logo", owner: "F3", route: "/", tier: "foundation", steps: [{custom: "brand-crop"}], note: "6x crop of the header brand (no width in the reference name): F3 compares the brand element at deviceScaleFactor 6" },
];

/** excludedIds are the references no entry compares: every loading skeleton (D12). */
export const excluded = (id: string): boolean => id.endsWith("-loading");
