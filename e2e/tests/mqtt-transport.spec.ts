import type { Page } from "@playwright/test";
import { join } from "node:path";
import { test, expect } from "../support/fixtures";
import { quickPick } from "../support/updates";
import { expectNoOverflow } from "../support/layout";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Console and "Make compliant" over MQTT (0.20.0, #79, #256): mq-esp32 is a simulated
// device whose HTTP port refuses every connection but which is on the broker. TasmoFleet reaches it
// only through MQTT (the second device, mq-esp8266-1m, is the same kind).
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv(["-only-mqtt-transport"]);
  const fwPort = await freePort(); // the transaction pages come with the firmware server
  app = await startApp({
    env: {
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_URL,
      TASMOFLEET_POLICY_RULES: JSON.stringify([{ command: "PowerOnState", want: "2", note: "e2e target" }]),
    },
  });
  // two devices: the Columns menu (used to see when the group topics are read) needs a second row
  const only = {
    "mq-esp32": env.devices["mq-esp32"],
    "mq-esp8266-1m": env.devices["mq-esp8266-1m"],
    "mq-fw-blocked": env.devices["mq-fw-blocked"],
    "mq-esp32-dual": env.devices["mq-esp32-dual"],
  };
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(only)])).toContain("created: mq-esp32");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

test("console over MQTT on a device HTTP cannot reach", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  await expect(page.getByRole("row").filter({ hasText: /mq-esp32(?!-)/ }).getByText("Online", { exact: true })).toBeVisible({ timeout: 30_000 });
  await page.getByRole("link", { name: "mq-esp32", exact: true }).click();
  await page.getByRole("link", { name: "Console" }).click();
  const box = page.getByRole("textbox", { name: "Command" });
  // The device becomes ours on the broker a moment after the import: the first send may still fail.
  await expect(async () => {
    await box.fill("Status 2");
    await page.getByRole("button", { name: "Send" }).click();
    await expect(page.getByText("StatusFWR")).toBeVisible({ timeout: 3_000 });
  }).toPass({ timeout: 60_000 });

  // A command that runs other commands could send Status 0 on our behalf: not over MQTT.
  await box.fill('Json {"Status":0}');
  await page.getByRole("button", { name: "Send" }).click();
  // Json is routine for an admin (no confirmation screen): the refusal is the device error text.
  await expect(page.getByRole("alert")).toBeVisible();
  await expect(page.getByText("StatusMEM")).toHaveCount(0); // what a Status 0 reply would have shown

  // Changing the topic of a device that only MQTT reaches can cut it off for good: it asks once.
  await box.fill("Topic somewhere-else");
  await page.getByRole("button", { name: "Send" }).click();
  await expect(page.getByText("reached over MQTT only")).toBeVisible();
});

test("Make compliant over MQTT", { tag: "@core" }, async ({ page }) => {
  test.setTimeout(300_000);
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  await expect(page.getByRole("row").filter({ hasText: /mq-esp32(?!-)/ }).getByText("Online", { exact: true })).toBeVisible({ timeout: 30_000 });
  // A write over MQTT needs every device's group topic (a message to a shared topic would reach other
  // devices). TasmoFleet reads them by itself shortly after the devices are known, and a change before
  // that is refused with the reason. So: try, and try again until the reads are in.
  const fixedRow = () => page.locator("table.audittable tr", { hasText: "Make compliant (device)" }).filter({ hasText: "PowerOnState" }).filter({ hasNotText: /refused|fail|unreachable/i });
  await expect(async () => {
    await page.goto(app.url + "/audit"); // a round may have fixed the device and failed on the other one
    if ((await fixedRow().count()) > 0) return;
    await page.goto(app.url + "/policy");
    await page.getByRole("button", { name: "Run now" }).click();
    await expect(page.locator(".banner", { hasText: /deviates? from a required rule/ })).toBeVisible({ timeout: 20_000 });
    await page.getByRole("button", { name: "Make compliant…" }).click();
    const dlg = page.locator("dialog#dlg-fix");
    await expect(dlg).toBeVisible();
    // The devices take the DeviceName they report after a while, so the plan is not matched by name.
    await expect(dlg.locator(".udlg-changes > div").first()).toContainText("PowerOnState 2");
    await dlg.getByRole("button", { name: /^Run \d+ changes?$/ }).click();
    await expect(page).toHaveURL(/\/transactions\/\d+/);
    await expect(page.locator(".upd-dev").getByText("Applied", { exact: true }).first()).toBeVisible({ timeout: 15_000 });
  }).toPass({ timeout: 240_000, intervals: [5_000] });
  await page.goto(app.url + "/audit");
  // The fix was written and succeeded; the audit says it travelled over MQTT (the device's HTTP port
  // refuses every connection).
  await expect(fixedRow().first()).toBeVisible();
  const detail = page.locator("table.audittable tr.detail-row", { hasText: "PowerOnState" }).filter({ hasText: /via\W+mqtt/ });
  expect(await detail.count()).toBeGreaterThan(0);
});

// Update over MQTT (0.20.0, #79, #259). The devices answer nothing over HTTP, so the preview probes
// them over MQTT; the language build cannot be read there, so the person says it; the update runs the
// same engine with the job pinned to MQTT. The backup before flashing runs over MQTT too (#257), so each
// device gets its MQTT password first (the devenv devices have the same one, the broker takes any).
// The devices take the DeviceName they report, so the picker is addressed by device id: the import
// creates them in the order of the object (mq-esp32 = 1, mq-esp8266-1m = 2, mq-fw-blocked = 3,
// mq-esp32-dual = 4).
const deviceId = { "mq-esp32": 1, "mq-esp8266-1m": 2, "mq-fw-blocked": 3, "mq-esp32-dual": 4 } as const;
const withPassword = new Set<number>();
async function ensurePassword(page: import("@playwright/test").Page, id: number) {
  if (withPassword.has(id)) return;
  await page.goto(app.url + `/devices/${id}/edit`);
  await page.getByLabel("MQTT password source").selectOption("own");
  await page.getByLabel("Own MQTT password").fill("devenv-fake-mqtt-password");
  await page.getByRole("button", { name: "Save changes" }).click();
  await expect(page).toHaveURL(/ok=saved/);
  withPassword.add(id);
}

async function pickById(page: import("@playwright/test").Page, name: keyof typeof deviceId) {
  await ensurePassword(page, deviceId[name]);
  await page.goto(app.url + "/updates/new");
  await quickPick(page, "None");
  await page.locator(`input[name=id][value="${deviceId[name]}"]`).check();
}

async function reviewMQTTUpdate(page: import("@playwright/test").Page, name: keyof typeof deviceId) {
  const row = () => page.locator(".upd-prow");
  // HTTP counts as failing, and every device's group topic as read, only after the first health polls
  // and reads: review again until the device is probed over MQTT. A short action timeout lets one
  // failed round end (the picker lists a device as Offline until then) instead of eating the whole wait.
  page.setDefaultTimeout(8_000);
  await expect(async () => {
    await pickById(page, name);
    await page.locator("label.upd-relopt", { hasText: "Tasmota 16.1.0" }).click();
    await page.getByRole("button", { name: "Review update" }).click();
    await expect(row()).toBeVisible();
    await expect(row()).not.toContainText("probe failed", { timeout: 5_000 });
  }).toPass({ timeout: 150_000, intervals: [5_000] }).catch(async (e) => {
    throw new Error(`${e}\npicker/preview page: ${(await page.locator("main").innerText().catch(() => "?")).slice(0, 1500)}`);
  });
  page.setDefaultTimeout(30_000);
  return row();
}

test("update over MQTT (staged ESP8266) from the preview", { tag: "@core" }, async ({ page }) => {
  test.setTimeout(420_000);
  await page.setExtraHTTPHeaders(admin);
  const row = await reviewMQTTUpdate(page, "mq-esp8266-1m");
  // Nothing is guessed: the language build is the person's choice, and without it nothing starts.
  await expect(row).toContainText("Language build of this device");
  await expect(row.locator("select")).toHaveValue("");
  await expect(page.getByRole("button", { name: "Start update", exact: true })).toHaveCount(0);
  await row.locator("select").selectOption("EN");
  await page.getByRole("button", { name: "Recheck" }).click();
  const again = page.locator(".upd-prow");
  await expect(again).toContainText("Over MQTT");
  await expect(again).toContainText("Staged: minimal firmware first");
  await page.getByRole("button", { name: "Start update", exact: true }).click();
  await expect(page).toHaveURL(/\/transactions\/\d+$/);
  await expect(page.locator(".upd-row").getByText("Done", { exact: true })).toBeVisible({ timeout: 300_000 });
  await expect(page.locator(".upd-row"), "the row names the path").toContainText("· over MQTT");
});

// FULL: a device that cannot reach the firmware server fails the update at once with that reason, and
// from then on the preview says there is no download path (until Forget or 24 h).
test("fw-blocked device shows no download path", async ({ page }) => {
  test.setTimeout(420_000);
  await page.setExtraHTTPHeaders(admin);
  const row = await reviewMQTTUpdate(page, "mq-fw-blocked");
  await row.locator("select").selectOption("EN");
  await page.getByRole("button", { name: "Recheck" }).click();
  await page.getByRole("button", { name: "Start update", exact: true }).click();
  await expect(page).toHaveURL(/\/transactions\/\d+$/);
  await expect(page.locator(".upd-row")).toContainText(/Failed|Done/, { timeout: 200_000 }).catch(async (e) => {
    throw new Error(`${e}\nlog: ${(await page.locator("#log-lines").innerText().catch(() => "?")).slice(0, 1500)}`);
  });
  await expect(page.locator("#log-lines"), "the job's log names the reason").toContainText("cannot reach the firmware server", { timeout: 5_000 });
  await pickById(page, "mq-fw-blocked");
  await page.locator("label.upd-relopt", { hasText: "Tasmota 16.1.0" }).click();
  await page.getByRole("button", { name: "Review update" }).click();
  const blocked = page.locator(".upd-prow");
  await blocked.locator("select").selectOption("EN"); // the language is asked first, the path is judged after it
  await page.getByRole("button", { name: "Recheck" }).click();
  await expect(page.locator(".upd-prow")).toContainText("no path");
  await expect(page.getByRole("button", { name: "Start update", exact: true })).toHaveCount(0);
});

// FULL: the same device without a language choice is not guessed.
test("language choice required", async ({ page }) => {
  test.setTimeout(240_000);
  await page.setExtraHTTPHeaders(admin);
  const row = await reviewMQTTUpdate(page, "mq-esp32");
  // An ESP32 with the dual-core tasmota32 build has language builds too; whichever the row shows, it
  // must be a choice and not a pre-filled value.
  await expect(row.locator("select")).toHaveValue("");
});

// #257: backup over MQTT (FileDownload). The device's HTTP port refuses everything, so the backup can
// only come over MQTT, and only with the device's MQTT password (the devenv devices have one; the
// broker takes any).
test("backup over MQTT", async ({ page }) => {
  test.setTimeout(300_000);
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  // Either device will do (both are HTTP-dead, on the broker and have the devenv password); the earlier
  // tests may have renamed them to what they report, so the first row is taken, not a name.
  await expect(page.getByText("Online", { exact: true }).first()).toBeVisible({ timeout: 30_000 });
  await page.locator("table[data-table=devices] tbody tr a").first().click();
  const devicePath = new URL(page.url()).pathname;

  const backUp = page.getByRole("button", { name: "Back up", exact: true });
  await page.goto(app.url + devicePath + "/edit");
  await page.getByLabel("MQTT password source").selectOption("own");
  await page.getByLabel("Own MQTT password").fill("devenv-fake-mqtt-password");
  await page.getByRole("button", { name: "Save changes" }).click();

  // The group topics of both devices are read shortly after start; until then the backup is refused with the
  // reason, which starts only a 5 s floor, so retrying every 6 s is a bounded wait for those reads only.
  await expect(async () => {
    await page.goto(app.url + devicePath);
    await backUp.click();
    await expect(page.getByText("Backup taken.")).toBeVisible({ timeout: 5_000 });
  }).toPass({ timeout: 300_000, intervals: [6_000] });

  await page.goto(app.url + devicePath + "/backups");
  await expect(page.locator("section.bk-list")).toBeVisible();
  await expect(page.getByText("No backups yet")).toHaveCount(0);
  const audit = await page.request.get(app.url + "/audit", { headers: admin });
  expect(await audit.text()).not.toContain("devenv-fake-mqtt-password");
});
// The device page's connection card (#261): mq-esp32 refuses every HTTP connection, so the router
// remembers the failure and the card, the buttons and the list say how it is reached instead.
const olga = { "X-authentik-username": "olga", "X-authentik-groups": "ops" };

async function openDevicePage(page: Page, headers: Record<string, string>): Promise<void> {
  await page.setExtraHTTPHeaders(headers);
  // The devenv imports in a fixed order into a fresh database, so mq-esp32 is device 1; it takes the
  // name it reports after a while, so go by the id and check it is the one we mean (by address).
  await page.goto(app.url + "/devices/1");
  await expect(page.locator(".devhd-meta")).toContainText(env.devices["mq-esp32"].addr);
  // the failure is remembered after the first HTTP call that fails: reload until the card says so
  await expect(async () => {
    await page.reload();
    await expect(page.locator("#devpaths-facts-sum")).toContainText("HTTP not reachable", { timeout: 2_000 });
  }).toPass({ timeout: 60_000, intervals: [3_000] });
}

test("connection card explains paths", async ({ page }) => {
  await openDevicePage(page, admin);
  await page.locator("#devpaths-facts-sum").click();
  await page.locator("#devpaths-actions-sum").click();
  const card = page.locator("#devpaths");
  await expect(card.locator("#devpaths-facts").getByText("Not reachable")).toBeVisible();
  await expect(card.locator("#devpaths-facts").getByText("Experimental")).toBeVisible();
  // commands go over MQTT, and the list marks the device
  await expect(card.locator("#devpaths-actions").getByText("Over MQTT, because", { exact: false }).first()).toBeVisible();
  await page.goto(app.url + "/devices");
  await expect(page.locator("[data-via-mqtt]").first()).toBeVisible();
});

test("disabled actions carry the reason", async ({ page }) => {
  await openDevicePage(page, admin);
  await page.locator("#devpaths-actions-sum").click();
  const row = page.locator("#devpaths-actions .ukv-row", { hasText: "Back up" }).first();
  const reason = (await row.locator(".devcard-note").innerText()).trim();
  const button = page.getByRole("button", { name: "Back up", exact: true });
  // without a way to back it up over MQTT the button says the same sentence as the card
  if (/^No path/.test(reason)) {
    await expect(button).toBeDisabled();
    await expect(button).toHaveAttribute("title", reason);
    await expect(page.locator("#devhd-why")).toContainText(reason);
  } else {
    await expect(button).toBeEnabled();
  }
});

test("operators see the card read-only", async ({ page }) => {
  await openDevicePage(page, olga);
  await page.locator("#devpaths-facts-sum").click();
  await expect(page.locator("#devpaths-facts")).toBeVisible();
  await expect(page.getByRole("button", { name: "Forget" })).toHaveCount(0);
});

test("an open fold stays open across a live refresh", async ({ page }) => {
  await openDevicePage(page, admin);
  const fold = page.locator("#devpaths details").first();
  await page.locator("#devpaths-facts-sum").click();
  await expect(fold).toHaveAttribute("open", "");
  const live = page.waitForResponse((r) => /\/devices\/\d+\/live$/.test(r.url()));
  await page.evaluate(() => {
    const el = document.querySelector("#devlive")!;
    (window as unknown as { htmx: { trigger(e: Element, n: string): void } }).htmx.trigger(el, "sse:resync");
  });
  await live;
  await expect(fold).toHaveAttribute("open", "");
});

for (const width of [375, 1280]) {
  test(`connection card stacks at ${width}px without sideways scroll`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await openDevicePage(page, admin);
    await page.locator("#devpaths-facts-sum").click();
    await page.locator("#devpaths-actions-sum").click();
    await expectNoOverflow(page, "connection card");
  });
}

// Upload over MQTT (experimental, 0.20.0, #79, #262): a person opts in per transaction, with the
// delivery "Push where possible" and a checkbox that exists only then.
const uploadBox = "Allow upload over MQTT (experimental: not tested on a real device)";

async function reviewUpload(page: import("@playwright/test").Page, name: keyof typeof deviceId) {
  const row = () => page.locator(".upd-prow");
  page.setDefaultTimeout(8_000);
  await expect(async () => {
    await pickById(page, name);
    await page.locator("label.upd-relopt", { hasText: "Tasmota 16.1.0" }).click();
    await page.getByLabel("Back up each device before flashing").uncheck();
    await page.locator("select[name=delivery]").selectOption("push");
    await page.getByLabel(uploadBox).check();
    await page.getByRole("button", { name: "Review update" }).click();
    await expect(row()).toBeVisible();
    await expect(row()).not.toContainText("probe failed", { timeout: 1_000 });
  }).toPass({ timeout: 150_000, intervals: [5_000] }).catch(async (e) => {
    throw new Error(`${e}\npicker/preview page: ${(await page.locator("main").innerText().catch(() => "?")).slice(0, 1500)}`);
  });
  page.setDefaultTimeout(30_000);
  return row();
}

// The language build is asked for only where it cannot be read: choose English when the row asks.
async function chooseLanguageIfAsked(page: Page, row: import("@playwright/test").Locator) {
  const pick = row.locator("select");
  if ((await pick.count()) > 0) {
    await pick.selectOption("EN");
    await page.getByRole("button", { name: "Recheck" }).click();
  }
}

// The layout of a device is the one a read of its /up page over HTTP recorded, and it is only proof for the
// hardware it was read from: TasmoFleet drops it when the chip, the flash sizes or the free flash change. The
// device never answers HTTP here, so the record is seeded, together with the identity the update job will
// store again (chip, flash, sketch flash, free flash) taken from the device's own Status 4 via the console,
// so the job's first identity write changes nothing and the proof stays.
async function seedProvenLayout(page: Page, name: keyof typeof deviceId) {
  const id = deviceId[name];
  await page.goto(app.url + `/devices/${id}`);
  await page.getByRole("link", { name: "Console" }).click();
  const box = page.getByRole("textbox", { name: "Command" });
  let text = "";
  await expect(async () => {
    await box.fill("Status 4");
    await page.getByRole("button", { name: "Send" }).click();
    await expect(page.getByText("StatusMEM").first()).toBeVisible({ timeout: 3_000 });
    text = await page.locator("main").innerText();
  }).toPass({ timeout: 90_000, intervals: [3_000] });
  const num = (key: string) => {
    const m = new RegExp(`"${key}":\\s*(\\d+)`).exec(text);
    if (!m) throw new Error(`Status 4 has no ${key}: ${text.slice(0, 400)}`);
    return Number(m[1]);
  };
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    db.prepare("UPDATE devices SET flash_size_kb=?, program_flash_kb=?, free_kb=?, up_layout='direct', up_layout_at=? WHERE id=?")
      .run(num("FlashSize"), num("ProgramFlashSize"), num("Free"), Date.now(), id);
  } finally {
    db.close();
  }
}

// @core: the refusal is deterministic and says why. An ESP8266 has no second stage over MQTT (its
// minimal firmware has no FileUpload), so the upload is never offered.
test("upload over MQTT is refused for a device where it does not fit, with the reason", { tag: "@core" }, async ({ page }) => {
  test.setTimeout(420_000);
  await page.setExtraHTTPHeaders(admin);
  const row = await reviewUpload(page, "mq-fw-blocked");
  await row.locator("select").selectOption("EN");
  await page.getByRole("button", { name: "Recheck" }).click();
  // The firmware server is not known to be unreachable for this device yet, so the download stays
  // possible; the upload is refused with its reason and never offered.
  await expect(page.locator(".upd-prow")).toContainText("no upload over MQTT: an ESP8266 cannot take an upload over MQTT");
  await expect(page.locator(".upd-prow")).not.toContainText("Push upload through the MQTT broker");
});

// FULL: the checkbox exists only together with "Push where possible".
test("checkbox only with Push where possible", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/updates/new");
  const box = page.getByLabel(uploadBox);
  await expect(box).toBeHidden();
  await page.locator("select[name=delivery]").selectOption("push");
  await expect(box).toBeVisible();
  await box.check();
  await page.locator("select[name=delivery]").selectOption("");
  await expect(box).toBeHidden();
  await page.locator("select[name=delivery]").selectOption("push");
  await expect(box, "a box hidden by the download delivery is cleared").not.toBeChecked();
});

// FULL: a dual-OTA ESP32 that HTTP and the firmware server do not reach takes the image through the broker.
test("upload over MQTT (experimental) updates a dual-OTA device", async ({ page }) => {
  test.setTimeout(480_000);
  await page.setExtraHTTPHeaders(admin);
  await seedProvenLayout(page, "mq-esp32-dual");
  const row = await reviewUpload(page, "mq-esp32-dual");
  await chooseLanguageIfAsked(page, row);
  await expect(page.locator(".upd-prow")).toContainText("Push upload through the MQTT broker");
  await page.getByRole("button", { name: "Start update", exact: true }).click();
  await expect(page).toHaveURL(/\/transactions\/\d+$/);
  await expect(page.locator(".upd-row").getByText("Done", { exact: true })).toBeVisible({ timeout: 300_000 }).catch(async (e) => {
    throw new Error(`${e}\nlog: ${(await page.locator("#log-lines").innerText().catch(() => "?")).slice(0, 1500)}`);
  });
  await expect(page.locator(".upd-row"), "the row names the path").toContainText("· upload over MQTT (experimental)");
});

// FULL: the same device without a recorded layout is refused with the reason (a safeboot-shaped Status 4
// looks like room, and an upload there could overwrite the running app).
test("upload over MQTT needs a proven dual-OTA layout", async ({ page }) => {
  test.setTimeout(300_000);
  await page.setExtraHTTPHeaders(admin);
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    db.prepare("UPDATE devices SET up_layout='', up_layout_at=NULL WHERE id=?").run(deviceId["mq-esp32"]);
  } finally {
    db.close();
  }
  const row = await reviewUpload(page, "mq-esp32");
  await chooseLanguageIfAsked(page, row);
  await expect(page.locator(".upd-prow")).toContainText("not proven dual-OTA");
  await expect(page.locator(".upd-prow")).not.toContainText("Push upload through the MQTT broker");
});
