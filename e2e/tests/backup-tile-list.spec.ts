import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Why (#381): the "Devices without a backup" tile on Settings -> Backups is a number nobody can
// act on unless it opens the devices it counts. The click must land on exactly those devices,
// and the tile must work from the keyboard like the dashboard tiles.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
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

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

test("the Devices without a backup tile opens exactly those devices", async ({ page }) => {
  test.setTimeout(90_000);
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  const total = await page.locator("table[data-table=devices] tbody tr").count();
  expect(total, "several devices").toBeGreaterThan(1);
  await page.getByRole("link", { name: "plug-4m", exact: true }).click();
  const id = page.url().match(/devices\/(\d+)/)![1];
  const csrf = await page.locator("input[name=csrf]").first().inputValue();
  const took = await page.request.post(`${app.url}/devices/${id}/backup`, { headers: admin, form: { csrf }, maxRedirects: 0 });
  expect(took.status(), "backup taken").toBe(303);

  await page.goto(app.url + "/settings/system?tab=backups");
  const tile = page.getByRole("link", { name: /Devices without a backup/ });
  await expect(tile.locator(".sys-sum-value")).toHaveText(String(total - 1));
  await tile.focus();
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/\/devices\?chip=nobackup/);
  const rows = page.locator("table[data-table=devices] tbody tr");
  await expect(rows).toHaveCount(total - 1);
  await expect(page.getByRole("link", { name: "plug-4m", exact: true })).toHaveCount(0);
  await expect(page.locator(".dv-chip[aria-current=true]")).toContainText("No backup");
});
