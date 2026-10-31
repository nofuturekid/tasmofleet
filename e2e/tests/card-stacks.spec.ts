import { test, expect } from "../support/fixtures";
import { cardStackGaps } from "../support/layout";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Design rule (#391): in every card grid the cards stack directly under each other per column; a
// short card next to a tall one leaves no empty gap below it. The check is generic (any card with
// more than the grid gap above it in its column fails), so a page that goes back to a row-aligned
// grid fails here without a page-specific assertion.
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
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
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_GITHUB_URL,
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

const tabs = ["general", "network", "mqtt", "discovery", "backups", "firmware", "signin", "advanced"];
// [name, path, largest gap between stacked cards]: the policy pages keep their own card gap (--card-gap, 20 to 24 px)
const pages: [string, string, number][] = [
  ...tabs.map((t): [string, string, number] => [`settings tab ${t}`, `/settings/system?tab=${t}`, 17]),
  ["about", "/about", 17],
  ["new policy rule", "/policy/rules/new", 25],
];

for (const width of [1280, 1000, 600, 375]) {
  for (const [name, path, maxGap] of pages) {
    test(`${name}: cards stack under each other at ${width}px`, async ({ page }) => {
      await page.setExtraHTTPHeaders(adam);
      await page.setViewportSize({ width, height: 900 });
      await page.goto(app.url + path);
      await expect(page.locator(".scard:visible, .about-card:visible, .devcard:visible").first()).toBeVisible();
      expect(await cardStackGaps(page, maxGap)).toEqual([]);
    });
  }
  test(`device settings tab: cards stack under each other at ${width}px`, async ({ page }) => {
    await page.setExtraHTTPHeaders(adam);
    await page.setViewportSize({ width, height: 900 });
    await page.goto(app.url + "/devices");
    const href = (await page.getByRole("link", { name: "esp32-dimmer", exact: true }).first().getAttribute("href"))!;
    await page.goto(app.url + href + "/settings");
    await expect(page.locator("section.gcard").first()).toBeVisible({ timeout: 30_000 });
    expect(await cardStackGaps(page)).toEqual([]);
  });
}
