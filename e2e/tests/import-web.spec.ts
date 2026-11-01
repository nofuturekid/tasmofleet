import { test, expect } from "../support/fixtures";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// M2-7: an admin uploads TasmoAdmin's devices.csv, sees the dry-run plan,
// confirms, and the devices appear in the inventory.
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
    },
  });
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

test("upload devices.csv, review the plan, apply", async ({ page }) => {
  const dir = writeTasmoAdminDir(env.devices);
  const names = Object.keys(env.devices);
  await page.setExtraHTTPHeaders({ "X-authentik-username": "alice", "X-authentik-groups": "adm" });
  await page.goto(app.url + "/devices/new");
  await page.setInputFiles('input[name="files"]', {
    name: "devices.csv",
    mimeType: "text/csv",
    buffer: readFileSync(join(dir, "devices.csv")),
  });
  await expect(page.getByRole("heading", { name: "Import plan" })).toBeVisible();
  await expect(page.locator(".add-improw-name", { hasText: names[0] }).first()).toBeVisible();
  await page.getByRole("button", { name: /^Apply plan/ }).click();
  await expect(page.getByText("Import finished.")).toBeVisible();
  await page.goto(app.url + "/devices");
  await expect(page.getByRole("link", { name: names[0], exact: true })).toBeVisible();
});

test("operators cannot reach the import", { tag: "@core" }, async ({ page }) => {
  await page.setExtraHTTPHeaders({ "X-authentik-username": "olivia", "X-authentik-groups": "ops" });
  const r = await page.goto(app.url + "/devices/new");
  expect(r?.status()).toBe(403);
});
