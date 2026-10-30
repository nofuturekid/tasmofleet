import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Console and "Make compliant" over MQTT (0.20.0, #79, #256): mq-esp32 is a simulated
// device whose HTTP port refuses every connection but which is on the broker. TasmoFleet reaches it
// only through MQTT (the second device, mq-esp8266-1m, is the same kind).
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv(["-mqtt-transport"]);
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
      TASMOFLEET_POLICY_RULES: JSON.stringify([{ command: "PowerOnState", want: "2", note: "e2e target" }]),
    },
  });
  // two devices: the Columns menu (used to see when the group topics are read) needs a second row
  const only = { "mq-esp32": env.devices["mq-esp32"], "mq-esp8266-1m": env.devices["mq-esp8266-1m"] };
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
  await expect(page.getByRole("row").filter({ hasText: "mq-esp32" }).getByText("Online", { exact: true })).toBeVisible({ timeout: 30_000 });
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
  await expect(page.getByRole("row").filter({ hasText: "mq-esp32" }).getByText("Online", { exact: true })).toBeVisible({ timeout: 30_000 });
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
