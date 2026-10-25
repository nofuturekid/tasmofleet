import { deviceBox } from "../support/devicelist";
import { pickForUpdate, quickPick, startUpdate, txnState } from "../support/updates";
import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Mixed fleet from tasmofleet-devenv: MQTT and HTTP-only devices, ESP8266 1 MB
// (staged), ESP32 (direct), an unknown chip (must be refused). The firmware
// source is the devenv upstream, selected with FIRMWARE_UPSTREAM_OVERRIDE.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
  const out = app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
  expect(out).toContain("created: lamp-1m");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const operator = { "X-authentik-username": "olivia", "X-authentik-groups": "ops" };
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

test("mixed fleet view shows MQTT and HTTP-only devices online", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices");
  for (const name of ["lamp-1m", "esp32-dimmer", "plug-4m-http", "esp32s3-http", "mystery-chip"]) {
    await expect(page.getByRole("link", { name, exact: true })).toBeVisible();
  }
  for (const name of ["lamp-1m", "plug-4m-http"]) {
    const row = page.getByRole("row").filter({ hasText: name });
    await expect(row.getByText("Online", { exact: true })).toBeVisible({ timeout: 30_000 });
  }
});

// The picker on /updates/new: it starts with every outdated device that can be updated, the quick
// selection must leave out devices that cannot be updated, the plan beside it counts along, and the
// preview must then list exactly the chosen devices. Without JavaScript the same menu selects on the
// server.
async function waitAllReachable(page: import("@playwright/test").Page) {
  await page.goto(app.url + "/devices");
  for (const name of ["lamp-1m", "esp32-dimmer", "plug-4m-http", "esp32s3-http", "mystery-chip"]) {
    await expect(page.getByRole("row").filter({ hasText: name }).getByText("Online", { exact: true })).toBeVisible({ timeout: 30_000 });
  }
}


test("update picker: outdated preselected, quick selection, the plan counts along, preview lists the selection", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await waitAllReachable(page);
  await page.goto(app.url + "/updates/new");
  const boxes = page.locator("input[name=id]:not(:disabled)");
  const checked = page.locator("input[name=id]:checked");
  const plan = page.locator("[data-plan-devices]");
  const total = await boxes.count();
  expect(total).toBeGreaterThan(2);
  const outdated = await page.locator('[data-update="1"] input[name=id]:not(:disabled)').count();
  await expect(checked).toHaveCount(outdated);
  await expect(plan).toHaveText(String(outdated));

  await quickPick(page, "All that can be updated");
  await expect(checked).toHaveCount(total);
  await expect(plan).toHaveText(String(total));
  await expect(page.locator("[data-select-n]")).toHaveText(`${total} selected`);
  await boxes.first().uncheck();
  await expect(plan).toHaveText(String(total - 1));
  await quickPick(page, /^All ESP32-S3/);
  await expect(checked).toHaveCount(1);
  await quickPick(page, "None");
  await expect(checked).toHaveCount(0);
  await expect(plan).toHaveText("0");

  await quickPick(page, "All that can be updated");
  await page.locator("label.upd-relopt", { hasText: "Tasmota 16.1.0" }).click();
  await page.getByRole("button", { name: "Review update" }).click();
  await expect(page.getByRole("heading", { name: /^4 · Preview/ })).toBeVisible();
  await expect(page.locator(".upd-prow")).toHaveCount(total);
});

test("update picker without JavaScript: the quick selection works on the server", async ({ browser }) => {
  const ctx = await browser.newContext({ javaScriptEnabled: false, extraHTTPHeaders: admin });
  const page = await ctx.newPage();
  await page.goto(app.url + "/updates/new");
  const eligible = await page.locator("input[name=id]:not(:disabled)").count();
  await quickPick(page, "All that can be updated");
  await expect(page.locator("input[name=id]:checked")).toHaveCount(eligible);
  await quickPick(page, "None");
  await expect(page.locator("input[name=id]:checked")).toHaveCount(0);
  await ctx.close();
});

test("a device that is excluded from OTA is not selectable and says why", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "esp32-dimmer", exact: true }).click();
  await page.getByRole("link", { name: "Edit" }).click();
  await page.getByLabel("Exclude from OTA updates").check();
  await page.getByRole("button", { name: "Save changes" }).click();
  await page.goto(app.url + "/updates/new");
  const row = page.locator("label.upd-pick-row").filter({ hasText: "esp32-dimmer" });
  await expect(row.locator("input[name=id]")).toBeDisabled();
  await expect(row).toContainText("Excluded from updates");
  await quickPick(page, "All that can be updated");
  await expect(row.locator("input[name=id]")).not.toBeChecked();
  // undo, later tests use the device
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "esp32-dimmer", exact: true }).click();
  await page.getByRole("link", { name: "Edit" }).click();
  await page.getByLabel("Exclude from OTA updates").uncheck();
  await page.getByRole("button", { name: "Save changes" }).click();
});

test("device list: the bulk bar offers switching, asks first for several devices", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await waitAllReachable(page);
  const bar = page.getByRole("region", { name: "Selected devices" });
  await expect(bar).toBeHidden();
  await deviceBox(page, "lamp-1m").check();
  await expect(bar).toBeVisible();
  await expect(bar).toContainText("1 device selected");
  await deviceBox(page, "plug-4m-http").check();
  await expect(bar).toContainText("2 devices selected");
  await bar.getByRole("button", { name: "Power off", exact: true }).click();
  await expect(page.getByRole("button", { name: "Switch off 2 devices" })).toBeVisible(); // the scope page: nothing sent yet
  await page.getByRole("button", { name: "Switch off 2 devices" }).click();
  await expect(page.getByRole("heading", { name: "Power off finished", level: 1 })).toBeVisible();
  await expect(page.locator(".bk-rrow").filter({ hasText: "lamp-1m" })).toContainText("Succeeded");
  await expect(page.locator(".bk-rrow").filter({ hasText: "plug-4m-http" })).toContainText("Succeeded");
  await page.goto(app.url + "/devices");
  await deviceBox(page, "lamp-1m").check();
  await bar.getByRole("button", { name: "Clear", exact: true }).click();
  await expect(bar).toBeHidden();
});

// Switching a relay must not move the table: columns have fixed widths and
// only the power cell is swapped.
test("toggling a relay does not move the device table", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await waitAllReachable(page);
  const box = () => page.locator("table.devtable").evaluate((t) => {
    const r = t.getBoundingClientRect();
    const cells = [...t.querySelectorAll("tbody tr:first-child td")].map((c) => Math.round(c.getBoundingClientRect().width));
    return { x: r.x, y: r.y, w: r.width, h: r.height, cells };
  });
  const row = page.getByRole("row").filter({ hasText: "esp32s3-http" });
  const button = row.getByRole("button", { name: /Toggle relay 1/ });
  const before = await box();
  const pressed = (await button.getAttribute("aria-pressed"))!;
  await button.click();
  await expect(row.getByRole("button", { name: /Toggle relay 1/ })).not.toHaveAttribute("aria-pressed", pressed);
  const after = await box();
  expect(Math.abs(after.x - before.x)).toBeLessThanOrEqual(1);
  expect(Math.abs(after.y - before.y)).toBeLessThanOrEqual(1);
  expect(Math.abs(after.w - before.w)).toBeLessThanOrEqual(1);
  expect(Math.abs(after.h - before.h)).toBeLessThanOrEqual(1);
  expect(after.cells).toEqual(before.cells);
});

// Retry: cancel a just-started update, then retry it. The retry is a new
// transaction linked both ways, planned afresh; the original stays as it was.
test("a cancelled transaction can be retried; both transactions link to each other", async ({ page }) => {
  test.setTimeout(240_000);
  await page.setExtraHTTPHeaders(operator);
  await waitAllReachable(page);
  await pickForUpdate(page, app.url, ["esp32c3-sensor"]);
  await page.locator("label.upd-relopt", { hasText: "Tasmota 16.1.0" }).click();
  await startUpdate(page);
  const original = page.url();
  await page.getByRole("link", { name: "Cancel remaining" }).click();
  await page.locator("dialog[open]").getByRole("button", { name: "Cancel remaining" }).click();
  await expect(page.locator("#batch-summary")).toContainText("1 cancelled", { timeout: 60_000 });
  await expect(page.getByRole("link", { name: "Retry failed" })).toBeVisible({ timeout: 30_000 });
  await page.getByRole("link", { name: "Retry failed" }).click();
  await expect(page.getByRole("button", { name: /Start retry of 1 device/ })).toBeVisible(); // confirmation first, nothing created yet
  await page.getByRole("button", { name: /Start retry of 1 device/ }).click();
  await expect(page).toHaveURL(/\/transactions\/\d+$/);
  const retry = page.url();
  expect(retry).not.toBe(original);
  await expect(page.getByText(/Retry of/)).toBeVisible();
  await page.goto(original);
  await expect(page.getByText(/Retried in/)).toBeVisible();
  await expect(page.getByRole("link", { name: /TX-\d+/ }).first()).toBeVisible();
  await page.goto(retry);
  await expect(txnState(page)).toHaveText("Completed", { timeout: 200_000 });
});

test("address links open the device page without credentials", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices");
  const row = page.getByRole("row").filter({ hasText: "lamp-1m" });
  const link = row.locator("a.ext");
  const [host, port] = env.devices["lamp-1m"].addr.split(":");
  await expect(link).toHaveAttribute("href", `http://${host}:${port}`);
  await expect(link).toHaveAttribute("target", "_blank");
  await expect(link).toHaveAttribute("rel", "noopener noreferrer");
  expect(await link.getAttribute("href")).not.toContain("@");
});

// Quarantined: https://github.com/nofuturekid/tasmofleet/issues/20
test("update batch: preview, staged ESP8266 and direct ESP32 succeed, unknown chip is refused", { tag: "@core" }, async ({ page, request }) => {
  test.setTimeout(240_000);
  const addr = env.devices["lamp-1m"].addr;
  const otaBefore = (await (await request.get(`http://${addr}/cm?cmnd=OtaUrl`)).json()).OtaUrl;
  await page.setExtraHTTPHeaders(operator);
  await pickForUpdate(page, app.url, ["lamp-1m", "esp32-dimmer", "mystery-chip"]);
  const lampId = await page.getByLabel("Select lamp-1m", { exact: true }).getAttribute("value");
  await page.locator("label.upd-relopt", { hasText: "Tasmota 16.1.0" }).click();
  await page.getByRole("button", { name: "Review update" }).click();

  const preview = (name: string) => page.locator(".upd-prow").filter({ hasText: name });
  await expect(preview("lamp-1m")).toContainText("Staged: minimal firmware first");
  await expect(preview("esp32-dimmer")).toContainText("→ 16.1.0");
  await expect(preview("esp32-dimmer")).not.toContainText("Staged");
  await expect(preview("mystery-chip")).toContainText(/refuse/i);

  await page.getByRole("button", { name: "Start update", exact: true }).click();
  await expect(page).toHaveURL(/\/transactions\/\d+$/);
  // Calm live view: while the update runs, no device row changes its size or position, so
  // nothing below it jumps (one line per row, the state in a box of fixed width).
  const layout = () => page.locator(".upd-row").evaluateAll((els) => els.map((e) => `${Math.round(e.getBoundingClientRect().top)}/${Math.round(e.getBoundingClientRect().height)}`));
  // The device behind the job is silent while it flashes: its page says "Updating", not a red failure.
  // The hold lasts as long as the job, about 15 s for the staged flow at the devenv's time scale (#20):
  // the device page is opened directly, not through the device list, whose click took 46 s on a
  // loaded CI runner, after the job had already succeeded.
  const second = await page.context().newPage();
  await second.setExtraHTTPHeaders(operator);
  await second.goto(`${app.url}/devices/${lampId}`);
  await expect(second.locator(".devhd-state")).toHaveText("Updating");
  const consoleTab = second.locator("nav.utabs [aria-disabled=true]", { hasText: "Console" });
  await expect(consoleTab, "the tab stays, disabled, with the reason as its title").toHaveAttribute("title", /Unavailable while/);
  await expect(second.getByRole("button", { name: "Back up", exact: true }), "nothing that needs the device is offered while it is held").toBeDisabled();
  await expect(second.getByRole("heading", { name: "Transactions" })).toBeVisible();
  // The Console page of that device: its tab cannot be used, but it is the page you are on, so it is not drawn grey.
  await second.goto(second.url() + "/console");
  await expect(second.locator("nav.utabs a[aria-current=page]")).toHaveText("Console");
  await expect(second.locator("nav.utabs a[aria-current=page]")).toHaveCSS("opacity", "1");
  await expect(second.locator("h1"), "the header is the same on every tab").toHaveText("lamp-1m");
  await second.close();
  const before0 = await layout();
  for (let i = 0; i < 6; i++) {
    await page.waitForTimeout(1000);
    expect(await layout(), "device rows must not move while the update runs").toEqual(before0);
  }
  for (const name of ["lamp-1m", "esp32-dimmer"]) {
    await expect(page.locator(".upd-row").filter({ hasText: name }).getByText("Done", { exact: true })).toBeVisible({ timeout: 200_000 });
  }

  // The log of a device is persisted with the job: timestamped lines, still there after a reload.
  await page.reload();
  await expect(txnState(page)).toHaveText("Completed");
  await expect(page.locator("#log-lines .upd-log-at").first()).toHaveText(/^\d\d:\d\d:\d\d$/);

  // the staged flow changes OtaUrl twice; it must be back where it was
  const ota = await (await request.get(`http://${addr}/cm?cmnd=OtaUrl`)).json();
  expect(ota.OtaUrl).toBe(otaBefore);
});

// Push where possible (F-9): an ESP32 gets the image by upload, an ESP8266 in the same
// transaction downloads it as usual; the preview says which and why, nothing is refused.
// The ESP32's OtaUrl is never touched.
test("push where possible: ESP32 is updated by upload, ESP8266 downloads, nothing refused", { tag: "@core" }, async ({ page, request }) => {
  test.setTimeout(240_000);
  const addr = env.devices["esp32s3-http"].addr;
  const otaBefore = (await (await request.get(`http://${addr}/cm?cmnd=OtaUrl`)).json()).OtaUrl;
  await page.setExtraHTTPHeaders(operator);
  await pickForUpdate(page, app.url, ["esp32s3-http", "plug-4m"]);
  await page.locator("label.upd-relopt", { hasText: "Tasmota 16.1.0" }).click();
  await page.locator("select[name=delivery]").selectOption("push");
  await page.getByRole("button", { name: "Review update" }).click();

  await expect(page.getByText(/uploads the file to 1 device; the others fetch/)).toBeVisible();
  const preview = (name: string) => page.locator(".upd-prow").filter({ hasText: name });
  await expect(preview("esp32s3-http")).toContainText("→ 16.1.0");
  await expect(preview("esp32s3-http")).toContainText("Push upload.");
  await expect(preview("plug-4m")).toContainText("→ 16.1.0");
  await expect(preview("plug-4m")).toContainText(/Downloads instead: .*ESP8266/);

  await page.getByRole("button", { name: "Start update", exact: true }).click();
  await expect(page).toHaveURL(/\/transactions\/\d+$/);
  await expect(page.locator(".upd-row").filter({ hasText: "esp32s3-http" })).toContainText("· upload");
  await expect(page.locator(".upd-row").filter({ hasText: "plug-4m" })).not.toContainText("· upload");
  for (const name of ["esp32s3-http", "plug-4m"]) {
    await expect(page.locator(".upd-row").filter({ hasText: name }).getByText("Done", { exact: true })).toBeVisible({ timeout: 200_000 });
  }
  const ota = await (await request.get(`http://${addr}/cm?cmnd=OtaUrl`)).json();
  expect(ota.OtaUrl, "a push update must not touch OtaUrl").toBe(otaBefore);
  const status = await (await request.get(`http://${addr}/cm?cmnd=Status%202`)).json();
  expect(status.StatusFWR.Version).toContain("16.1.0");
});

test("backup now, download is admin only", async ({ page, request }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "plug-4m", exact: true }).click();
  await page.getByRole("button", { name: "Back up", exact: true }).click(); // the device header's button
  await expect(page.getByText(/Backup taken/).first()).toBeVisible({ timeout: 30_000 });
  const id = (await page.url().match(/devices\/(\d+)/))![1];
  await page.goto(`${app.url}/devices/${id}/backups`);
  // design v2 (P10): the backup row shows its actions inline (.bk-acts), no row menu
  await expect(page.locator(".bk-acts")).not.toHaveCount(0);
  await expect(page.locator(".bk-acts a", { hasText: "Download" })).toHaveCount(0);

  await page.setExtraHTTPHeaders(admin);
  await page.goto(`${app.url}/devices/${id}/backups`);
  const href = await page.locator(".bk-acts a", { hasText: "Download" }).first().getAttribute("href");
  const asOperator = await request.get(app.url + href!.replace(/^\/tasmofleet/, ""), { headers: operator, maxRedirects: 0 });
  expect(asOperator.status()).toBe(403);
  const asAdmin = await request.get(app.url + href!, { headers: admin });
  expect(asAdmin.status()).toBe(200);
  // the file name says which device it is from (#55), not "<stamp>-dmp.dmp"
  expect(asAdmin.headers()["content-disposition"]).toMatch(/^attachment; filename="plug-4m-\d{8}T\d{6}Z(-snapshot|-decoded)?\.(dmp|json)"$/);
});
