import { mkdirSync } from "node:fs";
import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// The Settings page (issue #275): eight tabs in one form, a card grid, info popovers, one save bar.
// Header authentication: header principals cannot confirm, so only settings without a step-up are
// changed here (the step-up paths are unit tested).
let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" },
  });
});
test.afterAll(async () => {
  await app?.stop();
});
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
const tabs = (page: import("@playwright/test").Page) => page.getByRole("navigation", { name: "Settings sections" });

// Why: a person who edits one tab and looks at another must not lose the edit, and must be told
// which tabs hold unsaved changes. One Save then writes every tab.
test("edits survive a tab change, the bar lists the tabs with changes, one Save writes them all", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=general");
  const bar = page.locator("[data-sys-bar]");
  await expect(bar).toContainText("No unsaved changes");

  await page.locator("[data-key='METRICS_DEVICE_LABELS'] .swtrack").click();
  await expect(bar).toContainText("Unsaved changes:");
  await tabs(page).getByRole("link", { name: "Backups" }).click();
  await expect(page).toHaveURL(/tab=backups/);
  await page.locator("input[name='v.BACKUP_KEEP']").fill("7");
  await expect(bar.locator(".sbar-chip")).toHaveText(["General", "Backups"]);
  await expect(tabs(page).getByRole("link", { name: "General" })).toHaveClass(/is-dirty/);

  await tabs(page).getByRole("link", { name: "General" }).click();
  await expect(page.locator("input[name='v.METRICS_DEVICE_LABELS']")).toBeChecked(); // still there

  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
  await page.goto(app.url + "/settings/system?tab=backups");
  await expect(page.locator("input[name='v.BACKUP_KEEP']")).toHaveValue("7");
  await page.goto(app.url + "/settings/system?tab=general");
  await expect(page.locator("input[name='v.METRICS_DEVICE_LABELS']")).toBeChecked();
  await expect(page.locator("[data-sys-bar]")).toContainText("No unsaved changes");
});

// Why: touch and keyboard users cannot hover. The info button opens a popover; Escape closes it;
// opening another one closes the first.
test("info popovers open on click, close on Escape and show one at a time", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=network");
  const first = page.getByRole("button", { name: "About Device networks" });
  const second = page.getByRole("button", { name: "About Telemetry period" });
  await first.click();
  await expect(page.locator("#info-DEVICE_NETWORKS")).toBeVisible();
  await expect(first).toHaveAttribute("aria-expanded", "true");
  await second.click();
  await expect(page.locator("#info-TELEPERIOD")).toBeVisible();
  await expect(page.locator("#info-DEVICE_NETWORKS")).toBeHidden();
  await page.keyboard.press("Escape");
  await expect(page.locator("#info-TELEPERIOD")).toBeHidden();
  await expect(second).toHaveAttribute("aria-expanded", "false");
});

// Why: scan interval and networks do nothing while the scan is off; they are dimmed (and not
// focusable) until the switch is on.
test("dependent settings follow their switch", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=discovery");
  const dep = page.locator("[data-depends='v.DISCOVERY_SCAN']");
  await expect(dep).toHaveClass(/is-off/);
  await page.locator("[data-key='DISCOVERY_SCAN'] .swtrack").click();
  await expect(dep).not.toHaveClass(/is-off/);
  await dep.locator("input[name='v.DISCOVERY_SCAN_INTERVAL']").fill("2h");

});

// Why (#334): a field that depends on a switch is an ordinary field of the card: no indent, no rule.
test("dependent settings are not drawn as an indented block", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=discovery");
  const dep = page.locator("[data-depends='v.DISCOVERY_SCAN']");
  const css = await dep.evaluate((el) => { const c = getComputedStyle(el); return [c.borderLeftWidth, c.marginLeft, c.paddingLeft]; });
  expect(css).toEqual(["0px", "0px", "0px"]);
  const box = await dep.boundingBox();
  const card = await page.locator("#card-scan .sbody").boundingBox();
  expect(Math.abs(box!.x - card!.x)).toBeLessThan(1);
});

// Why (#335): the Discovery tab holds both ways of finding devices; the old tab=scan opens the first tab.
test("Discovery tab holds MQTT discovery and the network scan; tab=scan opens General", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=discovery");
  await expect(tabs(page).getByRole("link", { name: "Discovery", exact: true })).toHaveAttribute("aria-current", "page");
  await expect(page.locator("#card-mqttdiscovery")).toBeVisible();
  await expect(page.locator("#card-scan")).toBeVisible();
  await page.locator("#card-scan").getByRole("link", { name: "device networks" }).click();
  await expect(tabs(page).getByRole("link", { name: /Network/ })).toHaveAttribute("aria-current", "page");
  await expect(page.locator("#card-networks")).toBeVisible();
  await page.goto(app.url + "/settings/system?tab=scan");
  await expect(tabs(page).getByRole("link", { name: "General" })).toHaveAttribute("aria-current", "page");
});

// Why (#333): "keep the stored password" only matters while host or port differ from the stored
// value, so it shows up then and not before. The e2e stack stores no password, so the option is
// added to the served page.
test("the keep-password option appears only when host or port changed", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.route("**/settings/system?tab=mqtt", async (route) => {
    const res = await route.fetch();
    const html = (await res.text()).replace('<div class="sf w-tls', '</div><label class="ucheck sopt bkeep" data-broker-keep data-stored-host="" data-stored-port=""><input type="checkbox" name="reuse.MQTT_PASSWORD" value="1"/><span>Broker changed: keep the stored password for the new broker</span></label><div class="brow"><div class="sf w-tls');
    await route.fulfill({ response: res, body: html });
  });
  await page.goto(app.url + "/settings/system?tab=mqtt");
  const keep = page.locator("[data-broker-keep]");
  await expect(keep).toBeHidden();
  await page.locator("input[name='mqtt.port']").fill("1999");
  await expect(keep).toBeVisible();
  await page.locator("input[name='mqtt.port']").fill("");
  await expect(keep).toBeHidden();
  await page.locator("input[name='mqtt.host']").fill("other.example");
  await expect(keep).toBeVisible();
});

// Why (review of #333): after a refused save the page shows the new broker; the keep option must
// still be there, still ticked (the server wants a decision about the stored password), and
// compared with the SAVED broker, not with the broker the page was loaded with.
test("keep-password option survives a refused save with a new broker", async ({ page }) => {
  const own = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" },
  });
  try {
    await page.setExtraHTTPHeaders(admin);
    await page.goto(own.url + "/settings/system?tab=mqtt");
    await page.locator("input[name='mqtt.host']").fill("127.0.0.1");
    await page.locator("input[name='mqtt.port']").fill("1883");
    await page.locator("input[name='v.MQTT_PASSWORD']").fill("fake-e2e-password");
    await page.getByRole("button", { name: "Save settings" }).click();
    await expect(page.getByText("Saved.")).toBeVisible();
    const keep = page.locator("[data-broker-keep]");
    await expect(keep).toBeHidden(); // the broker is the saved one
    await page.locator("input[name='mqtt.host']").fill("other.example");
    await expect(keep).toBeVisible();
    await keep.locator("input").check();
    await page.locator("[data-sys-tab='backups'] input[name='v.BACKUP_KEEP']").evaluate((el) => { (el as HTMLInputElement).type = "text"; (el as HTMLInputElement).value = "not-a-number"; });
    await page.getByRole("button", { name: "Save settings" }).click();
    await expect(page.locator("[data-key='BACKUP_KEEP'] .ufield-err")).toBeVisible();
    await tabs(page).getByRole("link", { name: "MQTT" }).click(); // the page opens the tab with the error
    await expect(page.locator("input[name='mqtt.host']")).toHaveValue("other.example");
    await expect(keep).toBeVisible();
    await expect(keep.locator("input")).toBeChecked();
    // going back to the saved broker is what hides it
    await page.locator("input[name='mqtt.host']").fill("127.0.0.1");
    await expect(keep).toBeVisible(); // ticked in the refused save: never hidden by the page
  } finally {
    await own.stop();
  }
});

// Why (#336): the Storage card is part of the page whichever tab it was opened on.
test("the Backups storage card shows when the page was opened on another tab", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=general");
  await tabs(page).getByRole("link", { name: "Backups" }).click();
  await expect(page.locator("#card-storage")).toBeVisible();
  await expect(page.locator("input[name='v.BACKUP_INTERVAL']")).toHaveValue("");
});

// Why (#337): a page that was just loaded has no unsaved changes, on any tab.
test("a freshly loaded page reports no unsaved changes on any tab", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  for (const tab of ["general", "network", "mqtt", "discovery", "backups", "firmware", "signin", "advanced"]) {
    await page.goto(app.url + "/settings/system?tab=" + tab);
    await expect(page.locator("[data-sys-bar]"), tab).toContainText("No unsaved changes");
  }
});

// Why (#337): switching "Scheduled backups" on proposes 24h; switching it off again must leave
// nothing changed (off saves 0 whatever the interval field holds), not a dirty Backups tab.
test("scheduled backups on and off again leaves the tab clean", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=backups");
  const bar = page.locator("[data-sys-bar]");
  const interval = page.locator("input[name='v.BACKUP_INTERVAL']");
  await page.locator("[data-key='BACKUP_SCHEDULE'] .swtrack").click();
  await expect(interval).toHaveValue("24h");
  await expect(bar).toContainText("Unsaved changes:");
  await page.locator("[data-key='BACKUP_SCHEDULE'] .swtrack").click();
  await expect(interval).toHaveValue("");
  await expect(bar).toContainText("No unsaved changes");
});

// Why (#336, #338): "Kept per device" and "Never prune younger than" sit side by side as small
// inputs; the label stays on one line with its info icon right behind it, and the schedule card and
// the deleted devices' card (#410: under the schedule, in the narrow column) stack without a gap.
test("backups: schedule fields side by side, label and icon together, narrow cards without a gap", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/settings/system?tab=backups");
  const keep = page.locator("[data-key='BACKUP_KEEP']");
  const prune = page.locator("[data-key='BACKUP_MAX_AGE']");
  const k = (await keep.boundingBox())!, p = (await prune.boundingBox())!;
  expect(Math.abs(k.y - p.y)).toBeLessThan(2);
  expect((await prune.locator("input").boundingBox())!.width).toBeLessThan(140);
  const lbl = (await prune.locator(".lbl").boundingBox())!, info = (await prune.locator(".uinfo").boundingBox())!;
  expect(lbl.height).toBeLessThan(24);
  expect(info.x - (lbl.x + lbl.width)).toBeLessThan(12);
  const a = (await page.locator("#card-schedule").boundingBox())!, b = (await page.locator("#card-orphans").boundingBox())!;
  expect(b.y - (a.y + a.height)).toBeLessThan(24);
  expect(Math.abs(b.x - a.x), "same column").toBeLessThan(2);
  expect((await page.locator("#card-storage").boundingBox())!.x, "the storage table keeps the wide column").toBeLessThan(a.x);
});

// Why: the backup interval showed "0s" for manual only. The switch is a view of the same
// setting: off stores 0, on keeps the interval.
test("backup schedule switch dims the interval and stores 0 when off", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=backups");
  const sw = page.locator("input[name='sw.BACKUP_INTERVAL']");
  await expect(sw).not.toBeChecked(); // default: manual only
  await page.locator("[data-key='BACKUP_SCHEDULE'] .swtrack").click();
  await expect(page.locator("input[name='v.BACKUP_INTERVAL']")).toHaveValue("24h");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
  await expect(sw).toBeChecked();
  await page.locator("[data-key='BACKUP_SCHEDULE'] .swtrack").click();
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
  await expect(sw).not.toBeChecked();
});

// Why: on a phone eight tabs scroll sideways; a picker replaces the strip, the cards lose their
// frame, nothing overflows, and Save is in a bar that stays in view.
test("phone: section picker, no horizontal overflow, save bar in view", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto(app.url + "/settings/system?tab=signin");
  await expect(page.locator("[data-sys-picker]")).toBeVisible();
  await expect(tabs(page)).toBeHidden();
  await page.locator("[data-sys-picker] select").selectOption("mqtt");
  await expect(page.locator("#sec-mqtt")).toBeVisible();
  await expect(page.locator("#sec-signin")).toBeHidden();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  const save = page.getByRole("button", { name: "Save settings" });
  await expect(save).toBeInViewport();
});

// Local accounts (the default): the Sign-in tab shows the identity provider cards, which wait for
// single sign-on to be selected.
test.describe("with local accounts", () => {
  let local: App;
  test.beforeAll(async () => {
    local = await startApp({ env: { TASMOFLEET_ALLOW_INSECURE_HTTP: "true" } });
    local.cli(["user", "passwd", "--create", "--role", "admin", "settings-admin"], "e2e-fake-password-1\n");
  });
  test.afterAll(async () => {
    await local?.stop();
  });
  async function login(page: import("@playwright/test").Page) {
    await page.setExtraHTTPHeaders({});
    await page.goto(local.url + "/login");
    await page.getByLabel("Username").fill("settings-admin");
    await page.getByLabel("Password").fill("e2e-fake-password-1");
    await page.getByRole("button", { name: "Sign in" }).click();
    await expect(page.getByRole("heading", { name: "Fleet overview" })).toBeVisible();
  }

  // Why: the identity provider does nothing in local mode, so its cards are dimmed until single
  // sign-on is chosen; the choice itself counts as an unsaved change of the Sign-in tab.
  test("the identity provider cards wait for single sign-on", async ({ page }) => {
    await login(page);
    await page.goto(local.url + "/settings/system?tab=signin");
    await expect(page.locator("#card-idp")).toHaveClass(/is-off/);
    await expect(page.locator("#card-roles")).toHaveClass(/is-off/);
    await page.locator("[data-key='AUTH_MODE'] label.rcard", { hasText: "Single sign-on" }).click();
    await expect(page.locator("#card-idp")).not.toHaveClass(/is-off/);
    await expect(page.locator("[data-sys-bar] .sbar-chip")).toHaveText(["Sign-in"]);
  });

  // Screenshots of every tab for review: SETTINGS_SHOTS=<dir>.
  test("screenshots of every tab", async ({ page }) => {
    test.skip(!process.env.SETTINGS_SHOTS, "set SETTINGS_SHOTS=<dir> to write screenshots");
    test.setTimeout(240_000);
    const dir = process.env.SETTINGS_SHOTS!;
    mkdirSync(dir, { recursive: true });
    await login(page);
    for (const scheme of ["light", "dark"] as const) {
      await page.emulateMedia({ colorScheme: scheme });
      for (const [size, w, h] of [["desktop", 1280, 900], ["phone", 375, 812]] as const) {
        await page.setViewportSize({ width: w, height: h });
        for (const tab of ["general", "network", "mqtt", "discovery", "backups", "firmware", "signin", "advanced"]) {
          await page.goto(local.url + "/settings/system?tab=" + tab);
          await page.screenshot({ path: `${dir}/settings-${tab}-${size}-${scheme}.png`, fullPage: true });
        }
      }
    }
    // the Sign-in tab as three cards side by side on a wide screen (#337)
    await page.emulateMedia({ colorScheme: "light" });
    await page.setViewportSize({ width: 1600, height: 900 });
    await page.goto(local.url + "/settings/system?tab=signin");
    await page.screenshot({ path: `${dir}/settings-signin-wide-1600-light.png`, fullPage: true });
  });
});

// Why (#362): the yellow unsaved dots (tab, save bar) used to be added as inline content, so every
// neighbour jumped sideways the moment a value changed. Their space is reserved, so nothing moves.
test("the unsaved dots appear without moving the tab row, the field label or the save bar", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  if (process.env.UI362_W) await page.setViewportSize({ width: +process.env.UI362_W, height: 800 });
  if (process.env.UI362_SCHEME) await page.emulateMedia({ colorScheme: process.env.UI362_SCHEME as "light" | "dark" });
  await page.goto(app.url + "/settings/system?tab=backups");
  const bar = page.locator("[data-sys-bar]");
  await expect(bar).toContainText("No unsaved changes");
  const input = page.locator("input[name='v.BACKUP_KEEP']");
  const row = input.locator("xpath=ancestor::*[self::label or contains(@class,'kv-row') or @data-key][1]");
  const stripShown = await tabs(page).isVisible();
  const boxes = async () => {
    // Page coordinates: focusing the field may scroll the page, which is not a layout shift. The
    // sticky save bar is viewport-anchored, so its boxes stay viewport-relative.
    const scrollY = await page.evaluate(() => window.scrollY);
    const rectIn = async (loc: import("@playwright/test").Locator, dy: number) => {
      const b = await loc.boundingBox();
      expect(b).not.toBeNull();
      return { x: b!.x, y: b!.y + dy, width: b!.width, height: b!.height };
    };
    const rect = (loc: import("@playwright/test").Locator) => rectIn(loc, scrollY);
    const barRect = (loc: import("@playwright/test").Locator) => rectIn(loc, 0);
    return {
      // On a phone the strip is replaced by the section picker (display: none), so it has no boxes.
      tabs: stripShown ? await Promise.all((await tabs(page).getByRole("link").all()).map(rect)) : [],
      row: await rect(row),
      input: await rect(input),
      state: await barRect(bar.locator("[data-sbar-state]")),
      buttons: await Promise.all((await bar.getByRole("button").all()).map(barRect)),
    };
  };
  const shot = process.env.UI362_DIR;
  const scheme = await page.evaluate(() => matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
  const w = page.viewportSize()!.width;
  if (shot) await page.screenshot({ path: `${shot}/${process.env.UI362_TAG}-${w}-${scheme}-clean.png` });
  const before = await boxes();
  const cutClean = await bar.locator("[data-sbar-state]").evaluate((e) => e.scrollWidth - e.clientWidth);
  expect(cutClean, "clean save-bar state text truncated").toBeLessThanOrEqual(0);
  await input.fill("9");
  await expect(bar).toContainText("Unsaved changes:");
  await expect(page.locator("a.utab[href*='tab=backups']")).toHaveClass(/is-dirty/);
  if (shot) await page.screenshot({ path: `${shot}/${process.env.UI362_TAG}-${w}-${scheme}-dirty.png` });
  const after = await boxes();

  // The dots are really drawn (not just hidden placeholders).
  const dot = async (sel: string, pseudo: string) =>
    page.locator(sel).first().evaluate((el, p) => {
      const s = getComputedStyle(el, p);
      return { vis: s.visibility, bg: s.backgroundColor, w: s.width };
    }, pseudo);
  if (stripShown) {
    const tabDot = await dot("a.utab.is-dirty", "::after");
    expect(tabDot.vis).toBe("visible");
    expect(tabDot.w).toBe("6px");
    expect(tabDot.bg).not.toBe("rgba(0, 0, 0, 0)");
  }
  const barDot = await dot("[data-sys-bar].is-dirty", "::before");
  expect(barDot.vis).toBe("visible");
  expect(barDot.w).toBe("6px");

  const near = (a: number, b: number, what: string) => expect(Math.abs(a - b), what).toBeLessThanOrEqual(0.5);
  const same = (a: { x: number; y: number; width: number; height: number }, b: typeof a, what: string) => {
    for (const k of ["x", "y", "width", "height"] as const) near(a[k], b[k], `${what}.${k}`);
  };
  before.tabs.forEach((b, i) => same(b, after.tabs[i], `tab ${i}`));
  same(before.row, after.row, "label row");
  same(before.input, after.input, "input");
  // The state text itself changes ("No unsaved changes" -> "Unsaved changes:"), so only its
  // origin and height must hold; the buttons sit on the right and must not move at all.
  // The state text is never cut off (the dot is not inline, so it takes no room from the text).
  const cut = await bar.locator("[data-sbar-state]").evaluate((e) => e.scrollWidth - e.clientWidth);
  expect(cut, "save-bar state text truncated").toBeLessThanOrEqual(0);
  near(before.state.x, after.state.x, "state.x");
  near(before.state.y, after.state.y, "state.y");
  near(before.state.height, after.state.height, "state.height");
  before.buttons.forEach((b, i) => same(b, after.buttons[i], `button ${i}`));
});
