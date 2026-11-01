import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Config restore through the device's own restore upload (GET /rs, POST /u2; #430), against
// a simulated device: backup, restore with confirmation, wait for the reboot,
// automatic verification. The decoder is off here, so verification compares
// the dumps byte for byte. No firmware listener is configured: a restore needs none.
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
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const operator = { "X-authentik-username": "olivia", "X-authentik-groups": "ops" };
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

test("admin restores a backup; the job ends verified", { tag: "@core" }, async ({ page }) => {
  test.setTimeout(120_000);
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "plug-4m", exact: true }).click();
  await page.getByRole("button", { name: "Back up", exact: true }).click(); // the device header's button
  await expect(page.getByText(/Backup taken/).first()).toBeVisible({ timeout: 30_000 });

  await page.locator("nav.utabs").getByRole("link", { name: "Backups" }).click();
  await page.getByRole("link", { name: "Restore…" }).first().click();
  await expect(page.getByRole("heading", { name: "Restore backup" })).toBeVisible();
  await expect(page.getByText(/^plug-4m · backup from/)).toBeVisible();

  // without the confirmation nothing starts
  await page.getByRole("button", { name: "Restore and restart" }).click();
  await expect(page.getByText("Tick the box to confirm.")).toBeVisible();

  // the decoder is off here, so the chip family cannot be checked: the ONE confirmation names it
  await expect(page.locator("label", { hasText: /I understand/ })).toContainText(/same chip family/);
  await page.getByLabel(/I understand/).check();
  await page.getByRole("button", { name: "Restore and restart" }).click();
  await expect(page.getByRole("heading", { name: "Restore backup" })).toBeVisible();
  await expect(page).toHaveURL(/\/restores\/\d+$/);
  await expect(page.locator("#restore-status")).toContainText("Restore finished.", { timeout: 90_000 });
  await expect(page.locator("#restore-status")).toContainText("Verified");
  // every step is ticked off
  await expect(page.locator("#restore-status .rs-step.is-done")).toHaveCount(5);
});

test("operators cannot restore or delete backups", { tag: "@core" }, async ({ page, request }) => {
  await page.setExtraHTTPHeaders(operator);
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "plug-4m", exact: true }).click();
  await page.locator("nav.utabs").getByRole("link", { name: "Backups" }).click();
  await expect(page.getByRole("link", { name: /^Restore/ })).toHaveCount(0);
  await expect(page.locator(".bk-acts a")).toHaveCount(0); // no download, restore or delete for operators
  const r = await request.get(app.url + "/backups/1/restore", { headers: operator, maxRedirects: 0 });
  expect(r.status()).toBe(403);
});
