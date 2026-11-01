import { openMenu, gotoNav } from "../support/nav";
import { test, expect } from "../support/fixtures";
import { App, freePort, startApp } from "../support/stack";

// The app under a static prefix: every link, the SSE stream and the static
// files must carry it (deployment under /tasmofleet, spec 15.4).
let app: App;
test.beforeAll(async () => {
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TASMOFLEET_BASE_PATH: "/tasmofleet",
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
    },
  });
});
test.afterAll(async () => app.stop());

test("navigation stays below the base path", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders({ "X-authentik-username": "adam", "X-authentik-groups": "adm" });
  await page.goto(app.url + "/");
  for (const [group, name, path, heading] of [["Devices", "List", "devices", "Devices"], ["Updates", "Transactions", "transactions", "Update transactions"], ["Updates", "Firmware", "firmware", "Firmware"], ["System", "Audit log", "audit", "Audit log"], ["System", "About", "about", "About"]]) {
    await gotoNav(page, group, name);
    await expect(page).toHaveURL(new RegExp(`/tasmofleet/${path}$`));
    await expect(page.getByRole("heading", { name: heading, exact: true })).toBeVisible();
  }
  const hrefs = await page.locator("a[href]").evaluateAll((els) => els.map((e) => e.getAttribute("href")!));
  for (const h of hrefs) expect(h.startsWith("/tasmofleet/") || h.startsWith("http"), h).toBeTruthy();
});
