import { mkdirSync } from "node:fs";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import { realisticDevenvArgs, realisticPolicyRules, seedRealisticData } from "../support/seed";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// v0.7.0 page redesign guards: device settings fit 2.5 screens, intros are one
// line, labels are short, no raw OIDC subject is ever printed, and the values
// typed before a step-up survive it. Screenshots go to $UX_SHOTS when set.
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

let app: App;
let env: Devenv;

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
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
  const seedPage = await browser.newPage({ extraHTTPHeaders: admin });
  try {
    await seedRealisticData(seedPage, app, env);
  } finally {
    await seedPage.close();
  }
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

// folds only exist on a phone; the desktop layout has nothing to open

async function pages(page: import("@playwright/test").Page): Promise<Record<string, string>> {
  await page.goto(app.url + "/devices");
  const lamp = await page.getByRole("link", { name: "lamp-1m", exact: true }).getAttribute("href");
  expect(lamp).toBeTruthy();
  const base = lamp!.replace(app.url, "");
  return {
    "device settings": `${base.replace(/\/?$/, "")}/settings`,
    update: "/updates/new",
    firmware: "/firmware",
    audit: "/audit?since=all",
    policy: "/policy",
    "policy import / export": "/policy/io",
    "system settings": "/settings/system",
    discovery: "/discovery",
  };
}

// Device settings (#367): grouped cards in three independent column stacks at 1280, two balanced
// columns at 1024, one at 375; the Save bar spans the full width and stays in view while scrolling.
test("device settings: three card columns at 1280, two at 1024, one at 375, sticky Save bar", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 800 });
  const p = (await pages(page))["device settings"];
  await page.goto(app.url + p);
  await expect(page.getByRole("button", { name: "Save to device" })).toBeVisible();
  await expect(page.getByText("Applied immediately and saved to the device once.")).toBeVisible();
  const cards = page.locator("form.devform-cards section.gcard");
  await expect(cards).toHaveCount(6);
  await expect(page.locator("#card-power input[name=SaveData]")).toBeVisible();
  await expect(page.locator("#card-power select[name=SetOption0]")).toBeVisible();
  const lefts = () => cards.evaluateAll((els) => els.map((e) => Math.round(e.getBoundingClientRect().left)));
  expect(new Set(await lefts()).size, "three card columns at 1280").toBe(3);
  await expect(page.locator("nav.utabs a[aria-current=page]")).toHaveText("Settings");
  await expect(page.locator(".jumpnav")).toHaveCount(0);
  const bar = await page.locator(".devform-foot").boundingBox();
  const form = await page.locator("form.devform-cards").boundingBox();
  expect(Math.round(bar!.width), "Save bar spans the form").toBe(Math.round(form!.width));
  await page.evaluate(() => window.scrollTo(0, 100));
  await expect(page.getByRole("button", { name: "Save to device" })).toBeInViewport();
  await expectNoOverflow(page, "device settings 1280");
  await page.setViewportSize({ width: 1024, height: 800 });
  expect(new Set(await lefts()).size, "two card columns at 1024").toBe(2);
  await expectNoOverflow(page, "device settings 1024");
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto(app.url + p);
  await expect(cards.first()).toBeVisible();
  expect(new Set(await lefts()).size, "one column at 375").toBe(1);
  const ids = await cards.evaluateAll((els) => els.sort((a, b) => a.getBoundingClientRect().top - b.getBoundingClientRect().top).map((e) => e.id));
  expect(ids, "phone order").toEqual(["card-names", "card-network", "card-power", "card-options", "card-mqtt", "card-time"]);
  await expectNoOverflow(page, "device settings 375");
});

// Intros are one line, labels at most six words.
test("pages: intros fit one line and labels are short", async ({ page }) => {
  test.setTimeout(180_000);
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 800 });
  const bad: string[] = [];
  for (const [name, path] of Object.entries(await pages(page))) {
    await page.goto(app.url + path);
    for (const lead of await page.locator("p.lead:visible").all()) {
      const { h, lh } = await lead.evaluate((e) => ({ h: e.getBoundingClientRect().height, lh: parseFloat(getComputedStyle(e).lineHeight) || 24 }));
      if (h > lh * 1.4) bad.push(`${name}: intro wraps (${Math.round(h)}px): ${(await lead.innerText()).slice(0, 60)}`);
    }
    const labels = await page.locator("main label:visible").evaluateAll((els) =>
      els.map((l) => {
        const c = l.cloneNode(true) as HTMLElement;
        c.querySelectorAll("input, select, textarea, small, button, code, .info").forEach((x) => x.remove());
        return (c.textContent ?? "").trim();
      }),
    );
    for (const t of labels) if (t.split(/\s+/).filter(Boolean).length > 6) bad.push(`${name}: label of ${t.split(/\s+/).length} words: ${t.slice(0, 70)}`);
  }
  expect(bad).toEqual([]);
});

// A raw subject (32 or more hex characters) is never page text; it may only sit in a tooltip.
test("pages: no raw subject outside tooltips", async ({ page }) => {
  test.setTimeout(240_000);
  await page.setExtraHTTPHeaders(admin);
  const bad: string[] = [];
  const all = { ...(await pages(page)), users: "/users", dashboard: "/", devices: "/devices", transactions: "/transactions", about: "/about" };
  for (const [name, path] of Object.entries(all)) {
    for (const width of [1280, 375]) {
      await page.setViewportSize({ width, height: 900 });
      await page.goto(app.url + path);
      const text = await page.locator("body").innerText(); // tooltips live in title/data-tip attributes, not in the text
      const m = text.match(/[0-9a-f]{32,}/i); // a subject is 40 to 64 hex characters (QA M13: the Users page printed 40)
      if (m && name !== "firmware") bad.push(`${name} @${width}: ${m[0].slice(0, 16)}...`);
    }
  }
  expect(bad).toEqual([]);
});

// Over-restriction audit #20: what was typed before a step-up comes back.
test("system settings: typed values return after a step-up (?restore=1)", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system");
  await page.evaluate(() => sessionStorage.setItem("tf-draft:sysconfig", JSON.stringify({ "v.BACKUP_KEEP": "9", "v.OIDC_CLIENT_SECRET": "must-not-return" })));
  await page.goto(app.url + "/settings/system?restore=1");
  await expect(page.locator("input[name='v.BACKUP_KEEP']")).toHaveValue("9");
  expect(await page.evaluate(() => sessionStorage.getItem("tf-draft:sysconfig"))).toBeNull();
  // an ordinary visit drops a stale draft instead of restoring it
  await page.evaluate(() => sessionStorage.setItem("tf-draft:sysconfig", JSON.stringify({ "v.BACKUP_KEEP": "7" })));
  await page.goto(app.url + "/settings/system");
  await expect(page.locator("input[name='v.BACKUP_KEEP']")).not.toHaveValue("7");
});

test("screenshots", async ({ page }) => {
  test.skip(!process.env.UX_SHOTS, "set UX_SHOTS=<dir> to write screenshots");
  test.setTimeout(240_000);
  const dir = process.env.UX_SHOTS!;
  mkdirSync(dir, { recursive: true });
  await page.setExtraHTTPHeaders(admin);
  for (const [name, path] of Object.entries(await pages(page))) {
    for (const width of [1280, 375]) {
      await page.setViewportSize({ width, height: width === 1280 ? 800 : 812 });
      await page.goto(app.url + path);
      await page.waitForTimeout(300);
      await page.screenshot({ path: `${dir}/${name.replace(/ /g, "-")}-${width}.png`, fullPage: true });
    }
  }
});
