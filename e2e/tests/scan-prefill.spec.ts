import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// #270: the person already told TasmoFleet which networks hold their devices (DEVICE_NETWORKS), so
// the scan box on the Add page starts with those ranges and Scan now works without typing them
// again. A range one scan refuses (/16 here) is not prefilled and the page says so.
let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_DEVICE_NETWORKS: "127.0.0.0/24, 127.0.1.0/24, 10.1.0.0/16",
    },
  });
});
test.afterAll(async () => {
  await app?.stop();
});
test.use({
  extraHTTPHeaders: {
    "X-authentik-username": "adam",
    "X-authentik-groups": "adm",
  },
});

test("the scan field starts with the device networks a scan accepts, names the one it left out, and Scan now scans them", async ({
  page,
}) => {
  await page.goto(app.url + "/devices/new");
  const field = page.getByLabel("Subnet");
  await expect(field).toHaveValue("127.0.0.0/24, 127.0.1.0/24");
  await expect(page.locator("#scan-prefill-note")).toContainText("10.1.0.0/16");
  await expect(page.locator("#scan-prefill-note")).toContainText(
    "1024 addresses",
  );

  await page.getByRole("button", { name: "Scan now" }).click();
  await expect(page).toHaveURL(/\/discovery\/scan\/[0-9a-f]+/);
  // after a scan the field shows what was scanned, not the prefill
  await expect(page.getByLabel("Subnet")).toHaveValue(
    "127.0.0.0/24, 127.0.1.0/24",
  );
  await expect(page.locator("#scan-prefill-note")).toHaveCount(0);
});
