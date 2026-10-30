import { mkdirSync } from "node:fs";
import { join } from "node:path";
import { deviceBox, deviceEntry, sendCommand } from "../support/devicelist";
import { test, expect } from "../support/fixtures";
import { expectLayout, expectNativeControlsFollowTheme, expectNoOverflow, openFolds } from "../support/layout";
import { quickPick } from "../support/updates";
import { hash40, realisticDevenvArgs, renamedDevices, realisticPolicyRules, seedRealisticData } from "../support/seed";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Design check of every page: light and dark, desktop and phone width. The
// stylesheet is the only thing that decides how the pages look, so a layout
// that breaks (content wider than a 375 px phone, a CSP violation from a new
// inline style) must fail here before anyone sees it. Set UI_SHOTS=<dir> to
// keep a full-page screenshot of every page and mode for visual review.
// The data is as large as a real installation (27 devices, 10 releases, cached
// files, the ESP32-exception policy, audit entries, a retried transaction):
// overflow bugs hide on sparse data, so the layout is checked on realistic data.
const shots = process.env.UI_SHOTS;

let batchURL = "";
let app: App;
let local: App;
let env: Devenv;
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
const password = "e2e-fake-password-1";

test.beforeAll(async ({ browser }) => {
  test.setTimeout(420_000);
  env = await startDevenv(realisticDevenvArgs);
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_OPERATOR_GROUPS: "ops",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_GITHUB_URL,
      TASMOFLEET_POLICY_RULES: realisticPolicyRules,
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir({ ...env.devices, "garage-plug-offline": { ...Object.values(env.devices)[0], addr: "127.0.0.1:1", topic: "garage_plug_offline" } })]);
  const seedPage = await browser.newPage({ extraHTTPHeaders: admin });
  try {
    await seedRealisticData(seedPage, app, env);
  } finally {
    await seedPage.close();
  }
  local = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.2/32", TASMOFLEET_ADMIN_GROUPS: "adm" },
  });
  local.cli(["user", "passwd", "--create", "--role", "admin", "breakglass"], `${password}\n`);
});
test.afterAll(async () => {
  await app?.stop();
  await local?.stop();
  await env?.stop();
});

const modes = [
  { scheme: "light", size: "desktop", viewport: { width: 1280, height: 800 } },
  { scheme: "dark", size: "desktop", viewport: { width: 1280, height: 800 } },
  { scheme: "light", size: "phone", viewport: { width: 375, height: 812 } },
  { scheme: "dark", size: "phone", viewport: { width: 375, height: 812 } },
  { scheme: "light", size: "w1568", viewport: { width: 1568, height: 900 } },
  { scheme: "dark", size: "w1920", viewport: { width: 1920, height: 1080 } },
] as const;

for (const m of modes) {
  test(`every page renders cleanly: ${m.scheme} ${m.size}`, async ({ page }) => {
    test.setTimeout(300_000);
    await page.setViewportSize(m.viewport);
    await page.emulateMedia({ colorScheme: m.scheme, reducedMotion: "reduce" });
    if (shots) mkdirSync(shots, { recursive: true });

    // look takes the screenshot, checks that nothing is wider than the screen and that native
    // controls (select popups, pickers) follow the theme.
    const edges: Record<string, number> = {};
    const look = async (name: string) => {
      if (page.url().startsWith(app.url)) {
        // one container for all signed-in pages: the left edge never moves between short and long pages
        const left = await page.evaluate(() => {
          const m = document.querySelector("main");
          return m ? Math.round(m.getBoundingClientRect().left) : null;
        });
        if (left !== null) edges[name] = left;
      }
      await expectNoOverflow(page, name);
      // The generic layout guards (a to g) run at the two widths people use: 1280 and 375.
      if (m.size === "desktop" || m.size === "phone") await expectLayout(page, name);
      // native popups and pickers never show on a screenshot: their theme is checked by computed style
      if (m.size === "desktop") await expectNativeControlsFollowTheme(page, name);
      if (shots) await page.screenshot({ path: join(shots, `${name}-${m.scheme}-${m.size}.png`), fullPage: true });
    };

    // ---- signed-in app (header auth, admin) ----
    await page.setExtraHTTPHeaders(admin);
    await page.goto(app.url + "/devices");
    for (const name of Object.keys(env.devices).map((n) => renamedDevices[n] ?? n)) {
      await expect(deviceEntry(page, name).getByText("Online", { exact: true }).first()).toBeVisible({ timeout: 40_000 });
    }
    await page.goto(app.url + "/");
    await look("dashboard");
    await page.goto(app.url + "/devices");
    await look("devices");
    // the fixed bulk bar lies inside the viewport at every width (guard a) and the page stays clean with it
    await page.locator("input[name=id]").filter({ visible: true }).first().check();
    await expect(page.locator("[data-bulkbar]")).toBeVisible();
    await look("devices-selected");
    await page.locator("[data-bulkbar]").getByRole("button", { name: "Clear", exact: true }).click();
    await page.goto(app.url + "/devices?health=offline");
    await look("devices-empty");
    await page.goto(app.url + "/devices/new");
    await look("device-new");

    // Bulk "Send command": the scope page with the command box, then the preview.
    await page.goto(app.url + "/devices");
    await deviceBox(page, "lamp-1m").check();
    await sendCommand(page).click();
    await expect(page.getByRole("heading", { name: "Send a console command", level: 1 })).toBeVisible();
    await look("bulk-command");
    await page.getByRole("textbox", { name: "Command" }).fill("Status 2");
    await page.getByRole("button", { name: "Send to 1 device" }).click();
    await expect(page.getByRole("heading", { name: "Send Status 2 to 1 device?" })).toBeVisible();
    await look("bulk-command-preview");

    // Bulk "Back up" and "Restart" from the bar: the scope page (the picked devices, the offline one
    // marked "Will be skipped"; for a restart it is the confirmation), and the backup result that
    // lists the offline device as skipped. The fleet is all online except one device nobody answers
    // for (imported with the rest): it shows what "skipped" looks like.
    await page.goto(app.url + "/devices");
    // it never answered, so the list calls it Unknown (design v2, P2); the bulk pages still leave it out
    await expect(deviceEntry(page, "garage-plug-offline").getByText("Unknown", { exact: true }).first()).toBeVisible({ timeout: 60_000 });
    const pickTwo = async () => {
      await page.goto(app.url + "/devices");
      await deviceBox(page, "sim-04-esp32c3").check();
      await deviceBox(page, "garage-plug-offline").check();
    };
    // the bulk pages leave it out once the health tracker has given up on it (its polls failed),
    // which the list no longer shows (it stays Unknown), so the confirmation is retried until then
    await expect(async () => {
      await pickTwo();
      await page.locator("[data-bulkbar]").getByRole("button", { name: "Restart" }).click();
      await expect(page.getByRole("heading", { name: "Restart devices", level: 1 })).toBeVisible();
      await expect(page.locator(".bk-row", { hasText: "garage-plug-offline" })).toContainText("Will be skipped");
      await expect(page.locator(".bk-devs")).toContainText("sim-04-esp32c3"); // the picked devices are named
      await expect(page.getByRole("button", { name: "Restart 1 device" })).toBeVisible({ timeout: 2_000 });
    }).toPass({ timeout: 90_000 });
    await look("bulk-restart-scope");
    await pickTwo();
    await page.locator("[data-bulkbar]").getByRole("button", { name: "Back up" }).click();
    await expect(page.getByRole("heading", { name: "Back up devices", level: 1 })).toBeVisible();
    await look("bulk-backup-scope");
    await page.getByRole("button", { name: "Back up 1 device", exact: true }).click();
    await expect(page.getByRole("heading", { name: /^Backups (running|finished)$/, level: 1 })).toBeVisible();
    await expect(page.locator(".bk-rrow", { hasText: "garage-plug-offline" })).toContainText("Skipped");
    await look("bulk-backup-result");

    await page.goto(app.url + "/devices");
    await page.getByRole("link", { name: "lamp-1m", exact: true }).click();
    const deviceURL = page.url();
    // A backup gives the overview its snapshot facts (hostname, gateway, Wi-Fi); a second one
    // within the cooldown is refused with a sentence, which is fine here.
    await page.getByRole("button", { name: "Back up", exact: true }).click();
    await expect(page.getByText(/Backup taken|Last backup .* ago/).first()).toBeVisible({ timeout: 30_000 });
    await page.goto(deviceURL);
    await look("device");
    await page.goto(deviceURL + "/edit");
    await look("device-edit");
    await page.goto(deviceURL + "/console");
    await page.getByRole("textbox", { name: "Command" }).fill("Status 2");
    await page.getByRole("button", { name: "Send" }).click();
    await expect(page.getByText("StatusFWR")).toBeVisible();
    // a long reply line wraps inside the panel: nothing is cut off at its edge, no sideways scroll
    const cut = await page.locator(".console-lines").evaluate((e) => e.scrollWidth - e.clientWidth);
    expect(cut, "console panel scrolls sideways: a reply line is cut off").toBeLessThanOrEqual(0);
    await look("console");
    await page.goto(deviceURL + "/settings");
    await look("settings");

    await page.goto(app.url + "/updates/new");
    await look("updates-new");
    await quickPick(page, "All that can be updated");
    await look("updates-new-selected");
    // The batch only covers two devices so the others stay free for the pages above.
    await quickPick(page, "None");
    await page.getByLabel("Select esp32-dimmer").check();
    await page.getByLabel("Select esp32c3-sensor").check();
    await page.getByRole("button", { name: "Review update" }).click();
    await expect(page.getByRole("heading", { name: /^4 · Preview/ })).toBeVisible();
    await look("updates-preview");
    // A running update, seen at several moments: the first mode starts it, the
    // others look at the same batch (finished by then).
    if (!batchURL) {
      await page.getByRole("button", { name: "Start update", exact: true }).click();
      await expect(page).toHaveURL(/\/transactions\/\d+$/);
      batchURL = page.url();
      await look("batch-running-1");
      await page.waitForTimeout(4000);
      await look("batch-running-2");
      await page.waitForTimeout(6000);
      await look("batch-running-3");
      for (const name of ["esp32-dimmer", "esp32c3-sensor"]) {
        await expect(page.locator(".upd-row").filter({ hasText: name }).getByText("Done", { exact: true })).toBeVisible({ timeout: 200_000 });
      }
      await look("batch-finished");
    } else {
      await page.goto(batchURL);
      await look("batch-done");
    }
    await page.goto(app.url + "/transactions");
    await look("transactions");

    // A bulk command run: the job card's one-line log summary is the whole Status 2
    // reply (one long line of JSON), which once set the width of the page (1707 px at 1280).
    // Looked at right after the start, when done, and for a run that failed.
    await page.goto(app.url + "/devices");
    for (const name of ["lamp-1m", "attic-heater-plug-with-a-really-long-device-name-that-wraps", "AtticHeaterPlugWithAReallyLongDeviceNameThatNeverHasABreakAtAll"]) {
      await deviceBox(page, name).check();
    }
    await sendCommand(page).click();
    await page.getByRole("textbox", { name: "Command" }).fill("Status 2");
    await page.getByRole("button", { name: "Send to 3 devices" }).click();
    await expect(page.getByRole("heading", { name: "Send Status 2 to 3 devices?" })).toBeVisible();
    await page.getByRole("button", { name: "Send to 3 devices" }).click();
    await expect(page.getByText("› Status 2")).toBeVisible();
    await look("bulk-run-started");
    await expect(page.getByRole("heading", { name: "Command finished", level: 1 })).toBeVisible({ timeout: 60_000 });
    // the fixture must really be long, or this guard checks nothing
    await expect.poll(() => page.locator(".bk-detail").first().evaluate((e) => (e.textContent ?? "").length), { timeout: 30_000 }).toBeGreaterThan(150);
    await look("bulk-run-done");
    // the transaction page keeps every device's log
    await page.goto(page.url().replace(/\/devices\/bulk\/result\/(\d+).*$/, "/transactions/$1"));
    await expect(page.locator("#txn-state")).toHaveText("Completed", { timeout: 60_000 });
    await expect.poll(() => page.locator("#log-lines").evaluate((e) => Math.max(...[...e.querySelectorAll(".upd-log-t")].map((l) => (l.textContent ?? "").length))), { timeout: 30_000 }).toBeGreaterThan(150);
    await look("bulk-run-transaction");
    // a run that failed: the device with a web password rejects the command (a refused login proves nothing ran,
    // so the router would try the broker, where Status 2 would succeed). Json, a command that runs other commands, is
    // refused over MQTT, so the run fails on both paths.
    await page.goto(app.url + "/devices");
    await deviceBox(page, "locked-plug").check();
    await sendCommand(page).click();
    await page.getByRole("textbox", { name: "Command" }).fill('Json {"Name":"x"}');
    await page.getByRole("button", { name: "Send to 1 device" }).click();
    await expect(page.getByRole("heading", { name: "Send Json *** to 1 device?" })).toBeVisible();
    await page.getByRole("button", { name: "Send to 1 device" }).click();
    await expect(page.getByText("Finished · 1 failed", { exact: true })).toBeVisible({ timeout: 60_000 });
    await look("bulk-run-failed");
    await page.goto(app.url + "/firmware");
    await look("firmware");
    // the three newest releases as cards, the rest behind "Show N older releases", every width
    await expect(page.locator("a.fw-rel:visible")).toHaveCount(3);
    await page.getByRole("link", { name: "Show 7 older releases" }).click();
    await expect(page.locator("a.fw-chip:visible")).toHaveCount(7);
    await look("firmware-all-releases");
    await page.getByRole("link", { name: "Hide older releases" }).click();
    await page.getByRole("link", { name: "Release notes" }).click();
    await expect(page.locator("dialog[open]")).toBeVisible();
    await look("firmware-dialog"); // the page behind it must not shift
    await page.keyboard.press("Escape");
    await expect(page.locator("dialog[open]")).toHaveCount(0);
    await page.goto(app.url + "/policy");
    await look("policy");
    // a live check, the rule menu, the Make compliant dialog and its page without JavaScript
    await page.getByRole("button", { name: "Run now" }).click();
    await expect(page.locator("#live-tile")).toContainText(/ago|just now/, { timeout: 60_000 });
    await look("policy-checked");
    await page.getByLabel(/^Actions for/).first().click();
    await look("policy-rulemenu");
    await page.keyboard.press("Escape");
    const fix = page.getByRole("button", { name: "Make compliant…" });
    if (await fix.count()) {
      await fix.click();
      await expect(page.locator("dialog#dlg-fix")).toBeVisible();
      await look("policy-fix-dialog"); // the page behind it must not shift
      await page.keyboard.press("Escape");
      await page.locator("form:has(button[data-dialog=dlg-fix])").evaluate((f: HTMLFormElement) => f.submit());
      await expect(page.getByRole("heading", { name: /^Make \d+ devices? compliant$/, level: 1 })).toBeVisible();
      await look("policy-fix");
    }
    await page.goto(app.url + "/discovery");
    await look("discovery");
    await page.goto(app.url + "/import/tasmoadmin");
    await look("import");
    await page.goto(app.url + "/audit");
    await look("audit");
    // a filtered audit page offers exactly one way back, and on a phone it is a 40 px target
    await page.goto(app.url + "/audit?result=error&since=all");
    await expect(page.getByRole("link", { name: "Clear the filter" })).toHaveCount(1);
    if (m.size === "phone") {
      const box = await page.getByRole("link", { name: "Clear the filter" }).boundingBox();
      expect(box?.height ?? 0, "Clear the filter is below the 40 px touch target").toBeGreaterThanOrEqual(40);
    }
    await page.goto(app.url + "/settings/system");
    await look("system-settings");
    await page.goto(app.url + "/settings/system?tab=advanced");
    await look("system-settings-advanced");
    await page.goto(app.url + "/about");
    await look("about");
    await page.goto(app.url + "/styleguide");
    await look("styleguide");
    // Pages behind a link or a form: the device's backups, prune confirmation, drift and replace, a
    // transaction and its retry, a release detail, a cached file's delete confirmation, the users list.
    await page.goto(deviceURL + "/backups");
    await look("device-backups");
    await page.goto(deviceURL + "/backups/prune");
    await look("device-backups-prune");
    await page.goto(deviceURL + "/drift");
    await look("device-drift");
    await page.goto(deviceURL + "/replace");
    await look("device-replace");
    if (batchURL) {
      await page.goto(batchURL + "/retry");
      await look("transaction-retry");
    }
    await page.goto(app.url + "/firmware?notes=1&release=" + env.vars.DEVENV_FW_VERSION);
    await look("firmware-release");
    await page.goto(app.url + "/firmware");
    const deleteHref = await page.locator('a[href*="/firmware/artifacts/"][href$="/delete"]').first().getAttribute("href");
    if (deleteHref) {
      await page.goto(app.url + deleteHref.replace(/^.*?(\/firmware\/artifacts\/)/, "$1"));
      await look("firmware-artifact-delete");
    }
    await page.goto(app.url + "/policy/io");
    await look("policy-io");
    await page.goto(app.url + "/policy/rules/new");
    await look("policy-rule-new");
    await page.goto(app.url + "/policy");
    const firstRule = (await page.locator("td.p1 a").first().getAttribute("href"))!.split("/").pop();
    await page.goto(app.url + "/policy/rules/delete?id=" + firstRule);
    await look("policy-rule-delete");
    await page.goto(deviceURL + "/backups");
    const bkDelete = await page.locator('a[href$="/delete"][data-dialog="dlg-bkdel"]').first().getAttribute("href");
    expect(bkDelete, "the device has a backup with a Delete link").not.toBeNull();
    await page.goto(app.url + bkDelete!.replace(/^.*?(\/backups\/)/, "$1"));
    await look("backup-delete");
    await page.goto(app.url + "/users");
    await look("users-view");
    await page.goto(app.url + "/devices/9999");
    await look("not-found");

    expect(new Set(Object.values(edges)).size, `main left edge differs between pages: ${JSON.stringify(edges)}`).toBe(1);

    // ---- local sign-in, account pages (no proxy header, so the local form) ----
    await page.setExtraHTTPHeaders({});
    await page.goto(local.url + "/login");
    // standalone pages follow the system theme too (their <html> carries data-theme-auto): the
    // page background is dark in the dark scheme and light in the light one
    const bg = await page.evaluate(() => getComputedStyle(document.body).backgroundColor.match(/\d+/g)!.slice(0, 3).map(Number));
    const lum = (bg[0] + bg[1] + bg[2]) / 3;
    if (m.scheme === "dark") expect(lum, `login background ${bg} is light in dark mode`).toBeLessThan(80);
    else expect(lum, `login background ${bg} is dark in light mode`).toBeGreaterThan(200);
    await look("login");
    await page.getByLabel("Username").fill("breakglass");
    await page.getByLabel("Password").fill("wrong-password-xx");
    await page.getByRole("button", { name: "Sign in" }).click();
    await look("login-error");
    await page.getByLabel("Username").fill("breakglass");
    await page.getByLabel("Password").fill(password);
    await page.getByRole("button", { name: "Sign in" }).click();
    await expect(page.getByRole("heading", { name: "Fleet overview" })).toBeVisible();
    await page.goto(local.url + "/account");
    await look("account");
    await page.goto(local.url + "/users");
    // the own row says it is yours
    await expect(page.locator(".urow", { hasText: "breakglass" }).getByText("You", { exact: true })).toBeVisible();
    await look("users");
    // the add form keeps the prototype's 760 px card, its fields in two columns
    await page.goto(local.url + "/users/new");
    const newUser = await page.getByLabel("Username").boundingBox();
    expect(newUser?.width ?? 0, "the Add local user inputs run across the card").toBeLessThanOrEqual(380);
    await look("users-new");
  });
}

// Explicit phone-width guard for the pages whose scope chips once pushed the
// layout wider than the screen (real fleets have the ESP32 exception rule, whose
// scope is nine chips). Checked light only: the dark theme changes no geometry.
test("phone width: policy, rule editor and setup never scroll sideways", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await page.setExtraHTTPHeaders(admin);
  for (const path of ["/policy", "/policy/io", "/policy/rules/new", "/setup", "/transactions"]) {
    await page.goto(app.url + path);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await expectNoOverflow(page, path);
  }
  // Opened navigation menus stay on screen at phone and tablet width.
  for (const width of [375, 640, 800]) {
    await page.setViewportSize({ width, height: 812 });
    await page.goto(app.url + "/about");
    if (width < 720) { // phone: the drawer holds the groups and the account
      await page.getByLabel("Open navigation", { exact: true }).click();
      await expect(page.locator("dialog#nav-drawer")).toBeVisible();
      await expectNoOverflow(page, `phone drawer at ${width}px`);
      await page.keyboard.press("Escape");
      continue;
    }
    await page.locator("details.usermenu > summary").click();
    await expectNoOverflow(page, `user menu at ${width}px`);
    await page.keyboard.press("Escape");
  }
  await page.setViewportSize({ width: 375, height: 812 });
  // The version history once held a full 40-character identifier that cannot
  // wrap: it made the rules page 668 px wide. Show the short form, full value on hover.
  await page.goto(app.url + "/policy/history");
  const versions = page.locator("#history");
  await expect(versions.getByText(hash40)).toHaveCount(0);
  await expect(versions.locator(`[title*="${hash40}"]`).first()).toContainText(hash40.slice(0, 8));
  // A scope that covers every ESP32 variant reads as one name, not nine.
  await page.goto(app.url + "/policy");
  await expect(page.locator("section#rules tbody tr", { hasText: "ESP32 (all variants)" }).first()).toBeVisible();
  await expect(page.getByText("esp32c61", { exact: true }).filter({ visible: true })).toHaveCount(0);
  await page.goto(app.url + "/policy/rules/new?template=timezone");
  await page.locator(".pol-scope > summary").click(); // the scope choices are a fold
  await expect(page.getByLabel("ESP32 (all variants)")).toBeVisible();
});
