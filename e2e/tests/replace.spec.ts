import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Backups (delete, delete older than) and "Replace device" against two
// simulated devices of the same chip family: plug-4m is the inventory entry
// whose hardware "died", plug-4m-http is the fresh board that is not in the
// inventory. No firmware listener is configured: a replace pushes the backup with the device's own
// restore upload and needs none (#430).
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_URL,
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir({ "plug-4m": env.devices["plug-4m"], "lamp-1m": env.devices["lamp-1m"] })]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const operator = { "X-authentik-username": "olivia", "X-authentik-groups": "ops" };
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

// pickNewDevice previews the replacement onto the board at host:port: from the discovered list
// when discovery lists it, otherwise typed in.
async function pickNewDevice(page: import("@playwright/test").Page, host: string, port: string) {
  const listed = page.locator(`select[name="target"] option[value="${host}:${port}"]`);
  if (await listed.count()) {
    await page.locator('select[name="target"]').selectOption(`${host}:${port}`);
  } else {
    const other = page.getByText("Enter an address instead");
    if (await other.count()) await other.click();
    await page.getByRole("textbox", { name: "Address" }).fill(host);
    await page.getByRole("textbox", { name: "Port" }).fill(port);
  }
  await page.getByRole("button", { name: "Preview what’s copied" }).click();
}

async function backUp(page: import("@playwright/test").Page, name: string) {
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name, exact: true }).click();
  await page.getByRole("button", { name: "Back up", exact: true }).click(); // the device header's button
  await expect(page.getByText(/Backup taken/).first()).toBeVisible({ timeout: 30_000 });
}

test("admin deletes a backup; the newest one is warned about; delete selected confirms first", async ({ page }) => {
  test.setTimeout(120_000);
  await page.setExtraHTTPHeaders(admin);
  // lamp-1m has its own backup cooldown; plug-4m is kept for the replace test
  await backUp(page, "lamp-1m");
  await page.locator("nav.utabs").getByRole("link", { name: "Backups" }).click();
  await expect(page.getByRole("link", { name: "Restore…" })).toBeVisible();

  // "Delete selected": the confirmation names the count and the newest backup before anything goes
  await page.locator(".bk-row").getByRole("checkbox").first().check();
  await expect(page.locator(".bk-selbar")).toContainText("1 selected");
  await page.getByRole("button", { name: "Delete selected" }).click();
  await expect(page.locator("section.confirm-card")).toContainText("This includes the newest backup");
  await page.getByRole("link", { name: "Cancel" }).click();
  await expect(page).toHaveURL(/\/devices\/\d+\/backups$/);

  // single delete: the dialog carries the extra warning because this is the newest
  const del = page.locator(".bk-acts a.bk-del").first();
  await del.click();
  const dlg = page.locator("dialog[open]");
  await expect(dlg).toContainText("newest backup of this device");
  await dlg.getByRole("button", { name: "Cancel" }).click();
  await expect(dlg).toHaveCount(0); // cancelled: the row is still there
  await expect(del).toBeVisible();

  // Without JavaScript the same link opens a confirmation page with the same warning; Cancel comes back.
  const href = await del.getAttribute("href");
  expect(href).toMatch(/\/backups\/\d+\/delete$/);
  await page.goto(app.url + href!.replace(/^\/tf/, ""));
  await expect(page.locator("section.confirm-card")).toContainText("newest backup of this device");
  await page.getByRole("link", { name: "Cancel" }).click();
  await expect(page).toHaveURL(/\/devices\/\d+\/backups$/);

  await page.locator(".bk-acts a.bk-del").first().click();
  await page.locator("dialog[open]").getByRole("button", { name: "Delete backup" }).click();
  await expect(page.getByText("Backup deleted.")).toBeVisible();
  await expect(page.getByText("No backups yet")).toBeVisible();
});

test("operators see no delete or replace controls and are refused", { tag: "@core" }, async ({ page, request }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "plug-4m", exact: true }).click();
  await expect(page.getByRole("link", { name: "Replace device" })).toHaveCount(0);
  for (const p of ["/devices/1/replace", "/devices/1/backups/prune", "/devices/1/backups/delete?id=1"]) {
    const r = await request.get(app.url + p, { headers: operator, maxRedirects: 0 });
    expect(r.status()).toBe(403);
  }
});

test("replace device: refused while the old one answers, then done with a new board", async ({ page }) => {
  test.setTimeout(180_000);
  await page.setExtraHTTPHeaders(admin);
  await backUp(page, "plug-4m");
  const spare = env.devices["plug-4m-http"].addr;
  const [host, port] = spare.split(":");

  await page.getByRole("link", { name: "Replace device" }).click();
  await pickNewDevice(page, host, port);
  await expect(page.getByText(/still answers at its address/)).toBeVisible();

  // the old board dies: its address no longer answers
  await page.goto(app.url + "/devices/1/edit");
  await page.getByRole("textbox", { name: "Address" }).fill("127.0.0.1");
  await page.getByRole("spinbutton", { name: "Port" }).fill("1");
  await page.getByRole("button", { name: /Save changes/ }).click();

  await page.goto(app.url + "/devices/1/replace");
  await pickNewDevice(page, host, port);
  await expect(page.getByText("Copied to the new device")).toBeVisible();
  await expect(page.locator("label", { hasText: /I understand/ })).toContainText(/backed up first/);
  await page.getByRole("button", { name: "Start replacement…" }).click();
  await expect(page.getByText("Tick the box to confirm.")).toBeVisible();
  await page.getByLabel(/I understand/).check();
  await page.getByRole("button", { name: "Start replacement…" }).click();
  await expect(page.getByRole("heading", { name: "Replace device" })).toBeVisible();
  await expect(page.locator("#restore-status")).toContainText("Replace finished.", { timeout: 120_000 });
  await expect(page.locator("#restore-status")).toContainText("now stands for the new device");

  // the entry kept its name and now points at the new board
  await page.goto(app.url + "/devices/1");
  await expect(page.getByRole("heading", { name: /plug-4m/ })).toBeVisible();
  await expect(page.locator("#devconn")).toContainText(spare); // the Connection card's address
});
