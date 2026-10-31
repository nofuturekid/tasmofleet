import { Page, expect } from "@playwright/test";
import { copyFileSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { App, Devenv } from "./stack";
import { pickForUpdate, startUpdate, txnState } from "./updates";

/** devenvArgs make the simulator fleet and the fake upstream as large as a real home setup. */
export const realisticDevenvArgs = ["-fleet-size", "27", "-releases", "10"];

/**
 * realisticPolicyRules is the policy a real fleet has: the ESP32 exception
 * (esp32* expands to every ESP32 variant, the widest "Applies to" cell), a
 * rule for ESP8266 and one scoped by tag and exception.
 */
export const realisticPolicyRules = JSON.stringify([
  { name: "Keep power state", command: "SetOption0", want: "0", families: ["esp8266"], note: "ESP8266 devices restore their last power state after a restart." },
  { name: "ESP32 exception", command: "SetOption0", want: "1", families: ["esp32*"], note: "ESP32 boards must not restore the power state: a brown-out would switch loads on." },
  { name: "Telemetry period", command: "TelePeriod", want: "300", tags: ["sensors", "outdoor-weather-station"], except: ["locked-plug"], severity: "info", note: "Tags and exceptions make this the longest cell." },
]);

/**
 * seedRealisticData brings a freshly started app to the state a real
 * installation is in, so layout checks do not run on sparse data: every
 * simulated device is reachable, the firmware the fleet needs is cached (files
 * with device links), and there is a cancelled transaction with its retry.
 * The audit log fills itself from these actions. Needs a page with admin headers.
 */
export async function seedRealisticData(page: Page, app: App, env: Devenv): Promise<void> {
  const names = Object.keys(env.devices);
  await page.goto(app.url + "/devices");
  for (const name of names) {
    await expect(page.getByRole("row").filter({ hasText: name }).getByText("Online", { exact: true }).first()).toBeVisible({ timeout: 60_000 });
  }

  // Cached files, each used by several devices.
  await expect(async () => {
    await page.goto(app.url + "/firmware");
    await expect(page.locator(`#release-${env.vars.DEVENV_FW_VERSION.replaceAll(".", "\\.")}`)).toBeVisible();
  }).toPass({ timeout: 30_000 });
  // the newest release is the selected one; its files section offers the whole set
  await page.getByRole("button", { name: "Prefetch for my fleet" }).click();
  await expect(page.getByText(/State: finished\b/)).toBeVisible({ timeout: 90_000 });

  // A transaction that was cancelled, and its retry (linked both ways).
  await pickForUpdate(page, app.url, ["plug-4m"]);
  await startUpdate(page);
  await page.getByRole("link", { name: "Cancel remaining" }).click();
  await page.locator("dialog[open]").getByRole("button", { name: "Cancel remaining" }).click();
  await expect(page.locator("#batch-summary")).toContainText("1 cancelled", { timeout: 60_000 });
  await page.getByRole("link", { name: "Retry failed" }).click();
  await page.getByRole("button", { name: /Start retry of 1 device/ }).click();
  await expect(page.getByText(/Retry of/)).toBeVisible();
  await expect(txnState(page)).toHaveText("Completed", { timeout: 200_000 });

  // A second transaction with a long unbreakable device name next to the
  // retried one: long names are what stretch the transactions list.
  await pickForUpdate(page, app.url, ["workshop-ceiling-spotlight-controller-north-wing", "lamp-1m"]);
  await startUpdate(page);
  await expect(txnState(page)).toHaveText("Completed", { timeout: 200_000 });

  // Names as long as real ones, so the Name column is judged on real lengths.
  for (const [from, to] of Object.entries(renamedDevices)) await renameDevice(page, app, from, to);

  // Policy versions saved by a user whose identity is a 40-character hash (an
  // OIDC subject), with a change note that quotes one: long unbreakable tokens
  // in a table cell are what once made the policy rules page 668 px wide.
  await savePolicyVersion(page, app, "Telemetry rhythm", "TelePeriod", "300", `${hash40} sets the period`);
  await savePolicyVersion(page, app, "Save data", "SaveData", "1", "second version");

  await seedProposals(app);
}

/**
 * seedProposals puts discovery proposals into the database: no simulator
 * announces itself over MQTT or answers a scan on port 80, and the proposals
 * table is what the phone row cards on /discovery are judged on. One name is as
 * long as a real one (it must ellipsize, not push the buttons off the line).
 */
export async function seedProposals(app: App): Promise<void> {
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const now = Date.now();
    const insert = db.prepare(
      `INSERT INTO discovery_proposals (mac, kind, via, reason, name, address, port, topic, full_topic, existing_id, dismissed, first_seen, seen_at)
       VALUES (?,?,?,?,?,?,?,?,?,0,0,?,?)`,
    );
    insert.run("aa:bb:cc:00:00:01", "new", "mqtt", "", "Kitchen", "172.31.13.41", 80, "kitchen_plug", "tasmota/%topic%/%prefix%/", now - 3_600_000, now - 120_000);
    insert.run("aa:bb:cc:00:00:02", "new", "scan", "", "workshop-ceiling-spotlight-controller-south-wing", "172.31.13.42", 80, "workshop_spots", "tasmota/%topic%/%prefix%/", now - 7_200_000, now - 600_000);
  } finally {
    db.close();
  }
}

/**
 * seedAuditTail appends `count` audit entries newer than everything the app has written so far, copies of
 * the newest backup entry (an operator's action on a device) whose target rotates over the device with a
 * short name and the devices the seed renamed to long names, so the audit page still covers wrapping and
 * overflow of long targets on a phone. The app writes entries of its own at times nobody controls: the burst
 * of "Edit device" rows when the devices' first MQTT messages arrive (any order, in the same second),
 * "Identity mqtt ask" and "Discovery reprobe" rows of the locked device. Whichever of them fell into the
 * first page of the audit log changed its height (#237: 3502 px against 3414 to 3436 px, budget 3492). With
 * a tail as long as the page, the first page holds only entries this function wrote, and its height depends
 * on nothing else. The tail is dated an hour ahead so that nothing the app writes while the tests run (the
 * MQTT identity grace alone is 30 s) can sort in front of it.
 */
export async function seedAuditTail(app: App, count: number): Promise<void> {
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const latest = (db.prepare("SELECT MAX(at) AS at FROM audit_log").get() as { at: number }).at;
    const names = Object.values(renamedDevices);
    const longs = db
      .prepare(`SELECT id FROM devices WHERE name IN (${names.map(() => "?").join(",")}) ORDER BY id`)
      .all(...names) as { id: number }[];
    if (longs.length !== names.length) throw new Error("seedAuditTail: the renamed devices are missing");
    const model = db.prepare("SELECT target FROM audit_log WHERE action LIKE 'backup%' ORDER BY id DESC LIMIT 1").get() as { target: string } | undefined;
    if (!model) throw new Error("seedAuditTail: the seed wrote no backup entry to copy");
    // short, long, short, long, ...: half of the entries carry a long target
    const targets = longs.flatMap((d) => [model.target, `device:${d.id}`]);
    const copy = db.prepare(
      `INSERT INTO audit_log (at, actor, action, target, detail, result, remote_addr)
       SELECT ?, actor, action, ?, detail, result, remote_addr FROM audit_log WHERE action LIKE 'backup%' ORDER BY id DESC LIMIT 1`,
    );
    for (let i = 1; i <= count; i++) copy.run(latest + 3_600_000 + i * 1000, targets[i % targets.length]);
  } finally {
    db.close();
  }
}

/** renamedDevices are simulator devices the seed renames to real-length names (the name the UI shows). */
export const renamedDevices: Record<string, string> = {
  "sim-02-esp32": "Mr Bambu External Light",
  "sim-03-esp8266-1m": "livingroom-readinglamp.example.internal",
  // Longer than the content width allows on one line: once with hyphens (it can
  // break there) and once without any break opportunity. The devices list and
  // the transaction pages must wrap both inside the page (QA v0.9.0).
  "sim-06-esp8266-4m": "attic-heater-plug-with-a-really-long-device-name-that-wraps",
  "sim-07-esp8266-4m": "AtticHeaterPlugWithAReallyLongDeviceNameThatNeverHasABreakAtAll",
};

/** hash40 is a full SHA-1 style identifier, as it appears in real data. */
export const hash40 = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b";

async function savePolicyVersion(page: Page, app: App, name: string, command: string, want: string, note: string): Promise<void> {
  await page.setExtraHTTPHeaders({ "X-authentik-username": hash40, "X-authentik-groups": "adm" });
  try {
    await page.goto(app.url + "/policy/rules/new?command=" + encodeURIComponent(command)); // the draft rule page
    await expect(page.locator("#want")).toBeEnabled();
    await page.locator("input[name=name]").fill(name);
    const control = page.locator("#want");
    if ((await control.evaluate((e) => e.tagName)) === "SELECT") await control.selectOption({ index: 1 });
    else await control.fill(want);
    await page.locator("input[name=change_note]").fill(note);
    await page.getByRole("button", { name: "Save rule" }).click();
    await expect(page.getByText(`Rule “${name}” saved`)).toBeVisible(); // Save ends on the overview
  } finally {
    await page.setExtraHTTPHeaders({ "X-authentik-username": "adam", "X-authentik-groups": "adm" });
  }
}

async function renameDevice(page: Page, app: App, from: string, to: string): Promise<void> {
  await page.goto(app.url + "/devices");
  const href = await page.getByRole("link", { name: from, exact: true }).getAttribute("href");
  await page.goto(app.url + href + "/edit");
  await page.locator("input[name=name]").fill(to);
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page.getByRole("heading", { level: 1, name: to })).toBeVisible();
}

// ---------------------------------------------------------------------------
// Design fixture (design/v2/README.md)
// ---------------------------------------------------------------------------

/** designFixturePath is the file tasmofleet-devenv -design-fixture reads, generated by design/tools/v2-fixture.mjs. */
export const designFixturePath = join(__dirname, "../../testdata/design/v2-fixture.json");

export interface DesignFixture {
  latest: string;
  releases: string[];
  devices: { id: string; name: string; kind: string; chip: string; status: "online" | "offline" | "unknown" }[];
  rules: { id: number; name: string; key: string; val: string; scope: string; named?: string[]; exc?: string[]; sev: string; enabled?: boolean; note: string }[];
  backups: { id: string; date: string; trigger: string; fw: string; size: string }[];
  users: { id: string; user: string; role: string; src: string; active: boolean; pending?: boolean }[];
  scan: { ip: string; host: string; module: string; fw: string; chip: string }[];
  proposals: { id: string; name: string; host: string; ip: string; mac: string; via: "mqtt" | "scan"; status: "new" | "ignored" }[];
  moves: { id: string; device: string; address: string; via: "mqtt" | "scan" }[];
  transaction: { id: string; release: string; selected: string; devices: { id: string; name: string; from: string; state: string }[] };
}

export function loadDesignFixture(): DesignFixture {
  return JSON.parse(readFileSync(designFixturePath, "utf8")) as DesignFixture;
}


// ---------------------------------------------------------------------------
// README media fleet (make demo-media)
// ---------------------------------------------------------------------------

/** isMedia: the run renders the README's pictures (DEMO_MEDIA=1), which use the cleaned-up fleet of mediaFixture. */
export const isMedia = !!process.env.DEMO_MEDIA;

/** mediaAdmin is the signed-in admin of the README pictures: the name shows in the navigation bar. */
export const mediaAdmin = { "X-authentik-username": "markus", "X-authentik-groups": "adm" };

/** pageAdmin is the admin principal of the run: the design's Maren, or the media's Markus. */
export const pageAdmin = isMedia ? mediaAdmin : designAdmin;

/** mediaNames are the fixture devices that get a plainer name for the pictures (no "Kitchen Plug 2" style numbering, no 60-character name). */
const mediaNames: Record<string, string> = {
  d7: "Attic Heater Plug",
  d25: "Greenhouse Fan",
  d34: "Workshop Heater Plug",
  d36: "Carport Ceiling Light",
  d38: "Basement Stairs Switch",
  d39: "Guest Bathroom Lamp",
  d43: "Guest Room Heater Plug",
  d44: "Terrace Ceiling Light",
  d47: "Dining Room Dimmer",
  d48: "Guest Room LED Strip",
  d50: "Bathroom Heater Plug",
  d51: "Garage Entrance Ceiling Light",
  d56: "Workshop Bench Dimmer",
  d59: "Entrance Ceiling Light",
};

/**
 * mediaFixture derives the fleet of the README pictures from the design fixture, which stays as it
 * is for the other specs: believable device names (each also its Friendly name 1), a home domain
 * instead of lab.home, a running update without a failed device (5 done, 4 in progress, 3 waiting),
 * and only a few devices that need attention (two offline, one never answered).
 */
export function mediaFixture(fx: DesignFixture): DesignFixture {
  const m: DesignFixture = JSON.parse(JSON.stringify(fx).replaceAll("mqtt.lab.home", "mqtt.home.lan").replaceAll("@lab.home", "@home.lan").replaceAll("Use the lab MQTT broker", "Use the home MQTT broker"));
  for (const d of m.devices) {
    d.name = mediaNames[d.id] ?? d.name;
    (d as { friendlyName?: string }).friendlyName = d.name;
    if (["d38", "d49", "d53"].includes(d.id)) d.status = "online";
  }
  const name = (id: string) => m.devices.find((d) => d.id === id)!.name;
  for (const t of m.transaction.devices) {
    t.name = name(t.id);
    if (t.state === "failed") t.state = "downloading";
  }
  for (const u of m.users) if (u.user === "maren") u.user = "markus";
  for (const u of m.users as { name: string }[]) if (u.name === "Maren Koch") u.name = "Markus Koch";
  return m;
}

/** designAdmin and designOperator are the header principals of the visual stacks; the prototype's signed-in user is Maren. */
export const designAdmin = { "X-authentik-username": "maren", "X-authentik-groups": "adm" };
export const designOperator = { "X-authentik-username": "jonas", "X-authentik-groups": "ops" };

/**
 * designPolicyRules converts the prototype's rules to the POLICY_RULES format v0.10.1 understands.
 * The prototype's "Required" (v6; "Enforce" before) is our stronger level ("warn", labelled Required in the port), its "Advisory"
 * (before: "Warn") the hint ("info"). Scopes become device lists (named devices, the plugs) or the ESP32
 * families. The prototype's one disabled rule (since N3 a rule can be disabled) checks LedState, a setting
 * TasmoFleet does not read, so it is left out.
 */
export function designPolicyRules(fx: DesignFixture): string {
  const name = (id: string) => fx.devices.find((d) => d.id === id)!.name;
  const rules = fx.rules
    .filter((r) => r.enabled !== false)
    .map((r) => ({
      name: r.name,
      command: r.key,
      want: r.val,
      note: r.note || undefined,
      severity: /^(Required|Enforce)$/.test(r.sev) ? "warn" : "info",
      devices: r.scope === "Named devices" ? (r.named ?? []).map(name) : /Plugs/.test(r.scope) ? fx.devices.filter((d) => /Plug/.test(d.kind)).map((d) => d.name) : undefined,
      families: /ESP32/.test(r.scope) ? ["esp32*"] : undefined,
      except: r.exc?.length ? r.exc.map(name) : undefined,
    }));
  return JSON.stringify(rules);
}

/**
 * seedDesignV2 brings an app that imported the design fixture's devices to the prototype's state:
 * the offline devices are taken away after the app saw them online, four backups of d2, the
 * prototype's discovered devices and address changes as discovery proposals (with the prototype's
 * first and last seen), the local accounts and header principals of the user list, one finished
 * update transaction and one policy live check. Needs a page with admin headers and a desktop-size viewport.
 *
 * Not reproducible yet and masked until the owning package (plan 5.2): the backups' trigger labels
 * (N1), single sign-on principals (N4). The running transaction is seedRunningTransaction's.
 */
export async function seedDesignV2(page: Page, app: App, env: Devenv, fx: DesignFixture): Promise<void> {
  const t0 = Date.now();
  const phase = (what: string) => console.log(`seedDesignV2: ${what} (${Math.round((Date.now() - t0) / 1000)} s)`);
  const online = fx.devices.filter((d) => d.status !== "unknown");
  await page.goto(app.url + "/devices");
  // About 30 s for the whole fleet. A slow device list shows here first: re-measuring every row for
  // each live cell swap made it 140 s, so the limit stays tight instead of growing again.
  for (const d of online) {
    await expect(page.getByRole("row").filter({ hasText: d.name }).getByText("Online", { exact: true }).first()).toBeVisible({ timeout: 60_000 });
  }
  phase("devices online");
  // "Offline" needs a first contact, so the devices the prototype shows as offline are stopped now.
  const stopped = await page.request.post(env.vars.DEVENV_CONTROL_URL + "/fixture/offline");
  expect(stopped.status()).toBe(204);
  // Wait on what the server renders, not on a live swap (#69): the Offline update reaches an open
  // page over SSE only if the page is connected and keeps up, which a slow runner with 60 devices
  // coming online does not guarantee, and a missed update is never repeated. A fresh page always
  // shows the tracker's state, so each attempt reloads.
  for (const d of fx.devices.filter((x) => x.status === "offline")) {
    await expect(async () => {
      await page.reload();
      await expect(page.getByRole("row").filter({ hasText: d.name }).getByText("Offline", { exact: true }).first()).toBeVisible({ timeout: 15_000 });
    }).toPass({ timeout: 120_000, intervals: [1_000] });
  }

  phase("devices offline");
  const ids = await deviceIds(page, app);
  const d2 = fx.devices.find((d) => d.id === "d2")!;
  // A manual backup is refused for 1 minute after the last one, so one real backup is taken and
  // the older ones are copies of its rows, dated as far back as the prototype's list.
  await page.goto(`${app.url}/devices/${ids.get(d2.name)}`);
  const taken = page.waitForResponse((r) => r.request().method() === "POST" && /\/devices\/\d+\/backup$/.test(new URL(r.url()).pathname), { timeout: 60_000 });
  await page.getByRole("button", { name: "Back up", exact: true }).first().click(); // the device header's button (P3): a form post answered by a redirect
  expect((await taken).status(), `backup of ${d2.name}`).toBe(303);
  await seedOlderBackups(app, Number(ids.get(d2.name)), fx);
  phase("backups seeded");
  await seedDesignProposals(app, fx, ids);
  await seedDesignTags(app, fx);
  // The prototype prefills the add form from a fleet default device password (Settings → Devices, N20).
  await page.goto(app.url + "/devices");
  const token = await page.evaluate(() => JSON.parse(document.body.getAttribute("hx-headers") ?? "{}")["X-CSRF-Token"] ?? "");
  const pw = await page.request.post(app.url + "/settings/system", { form: { csrf: token, "v.DEVICE_DEFAULT_PASSWORD": "design-fixture-device-pw" }, maxRedirects: 0 });
  expect(pw.status(), "fleet default device password").toBe(303);
  for (const u of fx.users.filter((x) => x.src === "local")) {
    const args = ["user", "add", "--role", u.role.toLowerCase(), ...(u.pending ? ["--must-change"] : []), u.user];
    app.cli(args, "design-fixture-password-1\n");
    // A deactivated local admin needs another enabled admin ("this would leave no enabled admin");
    // the prototype's is a single-sign-on account, which arrives with N4. Until then it stays active.
  }
  // header principals appear in the user list once they have made a request
  for (const headers of [designOperator, { "X-authentik-username": "kiosk-hallway", "X-authentik-groups": "ops" }]) {
    expect((await page.request.get(app.url + "/devices", { headers })).status()).toBe(200);
  }
  phase("principals and accounts");
  await seedFinishedUpdate(page, app, fx);
  phase("update finished");
  // The prototype's fleet knows its policy deviations (dashboard tile, "Needs attention", device
  // badges). Ours reads them live: one "Run now" of the policy live check, as an admin would.
  await page.goto(app.url + "/policy");
  const checked = page.waitForResponse((r) => r.request().method() === "POST" && new URL(r.url()).pathname.endsWith("/policy/live"), { timeout: 120_000 });
  await page.locator("form#live-check").evaluate((f: HTMLFormElement) => f.requestSubmit());
  expect((await checked).ok(), "policy live check").toBe(true);
  await page.waitForURL(/\/policy\/live/, { waitUntil: "load" }); // the answer is a page: let it finish before the next navigation
  phase("policy checked");
  await seedFirmwareCache(page, app, fx);
  phase("firmware cache");
}

/**
 * seedFirmwareCache leaves the cache as the prototype's firmware screen shows it: the newest
 * release's ESP8266 and ESP32 files cached, its ESP32-C3 file not (it offers Prefetch), and the
 * two files of the release before it.
 */
async function seedFirmwareCache(page: Page, app: App, fx: DesignFixture): Promise<void> {
  await page.goto(app.url + "/firmware");
  const c3 = page.locator(".fw-crow").filter({ hasText: "tasmota32c3.bin" }).filter({ hasText: fx.latest });
  if ((await c3.count()) > 0) {
    await c3.getByRole("link", { name: /^Delete/ }).click();
    await page.locator("dialog[open]").getByRole("button", { name: "Delete file" }).click();
    await expect(page.locator(".fw-crow").filter({ hasText: "tasmota32c3.bin" })).toHaveCount(0);
  }
  const before = fx.releases[1];
  for (const file of ["tasmota.bin.gz", "tasmota32.bin"]) {
    await page.goto(`${app.url}/firmware?release=${before}`);
    const fetch = page.getByRole("button", { name: `Prefetch ${file}`, exact: true });
    if ((await fetch.count()) === 0) continue; // cached already
    await fetch.click();
    await expect(fetch).toHaveCount(0, { timeout: 60_000 });
  }
}

/** deviceIds maps device names to the app's numeric ids by reading the links on /devices. */
export async function deviceIds(page: Page, app: App): Promise<Map<string, string>> {
  await page.goto(app.url + "/devices");
  const links = await page.evaluate(() =>
    [...document.querySelectorAll<HTMLAnchorElement>("a[href]")]
      .map((a) => ({ name: (a.textContent ?? "").trim(), path: new URL(a.href).pathname }))
      .filter((l) => /\/devices\/\d+$/.test(l.path) && l.name),
  );
  return new Map(links.map((l) => [l.name, l.path.split("/").pop()!]));
}

/**
 * seedOlderBackups copies the newest backup of a device once per older entry of the prototype's list.
 * Every copy gets its own files: the retention removes a pruned row's file, and a shared file would
 * take the newest backup with it (a restore screen takes two backups per restore and pushes the
 * oldest copies past the retention).
 */
async function seedOlderBackups(app: App, deviceId: number, fx: DesignFixture): Promise<void> {
  const when = (b: DesignFixture["backups"][number]) => Date.parse(b.date.replace(",", ""));
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const newest = db
      .prepare(
        `SELECT device_id, kind, taken_at, path, size, sha256, decoder_version, note FROM backups
         WHERE device_id = ? AND taken_at = (SELECT MAX(taken_at) FROM backups WHERE device_id = ?)`,
      )
      .all(deviceId, deviceId) as { device_id: number; kind: string; taken_at: number; path: string; size: number; sha256: string; decoder_version: string; note: string }[];
    const insert = db.prepare(
      `INSERT INTO backups (device_id, kind, taken_at, fw_version, path, size, sha256, decoder_version, note) VALUES (?,?,?,?,?,?,?,?,?)`,
    );
    fx.backups.slice(1).forEach((older, i) => {
      for (const b of newest) {
        const path = b.path.replace(/(\.[a-z]+)$/, `-seed${i + 1}$1`);
        copyFileSync(join(app.dataDir, b.path), join(app.dataDir, path));
        insert.run(b.device_id, b.kind, b.taken_at - (when(fx.backups[0]) - when(older)), older.fw, path, b.size, b.sha256, b.decoder_version, b.note);
      }
    });
  } finally {
    db.close();
  }
}

/** designTags are the prototype's device tags (tagsOf in template.html): derived from the kind and the room. */
export function designTags(d: { name: string; kind: string }): string[] {
  const t: string[] = [];
  if (/Plug/.test(d.kind)) t.push("plugs");
  if (/Lamp|Light|LED|Dimmer/.test(d.kind)) t.push("lights");
  if (/Switch|Fan/.test(d.kind)) t.push("switches");
  if (/Heater/.test(d.kind)) t.push("heating");
  if (/^(Garden|Porch)/.test(d.name)) t.push("outdoor");
  return t;
}

/** seedDesignTags gives every device the prototype's tags, so the Tag filter has the same options and counts. */
async function seedDesignTags(app: App, fx: DesignFixture): Promise<void> {
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const set = db.prepare("UPDATE devices SET tags = ? WHERE name = ?");
    for (const d of fx.devices) set.run(JSON.stringify(designTags(d)), d.name);
  } finally {
    db.close();
  }
}

/**
 * seedDesignProposals puts the prototype's discovery proposals (P0: new and ignored devices) and
 * address changes (M0: a known device announced a new address) into the database, newest first in
 * the prototype's order. The address changes carry the device's own MAC, as a real announcement does.
 * designSeen gives each its first and last seen in minutes before now (P0/M0 in design/v2/template.html),
 * so the Discovered tab shows the prototype's ages. Model and firmware are not stored with a proposal.
 */
const designSeen: Record<string, { firstMin: number; lastMin: number }> = {
  p1: { firstMin: 95, lastMin: 2 },
  p2: { firstMin: 10, lastMin: 5 },
  p3: { firstMin: 900, lastMin: 1 },
  p4: { firstMin: 60 * 24 * 40, lastMin: 60 * 24 * 3 },
  p5: { firstMin: 60 * 24 * 20, lastMin: 360 },
  m1: { firstMin: 4, lastMin: 4 },
  m2: { firstMin: 25, lastMin: 25 },
};

async function seedDesignProposals(app: App, fx: DesignFixture, ids: Map<string, string>): Promise<void> {
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const now = Date.now();
    const insert = db.prepare(
      `INSERT INTO discovery_proposals (mac, kind, via, reason, name, address, port, topic, full_topic, existing_id, dismissed, first_seen, seen_at)
       VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
    );
    const seen = (id: string) => {
      const t = designSeen[id];
      if (!t) throw new Error(`design fixture: no first/last seen for ${id}`);
      return [now - t.firstMin * 60_000, now - t.lastMin * 60_000];
    };
    for (const p of fx.proposals) {
      insert.run(p.mac, "new", p.via, "", p.name, p.ip, 80, p.host.toLowerCase(), "tasmota/%topic%/%prefix%/", 0, p.status === "ignored" ? 1 : 0, ...seen(p.id));
    }
    const macOf = db.prepare("SELECT mac FROM devices WHERE id = ?");
    for (const m of fx.moves) {
      const dev = fx.devices.find((d) => d.id === m.device)!;
      const id = Number(ids.get(dev.name));
      if (!id) throw new Error(`design fixture device ${m.device} has no app id`);
      const { mac } = macOf.get(id) as { mac: string };
      if (!mac) throw new Error(`design fixture: ${dev.name} has no MAC for its address change`);
      insert.run(mac, "address-change", m.via, "", dev.name, m.address, 80, "", "", id, 0, ...seen(m.id));
    }
  } finally {
    db.close();
  }
}

/** seedFinishedUpdate runs one real update of an outdated device, so /transactions has a finished transaction. */
async function seedFinishedUpdate(page: Page, app: App, fx: DesignFixture): Promise<void> {
  const outdated = fx.devices.find((d) => d.status === "online" && d.id !== "d2" && /Lamp/.test(d.kind));
  if (!outdated) throw new Error("design fixture has no online lamp to update");
  await expect(async () => {
    await page.goto(app.url + "/firmware");
    await expect(page.locator(`#release-${fx.latest.replaceAll(".", "\\.")}`)).toBeVisible();
  }).toPass({ timeout: 60_000 });
  await page.getByRole("button", { name: "Prefetch for my fleet" }).click();
  await expect(page.getByText(/State: finished\b/)).toBeVisible({ timeout: 180_000 });
  await pickForUpdate(page, app.url, [outdated.name]);
  await startUpdate(page);
  await expect(txnState(page)).toHaveText("Completed", { timeout: 200_000 });
}

/**
 * seedRunningTransaction writes the prototype's running update (TX-0143: five devices done, one
 * failed, one flashing, two downloading, three queued) and its history (TX-0139 to TX-0142)
 * straight into the database. A real update
 * cannot be held in that mix. The engine never touches these rows: it resumes active jobs only at
 * its start and schedules queued ones only when a submission or a finishing job wakes it, and the
 * read-only stack this runs on submits nothing afterwards (the entries that start work have their
 * own stack). Times are relative to now; the log of the failed device reads like a real one.
 */
export async function seedRunningTransaction(app: App, ids: Map<string, string>, fx: DesignFixture): Promise<void> {
  const tx = fx.transaction;
  const state: Record<string, string> = { done: "succeeded", failed: "failed", flashing: "upgrade_issued", downloading: "set_ota_url", queued: "queued" };
  const path = ["queued", "preflight", "backup", "set_ota_url", "upgrade_issued", "await_reboot", "verify", "succeeded"];
  const events: Record<string, string> = { preflight: "start", backup: "identity", set_ota_url: "backup_done", upgrade_issued: "command_ok", await_reboot: "probe_offline", verify: "probe_online", succeeded: "probe_online" };
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const by = (db.prepare("SELECT created_by FROM ota_jobs ORDER BY id LIMIT 1").get() as { created_by: string } | undefined)?.created_by ?? "maren";
    const t0 = Date.now() - 4 * 60_000;
    const batch = Number(tx.id.replace(/\D/g, ""));
    db.prepare("INSERT INTO ota_batches (id, created_by, created_at) VALUES (?, ?, ?)").run(batch, by, t0);
    const job = db.prepare(
      `INSERT INTO ota_jobs (batch_id, device_id, state, stage, policy, target_release, target_variant, identity_before, last_error, created_by, created_at, updated_at, batch_parallel)
       VALUES (?, ?, ?, 'final', 'only-newer', ?, '', ?, ?, ?, ?, ?, 3)`,
    );
    const event = db.prepare("INSERT INTO ota_job_events (job_id, at, from_state, to_state, event, detail) VALUES (?, ?, ?, ?, ?, ?)");
    // The prototype's history below the running one: TX-0139 to TX-0142. The real finished update
    // of seedFinishedUpdate becomes TX-0140, the others are finished rows of other devices.
    const real = (db.prepare("SELECT id FROM ota_batches WHERE id < 139 ORDER BY id DESC LIMIT 1").get() as { id: number } | undefined)?.id;
    const day = 86_400_000;
    const old = (id: number, at: number, who: string) => db.prepare("INSERT INTO ota_batches (id, created_by, created_at) VALUES (?, ?, ?)").run(id, who, at);
    if (real !== undefined) {
      old(140, t0 - 3 * day, by);
      db.prepare("UPDATE ota_jobs SET batch_id = 140, created_at = ?, updated_at = ? WHERE batch_id = ?").run(t0 - 3 * day, t0 - 3 * day + 60_000, real);
      db.prepare("DELETE FROM ota_batches WHERE id = ?").run(real);
    }
    const finished = db.prepare(
      `INSERT INTO ota_jobs (batch_id, device_id, kind, commands, state, stage, target_release, target_variant, created_by, created_at, updated_at)
       VALUES (?, ?, ?, ?, ?, 'final', '', '', ?, ?, ?)`,
    );
    const others = fx.devices.filter((d) => d.status === "online" && !tx.devices.some((t) => t.name === d.name)).map((d) => Number(ids.get(d.name)));
    const history: [number, string, string, string, number, string, number][] = [
      [142, "policy", "TelePeriod 300", "succeeded", 6, by, t0 - day / 2],
      [141, "backup", "", "succeeded", Math.min(58, others.length), "scheduler", t0 - day / 4],
      [139, "command", "Restart 1", "cancelled", 4, "jonas", t0 - 4 * day],
    ];
    for (const [id, kind, cmds, st, n, who, at] of history) {
      old(id, at, who);
      for (const dev of others.slice(0, n)) finished.run(id, dev, kind, cmds, st, who, at, at + 60_000);
    }
    tx.devices.forEach((d, i) => {
      const deviceId = ids.get(d.name);
      if (!deviceId) throw new Error(`seedRunningTransaction: ${d.name} is not in the app`);
      const to = state[d.state];
      const at = t0 + i * 3_000;
      const failed = to === "failed";
      const res = job.run(batch, Number(deviceId), to, tx.release, JSON.stringify({ Version: d.from }), failed ? "device did not come back" : "", by, at, at + 60_000);
      const id = Number(res.lastInsertRowid);
      // the way a job takes through the states, up to where this one stands (a failed one died waiting for the reboot)
      const last = failed ? path.indexOf("await_reboot") : path.indexOf(to);
      for (let k = 1; k <= last; k++) event.run(id, at + k * 6_000, path[k - 1], path[k], events[path[k]], "");
      if (failed) event.run(id, at + (last + 1) * 6_000, "await_reboot", "failed", "timeout", "device did not come back");
    });
  } finally {
    db.close();
  }
}
