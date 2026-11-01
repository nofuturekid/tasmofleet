import { test, expect } from "../support/fixtures";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// M2-7: in OIDC mode the import asks the admin to sign in at the IdP again
// (prompt=login, auth_time checked) before it shows the upload form.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  const port = await freePort();
  const publicURL = `http://127.0.0.1:${port}`;
  env = await startDevenv(["-oidc-redirect", `${publicURL}/oidc/callback`]);
  app = await startApp({
    port,
    env: {
      TASMOFLEET_AUTH_MODE: "oidc",
      TASMOFLEET_PUBLIC_URL: publicURL,
      TASMOFLEET_ALLOW_INSECURE_HTTP: "true",
      TASMOFLEET_OIDC_ISSUER: env.vars.DEVENV_OIDC_ISSUER,
      TASMOFLEET_OIDC_CLIENT_ID: env.vars.DEVENV_OIDC_CLIENT_ID,
      TASMOFLEET_OIDC_CLIENT_SECRET: env.vars.DEVENV_OIDC_CLIENT_SECRET,
      TASMOFLEET_OPERATOR_GROUPS: "Tasmofleet Users",
      TASMOFLEET_ADMIN_GROUPS: "Tasmofleet Admins",
    },
  });
});
test.afterAll(async () => {
  await app.stop();
  await env.stop();
});

async function loginAs(who: string) {
  const r = await fetch(`${env.vars.DEVENV_CONTROL_URL}/login-as/${who}`, { method: "POST" });
  expect(r.status).toBe(204);
}

test("OIDC admin imports without a second authentication (step-up dropped 2026-10-21)", async ({ page }) => {
  const dir = writeTasmoAdminDir(env.devices);
  const names = Object.keys(env.devices);
  await loginAs("admin");
  await page.goto(app.url + "/devices/new");
  await expect(page.getByRole("button", { name: "Sign in again" })).toHaveCount(0);
  await page.setInputFiles('input[name="files"]', {
    name: "devices.csv",
    mimeType: "text/csv",
    buffer: readFileSync(join(dir, "devices.csv")),
  });
  await expect(page.locator(".add-improw-name", { hasText: names[0] }).first()).toBeVisible();
  await page.getByRole("button", { name: /^Apply plan/ }).click();
  await expect(page.getByText("Import finished.")).toBeVisible();
});
