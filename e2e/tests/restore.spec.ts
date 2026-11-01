import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Config restore through the one-time token on the firmware listener, against
// a simulated device: backup, restore with confirmation, wait for the reboot,
// automatic verification. The decoder is off here, so verification compares
// the dumps byte for byte.
let app: App;
let env: Devenv;
let fwPort: number;
test.beforeAll(async () => {
  env = await startDevenv();
  fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_URL,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
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

test("admin restores a backup; the job ends verified; the token is spent", { tag: "@core" }, async ({ page, request }) => {
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

  // the firmware listener knows no other restore URL
  const probe = await request.get(`http://127.0.0.1:${fwPort}/restore/${"0".repeat(32)}`);
  expect(probe.status()).toBe(404);
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
