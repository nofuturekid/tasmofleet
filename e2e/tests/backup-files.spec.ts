import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Why (#324): a run's files are shown in a fold on the device's Backups tab, reachable from the
// keyboard; the download of the archive is admin-only (it holds tlskey and Berry scripts).
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
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
const operator = { "X-authentik-username": "olivia", "X-authentik-groups": "ops" };

test("the Files fold opens from the keyboard and the archive download is admin only", async ({ page }) => {
  test.setTimeout(90_000);
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices");
  await page.getByRole("link", { name: "esp32-dimmer", exact: true }).click();
  const id = page.url().match(/devices\/(\d+)/)![1];
  const csrf = await page.locator("input[name=csrf]").first().inputValue();
  const took = await page.request.post(`${app.url}/devices/${id}/backup`, { headers: admin, form: { csrf }, maxRedirects: 0 });
  expect(took.status(), "backup taken").toBe(303);
  await page.goto(`${app.url}/devices/${id}/backups`);

  const fold = page.locator(".bk-row details.bk-files");
  await expect(fold).toHaveCount(1);
  await expect(fold.locator("summary .meta")).toContainText("stored");
  await expect(fold).not.toHaveAttribute("open", "");
  await expect(fold.locator(".bk-fl")).toBeHidden();

  // keyboard: Enter opens, Space closes
  await fold.locator("summary").focus();
  await page.keyboard.press("Enter");
  await expect(fold).toHaveAttribute("open", "");
  await expect(fold.locator(".bk-fl-path", { hasText: "autoexec.be" })).toBeVisible();
  await expect(fold.locator(".bk-fl-row", { hasText: "tlskey" }).getByText("Private key")).toBeVisible();
  await expect(fold.locator(".bk-fl-note")).toContainText("Contents are never shown");
  await page.keyboard.press("Space");
  await expect(fold).not.toHaveAttribute("open", "");

  // admin: the archive downloads as a tar named after the device, and the content is the device's file
  await fold.locator("summary").click();
  const href = await fold.getByRole("link", { name: "Download files (.tar)" }).getAttribute("href");
  const dl = await page.request.get(app.url + href!, { headers: admin });
  expect(dl.status()).toBe(200);
  expect(dl.headers()["content-disposition"]).toContain("-files.tar");
  expect((await dl.body()).subarray(0, 25).toString()).toBe(".tasmofleet-manifest.json");

  // operator: names and sizes, no download, and the route refuses
  await page.setExtraHTTPHeaders(operator);
  await page.goto(`${app.url}/devices/${id}/backups`);
  await page.locator(".bk-row details.bk-files summary").click();
  await expect(page.locator(".bk-fl-path", { hasText: "autoexec.be" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Download files (.tar)" })).toHaveCount(0);
  const refused = await page.request.get(app.url + href!, { headers: operator });
  expect(refused.status()).toBe(403);
});
