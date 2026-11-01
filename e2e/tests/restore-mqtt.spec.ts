import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

// Restore over MQTT (0.20.0, #79): mq-esp32 is a simulated device whose HTTP port refuses every
// connection but which is on the broker. The app runs WITHOUT a firmware listener, so the only way
// to restore is FileUpload; the backup it restores comes over FileDownload.
test.describe("restore over MQTT", () => {
  let mqApp: App;
  let mqEnv: Devenv;
  test.beforeAll(async () => {
    mqEnv = await startDevenv(["-only-mqtt-transport"]);
    mqApp = await startApp({
      env: {
        TASMOFLEET_AUTH_MODE: "header",
        TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
        TASMOFLEET_OPERATOR_GROUPS: "ops",
        TASMOFLEET_ADMIN_GROUPS: "adm",
        TASMOFLEET_MQTT_URL: mqEnv.vars.TASMOFLEET_MQTT_URL,
        TASMOFLEET_POLL_INTERVAL: "5s",
      },
    });
    const only = { "mq-esp32": mqEnv.devices["mq-esp32"], "mq-esp8266-1m": mqEnv.devices["mq-esp8266-1m"] };
    mqApp.cli(["import", "tasmoadmin", writeTasmoAdminDir(only)]);
  });
  test.afterAll(async () => {
    await mqApp?.stop();
    await mqEnv?.stop();
  });

  test("restore over MQTT on a device HTTP cannot reach", { tag: "@core" }, async ({ page }) => {
    test.setTimeout(300_000);
    await page.setExtraHTTPHeaders(admin);
    await page.goto(mqApp.url + "/devices");
    await expect(page.getByRole("row").filter({ hasText: "mq-esp32" }).getByText("Online", { exact: true })).toBeVisible({ timeout: 30_000 });
    await page.getByRole("link", { name: "mq-esp32", exact: true }).click();
    const devicePath = new URL(page.url()).pathname;

    // the device's MQTT password: both the backup and the restore need it
    await page.goto(mqApp.url + devicePath + "/edit");
    await page.getByLabel("MQTT password source").selectOption("own");
    await page.getByLabel("Own MQTT password").fill("devenv-fake-mqtt-password");
    await page.getByRole("button", { name: "Save changes" }).click();

    // The group topics of all devices are read shortly after start; until then MQTT transfers are refused with the reason.
    await expect(async () => {
      await page.goto(mqApp.url + devicePath);
      await page.getByRole("button", { name: "Back up", exact: true }).click();
      await expect(page.getByText("Backup taken.")).toBeVisible({ timeout: 5_000 });
    }).toPass({ timeout: 180_000, intervals: [5_000] });

    await page.goto(mqApp.url + devicePath + "/backups");
    await page.getByRole("link", { name: "Restore…" }).first().click();
    await expect(page.getByRole("heading", { name: "Restore backup" })).toBeVisible();
    // the page names the path, and it is not the HTTP download
    await expect(page.locator(".rs-path")).toContainText("MQTT");
    await expect(page.locator(".rs-cred")).toContainText("MQTT broker");
    await page.getByLabel(/I understand/).check();
    await page.getByRole("button", { name: "Restore and restart" }).click();
    await expect(page).toHaveURL(/\/restores\/\d+$/);
    await expect(page.locator("#restore-status")).toContainText("Restore finished.", { timeout: 120_000 });
    await expect(page.locator("#restore-status")).toContainText("uploaded over MQTT");
    await expect(page.locator("#restore-status .rs-step.is-done")).toHaveCount(5);

    // the device password never reaches the audit log
    const audit = await page.request.get(mqApp.url + "/audit", { headers: admin });
    const text = await audit.text();
    expect(text).toContain("restore.finish");
    expect(text).not.toContain("devenv-fake-mqtt-password");
  });
});
