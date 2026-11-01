import { deviceBox } from "../support/devicelist";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow, overflowProblem, smallTargets } from "../support/layout";
import { realisticDevenvArgs, realisticPolicyRules, seedAuditTail, seedRealisticData } from "../support/seed";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Phone layout on the realistic seed (27 devices, 10 releases, cached files, 50-odd audit events):
// zero horizontal overflow, measured against the visible width (see support/layout.ts).
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
const SCREEN = 812;

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
      TASMOFLEET_POLICY_LIVE_INTERVAL: "0", // no background policy reads at random times
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
    await seedAuditTail(app, 25); // one full audit page of entries the seed controls (#237)
  } finally {
    await seedPage.close();
  }
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

// The pages of the phone checks, on the realistic seed.
async function phonePages(page: import("@playwright/test").Page): Promise<Record<string, string>> {
  await page.goto(app.url + "/devices");
  const lamp = await page.getByRole("link", { name: "lamp-1m", exact: true }).getAttribute("href");
  expect(lamp).toBeTruthy();
  await page.goto(app.url + "/policy");
  const rule = await page.locator("td.p1 a").first().getAttribute("href");
  expect(rule).toBeTruthy();
  await page.goto(app.url + "/transactions");
  const tx = await page.locator("a.upd-hrow").first().getAttribute("href");
  expect(tx).toBeTruthy();
  return {
    dashboard: "/",
    devices: "/devices",
    "device detail": lamp!,
    firmware: "/firmware",
    "firmware, all releases": "/firmware?older=1",
    policy: "/policy",
    "policy import / export": "/policy/io",
    "policy rule editor": rule!.replace(app.url, "").replace(/^(?!\/)/, "/"),
    "new policy rule": "/policy/rules/new",
    transactions: "/transactions",
    "transaction detail": tx!.replace(app.url, "").replace(/^(?!\/)/, "/"),
    update: "/updates/new",
    audit: "/audit",
    "system settings": "/settings/system",
    "system settings, advanced": "/settings/system?tab=advanced",
    "add devices": "/devices/new",
    "network scan results": "/discovery/scan",
    "system snapshots": "/settings/system/snapshots",
    users: "/users",
    about: "/about",
    setup: "/setup",
  };
}

test("phone: no main view scrolls sideways", async ({ page }) => {
  test.setTimeout(240_000);
  await page.setViewportSize({ width: 375, height: SCREEN });
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.setExtraHTTPHeaders(admin);
  const pages = await phonePages(page);
  const overflowing: string[] = []; // all pages are checked, so one run names every culprit
  for (const [name, path] of Object.entries(pages)) {
    await page.goto(path.startsWith("http") ? path : app.url + path);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    const problem = await overflowProblem(page, name);
    if (problem) overflowing.push(problem);
  }
  expect(overflowing, "pages wider than the visible width").toEqual([]);
});

// UX review #22 and #36 (chips that wrapped on a phone), resolved by design v2: the settings sections (P8)
// are the prototype's underline tabs; on a phone they are a picker (#275) and every tab stays a link.
// (The Add page has no tabs since #410.)
test("phone: the settings tab row is a picker and its tabs stay links", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 375, height: SCREEN });
  await page.goto(app.url + "/settings/system");
  const sections = page.getByRole("navigation", { name: "Settings sections" });
  // issue #275: eight sections are too many for a strip, so a phone gets a section picker; the tabs
  // stay links in the page (without script they are the way to switch)
  await expect(page.locator('nav.utabs[aria-label="Settings sections"] a', { hasText: "Advanced" })).toHaveCount(1);
  await expect(page.locator("[data-sys-picker] select")).toBeVisible();
  await expect(page.locator('nav.utabs[aria-label="Settings sections"]')).toBeHidden();
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/settings/system");
  await expect(page.getByRole("navigation", { name: "Settings sections" }).getByRole("link", { name: "Advanced" })).toBeVisible();
});

// QA M1: a tab row that scrolls sideways must open with its CURRENT tab in view (tabs.js), on every page
// that has one; otherwise the active tab is cut off or invisible and no tab looks selected.
test("phone: the current tab of every tab row is fully in view", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 375, height: SCREEN });
  const lamp = await (async () => {
    await page.goto(app.url + "/devices");
    return (await page.getByRole("link", { name: "lamp-1m", exact: true }).getAttribute("href"))!.replace(app.url, "");
  })();
  const paths = ["/settings/system?tab=advanced", "/settings/system?tab=signin", `${lamp.replace(/\/?$/, "")}/settings`, `${lamp.replace(/\/?$/, "")}/drift`];
  const bad: string[] = [];
  for (const path of paths) {
    await page.goto(app.url + path);
    await page.evaluate(() => document.fonts.ready);
    for (const strip of await page.locator("nav.utabs").all()) {
      const r = await strip.evaluate((n) => {
        const cur = n.querySelector('[aria-current="page"]');
        if (!cur) return null;
        const s = n.getBoundingClientRect();
        const c = cur.getBoundingClientRect();
        return { left: c.left - s.left, right: s.right - c.right, label: cur.textContent };
      });
      if (r && (r.left < -0.5 || r.right < -0.5)) bad.push(`${path}: tab "${r.label}" is cut off (left ${Math.round(r.left)}, right ${Math.round(r.right)})`);
    }
  }
  expect(bad).toEqual([]);
});

// UX review #33: on a phone the whole transaction row opens the transaction.
test("phone: a tap anywhere on a transaction row opens it", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 375, height: SCREEN });
  await page.goto(app.url + "/transactions");
  const row = page.locator("a.upd-hrow").first();
  const href = await row.getAttribute("href");
  const box = (await row.boundingBox())!;
  await page.mouse.click(box.x + box.width - 6, box.y + box.height - 6); // the row's corner, away from any text
  await expect(page).toHaveURL(new RegExp(href!.replace(/^.*\/transactions\//, "/transactions/") + "$"));
});

// UX review #24: the list shows the short hash; the delete dialog of a cached file has it with a
// copy button for the whole value that works.
test("firmware: short hash with a copy button", async ({ page, context }) => {
  await context.grantPermissions(["clipboard-read", "clipboard-write"]);
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/firmware");
  await page.locator('a[data-dialog^="dlg-file-"]').first().click();
  const dlg = page.locator("dialog[open]");
  const full = await dlg.locator("code").getAttribute("title");
  expect(full).toMatch(/^[0-9a-f]{64}$/);
  await expect(dlg.locator("code")).toHaveText(full!.slice(0, 4) + "…" + full!.slice(-4));
  await dlg.getByRole("button", { name: /Copy the SHA-256/ }).click();
  await expect(dlg.locator("button[data-copy-text]")).toHaveText("Copied");
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(full);
});

test("tablet and phone: the devices table and the navigation never scroll sideways", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  // 800 px is the width at which the desktop table used to be 280 px too wide (Power column).
  for (const width of [375, 640, 800]) {
    await page.setViewportSize({ width, height: SCREEN });
    await page.goto(app.url + "/devices");
    await expect(page.getByRole("link", { name: "lamp-1m", exact: true })).toBeVisible();
    await expectNoOverflow(page, `devices at ${width}px`);
    // Every page carries the navigation: closed, and with each menu open.
    await page.goto(app.url + "/about");
    await expectNoOverflow(page, `nav at ${width}px`);
    if (width < 720) { // phone: the menu button opens the navigation drawer (below 45rem)
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
});

// Name fits about 22 characters, Vitals takes what is left. Vitals once got
// squeezed into two lines because Name took the remaining width.
test("desktop: the devices table keeps Vitals on one line and cells inside their columns", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  for (const width of [1280, 1568, 1920]) {
    await page.setViewportSize({ width, height: 900 });
    await page.goto(app.url + "/devices");
    await expect(page.getByRole("link", { name: "Mr Bambu External Light", exact: true })).toBeVisible();
    await expectNoOverflow(page, `devices at ${width}px`);
    const bad = await page.evaluate(() =>
      [...document.querySelectorAll(".devtable tbody tr")].flatMap((tr) =>
        [...tr.children].flatMap((td) => {
          const cell = td as HTMLElement;
          const label = cell.dataset.label;
          const found: string[] = [];
          if (label === "Vitals") {
            const range = document.createRange();
            range.selectNodeContents(cell);
            const lines = new Set([...range.getClientRects()].map((r) => Math.round(r.top))).size;
            if (lines > 1) found.push(`Vitals wraps (${cell.textContent?.trim()})`);
            if (cell.scrollWidth > cell.clientWidth) found.push(`Vitals is cut (${cell.textContent?.trim()})`);
          }
          if (label === "Power" && cell.scrollWidth > cell.clientWidth) found.push("Power is cut");
          if (label === "Status" && cell.scrollWidth > cell.clientWidth) found.push("Status is cut");
          return found;
        }),
      ),
    );
    expect(bad, `devices at ${width}px`).toEqual([]);
    // a long name is cut with an ellipsis and keeps its full text in the title
    const cell = page.locator("td[data-label=Name]", { hasText: "livingroom-readinglamp.example.internal" });
    await expect(cell).toHaveAttribute("title", "livingroom-readinglamp.example.internal");
  }
});

// A <details> that is open must keep something to click, or it cannot be closed again.
// QA #71: the details open inline in a log box, never as a tooltip over other rows.
test("audit: an entry opens its details inline in a log box and closes again", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/audit?since=all");
  await expect(page.locator(".audittable .tip")).toHaveCount(0);
  const row = page.locator(".audittable tbody tr:has(details)").first();
  const summary = row.locator("summary");
  await expect(summary).toHaveText("Details");
  await summary.click();
  await expect(row.locator("details")).toHaveAttribute("open", "");
  const detail = page.locator(".audittable tr.detail-row").first();
  await expect(detail).toBeVisible();
  await expect(detail.locator("pre.log > span").first()).toBeVisible();
  const box = await summary.boundingBox();
  expect(box?.height ?? 0, "the open summary must stay clickable").toBeGreaterThan(8);
  await summary.click();
  await expect(row.locator("details")).not.toHaveAttribute("open", "");
  await expect(detail).toBeHidden();
  // the Summary is the wide column, never narrower than the target
  const widths = await page.evaluate(() => {
    const th = [...document.querySelectorAll(".audittable thead th")].map((e) => [e.textContent, e.getBoundingClientRect().width]);
    return Object.fromEntries(th);
  });
  expect(widths["Summary"]).toBeGreaterThan(widths["Target"]);
  expect(widths["Details"]).toBeLessThan(120);
});

// One pager for every paged table, the same at every width (QA #71): Previous, the position,
// Next and the page sizes; plain links, filters and size in the URL.
test("audit: the shared pager is the same on desktop and phone", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  const pager = page.locator("nav.pager");
  for (const viewport of [{ width: 1280, height: 900 }, { width: 375, height: 812 }]) {
    await page.setViewportSize(viewport);
    await page.goto(app.url + "/audit?since=all&result=ok");
    await expect(pager.getByText(/Page 1 of \d+, \d+ events/)).toBeVisible();
    await expect(pager.getByText("Previous", { exact: true })).toHaveAttribute("aria-disabled", "true");
    await expect(pager.getByText("Per page")).toBeVisible();
  }
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/audit?since=all&result=ok");
  await pager.getByRole("link", { name: "100", exact: true }).click();
  await expect(page).toHaveURL(/per=100/);
  expect(page.url()).toContain("result=ok"); // the filter survives
  await page.goto(app.url + "/audit?since=all&per=25");
  await pager.getByRole("link", { name: "Next" }).click();
  await expect(page).toHaveURL(/page=2/);
  await expect(pager.getByText(/Page 2 of/)).toBeVisible();
  // keyboard: a pager link is a link
  await pager.getByRole("link", { name: "Next" }).focus();
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(/page=3/);
});

// UX review #1: a thumb needs 40 px. Buttons, summaries, links in rows, toggles,
// checkbox hit areas and form fields on every main view.
test("phone: every control is a 40 px touch target", async ({ page }) => {
  test.setTimeout(240_000);
  await page.setViewportSize({ width: 375, height: SCREEN });
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.setExtraHTTPHeaders(admin);
  const pages = await phonePages(page);
  const small: string[] = [];
  for (const [name, path] of Object.entries(pages)) {
    await page.goto(path.startsWith("http") ? path : app.url + path);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    for (const t of await smallTargets(page)) small.push(`${name}: ${t}`);
  }
  // the phone drawer holds the theme switch and the account links
  await page.goto(app.url + "/");
  await page.getByRole("link", { name: "Account and theme" }).click();
  await expect(page.locator("#nav-drawer [data-theme-choice]").first()).toBeVisible();
  for (const t of await smallTargets(page)) small.push(`drawer: ${t}`);
  expect(small, "touch targets below 40 px").toEqual([]);
});

// UX review #12 and #15, design v2 (F5): the bulk actions live in one bar that shows from the first
// selected device; at 1280 a dark bar with the actions inline, on a phone a panel with the actions in a grid.
test("devices: the bulk bar appears on selection and Clear hides it", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/devices");
  const bar = page.locator("[data-bulkbar]");
  await expect(bar).toBeHidden();
  await deviceBox(page, "esp32-dimmer").check();
  await expect(bar).toContainText("1 device selected");
  await expect(bar.getByRole("button", { name: "Power on", exact: true })).toBeEnabled();
  await bar.getByRole("button", { name: "Clear", exact: true }).click();
  await expect(bar).toBeHidden();

  await page.setViewportSize({ width: 375, height: SCREEN });
  await page.goto(app.url + "/devices");
  await expect(bar).toBeHidden();
  await page.locator("label.dcard-sel", { has: deviceBox(page, "esp32-dimmer") }).click();
  await expect(bar).toBeVisible();
  await page.evaluate(() => window.scrollTo(0, 400));
  await expect(bar.getByRole("button", { name: "Power off", exact: true })).toBeInViewport();
  const box = await bar.boundingBox();
  expect(box!.x).toBeGreaterThanOrEqual(0);
  expect(box!.x + box!.width).toBeLessThanOrEqual(375);
});

// UX review #27 (reworked in #197): New rule is a page of its own; its choices and the Any command
// field are on screen, also where the columns stack.
test("policy: New rule shows its choices on screen, with the Any command field reachable", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  for (const [w, h] of [[1280, 900], [375, SCREEN]]) {
    await page.setViewportSize({ width: w, height: h });
    await page.goto(app.url + "/policy/rules/new");
    await expect(page.getByRole("heading", { level: 1, name: "New rule" })).toBeInViewport();
    await expect(page.getByRole("link", { name: /Local timezone/ })).toBeVisible();
    await page.locator("#any-command").scrollIntoViewIfNeeded();
    await expect(page.locator("#any-command")).toBeInViewport();
  }
});

// UX review #30: with 20 devices on the page the button that goes on must stay reachable. The plan
// beside the steps is sticky (design v2), so "Review update" stays in view while the page scrolls.
test("update: the sticky plan keeps Review update reachable", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width: 1280, height: 700 });
  await page.goto(app.url + "/updates/new");
  await page.evaluate(() => window.scrollTo(0, 300));
  await expect(page.getByRole("button", { name: "Review update" })).toBeInViewport();
  // one select replaces the two power checkboxes
  await expect(page.getByLabel("Variant override")).toBeVisible();
  await expect(page.getByLabel("Power after update")).toBeVisible();
});

// UX review #3 and #4: one vocabulary (Transaction, prefetch, Users) and no raw markdown
// backticks in any rendered label.
test("pages: no backticks in labels, no batch/update-job wording", async ({ page }) => {
  test.setTimeout(240_000);
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.setExtraHTTPHeaders(admin);
  const pages = await phonePages(page);
  pages["device settings"] = pages["device detail"].replace(/\/?$/, "/settings");
  pages["updates preview"] = "/updates/new";
  const bad: string[] = [];
  for (const [name, path] of Object.entries(pages)) {
    await page.goto(path.startsWith("http") ? path : app.url + path);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    const text = await page.locator("body").innerText();
    if (text.includes("`")) bad.push(`${name}: backtick in ${JSON.stringify(text.slice(Math.max(0, text.indexOf("`") - 30), text.indexOf("`") + 30))}`);
    const m = text.match(/\bbatch(es)?\b|update jobs?|pre-fetch|\bdevice\(s\)|\bdeviations?\(s\)|\bvalue\(s\)/i);
    if (m) bad.push(`${name}: "${m[0]}"`);
  }
  expect(bad).toEqual([]);
  // audit targets read as names and links, not as device:12 / batch:1
  await page.goto(app.url + "/audit?since=all&per=100");
  const targets = await page.locator(".audittable td[data-label=Target]").allInnerTexts();
  expect(targets.filter((t) => /^(device|batch|transaction):\d+$/.test(t.trim()))).toEqual([]);
  await page.goto(app.url + "/");
  await expect(page.getByRole("heading", { name: "Update transactions" })).toBeVisible();
});

// QA v0.9.0: phone row cards must keep what identifies a row. A clipped name, a
// name squeezed to "K..." by the buttons, or a hidden "Needed by" makes a row
// unusable even when nothing overflows.
test("phone: proposal rows keep the name, the facts and the buttons on their own lines", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: SCREEN });
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/devices/new");
  const cards = page.locator(".add-cards .add-card");
  // the two seeded proposals plus whatever the simulator fleet announced itself
  expect(await cards.count()).toBeGreaterThanOrEqual(2);
  for (let i = 0; i < (await cards.count()); i++) {
    const card = cards.nth(i);
    const name = await card.locator(".add-card-title").boundingBox(); // the line the name has
    const acts = await card.locator(".add-card-acts").boundingBox();
    expect(name!.width, "the name keeps most of the line").toBeGreaterThan(150);
    expect(acts!.y, "Ignore and Add sit below the name").toBeGreaterThanOrEqual(name!.y + name!.height - 1);
    for (const fact of [".add-card-meta", ".add-card-src"]) {
      await expect(card.locator(fact), `${fact} is a visible line`).toBeVisible();
    }
    expect(await rowOverflows(card), "nothing sticks out of its card").toBe(false);
  }
  await expect(page.locator(".add-card-meta", { hasText: "172.31.13.41" })).toBeVisible();
});

// Why (QA v0.9.0, kept for design v2): a long device name wraps instead of being clipped, so two
// devices with a common prefix can be told apart; in the live preview the value stays beside it.
test("phone: the rule page's device names wrap instead of being clipped", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: SCREEN });
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/policy");
  await page.getByRole("button", { name: "Run now" }).click();
  const links = await page.locator("td.p1 a").evaluateAll((as) => as.map((a) => (a as HTMLAnchorElement).href));
  let rows = 0;
  for (const [i, href] of links.entries()) {
    await page.goto(href);
    const names = page.locator("#rule-devices td.p1 a");
    const c = await names.count();
    rows += c;
    for (let j = 0; j < c; j++) {
      const ok = await names.nth(j).evaluate((el) => el.scrollWidth <= el.clientWidth + 1);
      expect(ok, `rule ${i}: ${await names.nth(j).innerText()} is cut off`).toBe(true);
    }
  }
  expect(rows, "the rules judge devices to show").toBeGreaterThan(0);
});

// Why: on a phone a cached file is a card of two lines; the name, the release and size, and the
// Delete (or the transaction that needs the file) stay inside it.
test("phone: a cached file keeps its name and Delete inside the row", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: SCREEN });
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/firmware");
  const row = page.locator(".fw-crow").first();
  await expect(row).toBeVisible();
  const box = (await row.boundingBox())!;
  for (const part of [".fw-c-f", ".fw-c-v", ".fw-c-a"]) {
    const b = (await row.locator(part).boundingBox())!;
    expect(b.x >= box.x - 0.5 && b.x + b.width <= box.x + box.width + 0.5, `${part} inside the row`).toBe(true);
  }
  await expect(row.locator(".fw-del, .fw-needed")).toBeVisible();
});

/** rowOverflows: a descendant of the row reaches past the row's right edge. */
async function rowOverflows(row: import("@playwright/test").Locator): Promise<boolean> {
  return row.evaluate((tr) => {
    const right = tr.getBoundingClientRect().right + 0.5;
    return [...tr.querySelectorAll("*")].some((e) => e.getBoundingClientRect().width > 0 && e.getBoundingClientRect().right > right);
  });
}
