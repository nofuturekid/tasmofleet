import { test, expect } from "../support/fixtures";
import { App, Devenv, startApp, startDevenv, writeTasmoAdminDir, freePort } from "../support/stack";

// The GPIO templates tab of the Policy page (#327) against the simulator fleet (devenv -template-shapes):
// groups, the chosen reference, the preview that needs a tick, and a transaction that applies the
// reference to the deviating plug and ends with the tab showing Matches.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv(["-template-shapes"]);
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_POLICY_RULES: JSON.stringify([{ command: "PowerOnState", want: "2", note: "e2e target" }]),
    },
  });
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)])).toContain("created: tpl-nous-1");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const operator = { "X-authentik-username": "olivia", "X-authentik-groups": "ops" };
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

async function readAll(page: import("@playwright/test").Page) {
  await page.goto(app.url + "/policy/templates");
  await page.getByRole("button", { name: "Read again" }).click();
  await expect(page.locator(".gt-card", { hasText: "tpl-nous-dev" })).toBeVisible({ timeout: 60_000 });
}

// Why: operators compare templates but never change devices.
test("templates tab: operators see the groups and no Apply", async ({ page }) => {
  await page.setExtraHTTPHeaders(operator);
  await readAll(page);
  await expect(page.getByRole("link", { name: "GPIO templates" })).toBeVisible();
  await expect(page.locator(".gt-row", { hasText: "tpl-nous-dev" }).getByText("Differs")).toBeVisible();
  await expect(page.getByRole("button", { name: /Apply reference/ })).toHaveCount(0);
  await expect(page.locator("input[name=pick]")).toHaveCount(0);
  const r = await page.request.post(app.url + "/policy/templates/preview", { headers: operator, form: { csrf: "x", family: "esp8266", name: "x", ref: "1" } });
  expect(r.status()).toBe(403);
});

// Why: a tie names no reference; choosing one is the person's decision and is not stored.
test("templates tab: a tied group asks for a reference, choosing one compares against it", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await readAll(page);
  const tie = page.locator(".gt-card", { hasText: "tpl-tie-1" });
  await expect(tie.getByText("Choose a reference.")).toBeVisible();
  await expect(tie.getByRole("button", { name: /Apply reference/ })).toHaveCount(0);
  await tie.locator(".gt-row", { hasText: "tpl-tie-1" }).getByRole("link", { name: "Use as reference" }).click();
  await expect(page).toHaveURL(/\?ref=\d+/);
  const chosen = page.locator(".gt-card", { hasText: "tpl-tie-1" });
  await expect(chosen.getByText("Reference: chosen")).toBeVisible();
  await expect(chosen.getByRole("link", { name: "Back to most common" })).toBeVisible();
});

// Why: devices restart and relays may switch: the preview must show the command and demand the tick,
// then the transaction applies the reference to one device and the tab ends up showing Matches.
test("templates apply: preview needs the tick, the transaction applies, the tab shows Matches", { tag: "@core" }, async ({ page }) => {
  test.setTimeout(180_000);
  await page.setExtraHTTPHeaders(admin);
  await readAll(page);
  const card = page.locator(".gt-card", { hasText: "tpl-nous-dev" });
  await expect(card.getByText(/most common/)).toBeVisible();
  await card.getByLabel("Apply the reference to tpl-offline").uncheck(); // only the plug this test is about
  await card.getByRole("button", { name: /^Apply reference to \d/ }).click();
  await expect(page.getByRole("heading", { level: 1 })).toContainText("template to 1 device");
  await expect(page.locator(".gt-cmd").first()).toContainText("Template {");
  await expect(page.getByText("A backup is taken first")).toBeVisible();
  await expect(page.getByText("Restarts once.", { exact: true })).toBeVisible();
  const go = page.getByRole("button", { name: /^Back up, apply and restart 1 device/ });
  // the box is required: without it the browser does not post
  await go.click();
  await expect(page).toHaveURL(/\/policy\/templates\/preview/);
  await page.getByLabel(/I checked the changes/).check();
  await go.click();
  await expect(page).toHaveURL(/\/transactions\/\d+/);
  await expect(page.getByRole("heading", { name: "Apply GPIO template", level: 1 })).toBeVisible();
  const dev = page.locator(".upd-dev", { hasText: "tpl-nous-dev" });
  await expect(dev.getByText("Applied", { exact: true })).toBeVisible({ timeout: 90_000 });
  await expect(page.locator("#txn-state")).toHaveText("Completed", { timeout: 30_000 });

  await readAll(page);
  await page.goto(app.url + "/policy/templates");
  await page.getByRole("button", { name: "Read again" }).click();
  await expect(page.locator(".gt-card", { hasText: "tpl-nous-dev" }).locator(".gt-row", { hasText: "tpl-nous-dev" }).getByText("Matches")).toBeVisible({ timeout: 60_000 });
});

// Why: the devices can change between the preview and Apply (here one goes offline): Apply must not
// send anything on the old plan, it shows the fresh preview and starts no transaction.
test("templates apply: a device that vanished after the preview stops the apply", async ({ page }) => {
  test.setTimeout(120_000);
  await page.setExtraHTTPHeaders(admin);
  await readAll(page);
  const card = page.locator(".gt-card", { hasText: "tpl-offline" });
  await card.getByRole("button", { name: /^Apply reference to \d/ }).click();
  await expect(page.locator(".gt-cmd").first()).toContainText("Template {");
  const r = await fetch(env.vars.DEVENV_CONTROL_URL + "/device/tpl-offline/off", { method: "POST" });
  expect(r.ok).toBeTruthy();
  await page.getByLabel(/I checked the changes/).check();
  await page.getByRole("button", { name: /^Back up, apply and restart/ }).click();
  await expect(page.getByText(/changed since you looked/).first()).toBeVisible();
  await expect(page).not.toHaveURL(/\/transactions\/\d+$/);
  await page.goto(app.url + "/transactions");
  await expect(page.getByText("TX-0002")).toHaveCount(0);
});

// Why: a template transaction is never re-run, and operators are read-only for it.
test("template transaction: operators cannot cancel or retry it", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  for (const p of ["/transactions/1/cancel", "/transactions/1/retry"]) {
    const r = await page.request.get(app.url + p, { headers: operator });
    expect(r.status(), p).toBe(403);
  }
  const r = await page.request.get(app.url + "/transactions/1/retry", { headers: admin });
  expect(r.status()).toBe(409);
});

// Why: the preview and "Read again" read the devices live and Apply changes them, so none may be
// triggered cross-site: a POST without the CSRF token is refused, and a GET is not served at all.
test("templates routes: no CSRF token is refused, a GET does nothing", async ({ page }) => {
  const form = { family: "esp8266", name: "NOUS A1T", ref: "1", pick: "2", confirm: "1", digest: "x" };
  for (const p of ["/policy/templates/preview", "/policy/templates/read", "/policy/templates/apply"]) {
    const post = await page.request.post(app.url + p, { headers: admin, form });
    expect(post.status(), "POST " + p).toBe(403);
    const get = await page.request.get(app.url + p + "?family=esp8266&name=x&ref=1&pick=2", { headers: admin });
    expect([404, 405], "GET " + p).toContain(get.status());
  }
});
