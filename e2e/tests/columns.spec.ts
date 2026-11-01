import { mkdirSync } from "node:fs";
import { join } from "node:path";
import type { Browser, BrowserContext, Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import { realisticDevenvArgs, realisticPolicyRules, seedRealisticData } from "../support/seed";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// Table columns and page size are saved per user on the server: they must
// survive a reload and show up in a second browser of the same user, with the
// layout already in the HTML (no jump after load), and never leak to another
// user. Tooltips: chips explain themselves and a cut-off value shows its text.
// Set UX_SHOTS=<dir> to keep screenshots (desktop and phone, Columns menu open).
const shots = process.env.UX_SHOTS;
// UX_SEED=1: the data of a real installation (27 devices, transactions, audit entries); slow, for the screenshots
const seeded = !!process.env.UX_SEED;

let app: App;
let env: Devenv;
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
const eve = { "X-authentik-username": "eve", "X-authentik-groups": "adm" };

test.beforeAll(async ({ browser }) => {
  test.setTimeout(420_000);
  env = await startDevenv(seeded ? realisticDevenvArgs : []);
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
  // The width of a column follows its content (chip, firmware and status texts fill in as a device
  // comes online), so a test that drags a width and reloads needs a fleet that has settled: wait
  // until every simulated device is online before the first test.
  const total = String(Object.keys(env.devices).length);
  await expect
    .poll(
      async () => {
        const html = await (await fetch(app.url + "/devices", { headers: adam })).text();
        return /Online<span class="dv-n">(\d+)<\/span>/.exec(html)?.[1];
      },
      { timeout: 120_000, intervals: [1000] },
    )
    .toBe(total);
  if (seeded) {
    const seedPage = await browser.newPage({ extraHTTPHeaders: adam });
    try {
      await seedRealisticData(seedPage, app, env);
    } finally {
      await seedPage.close();
    }
  }
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const headCell = (page: Page, table: string, name: string) => page.locator(`table[data-table="${table}"] thead th`, { hasText: new RegExp(`^\\s*${name}\\s*$`) });
// A device event reloads the list and replaces the table. Locator.boundingBox() resolves the
// header and measures it in two steps, and on a slow machine the swap lands between them, again
// and again: the call then never settles within the poll's timeout although a header is on the
// page the whole time (reproduced with CPU throttling). So find and measure in one page-side step,
// which cannot see a half-replaced table, and give a slow runner time.
const width = async (page: Page, table: string, name: string) => {
  let w = 0;
  await expect
    .poll(
      async () =>
        (w = await page.evaluate(
          ([t, n]) => {
            const th = [...document.querySelectorAll(`table[data-table="${t}"] thead th`)].find((h) => h.textContent?.trim() === n);
            return th ? th.getBoundingClientRect().width : 0;
          },
          [table, name],
        )),
      { timeout: 15_000 },
    )
    .toBeGreaterThan(0);
  return w;
};
const saved = (page: Page, key: string) => page.waitForResponse((r) => r.url().endsWith(`/prefs/${key}`) && r.request().method() === "POST" && r.status() === 204);

/** a second browser of the same user: own cookies, own storage */
async function otherBrowser(browser: Browser, headers: Record<string, string>): Promise<{ ctx: BrowserContext; page: Page }> {
  const ctx = await browser.newContext({ extraHTTPHeaders: headers, viewport: { width: 1280, height: 800 } });
  return { ctx, page: await ctx.newPage() };
}

/** postPref sends a preference the way the page script does: same origin, token from the page. */
async function postPref(page: Page, key: string, doc: unknown): Promise<number> {
  return page.evaluate(
    async ([k, d]) => {
      const token = JSON.parse(document.body.getAttribute("hx-headers") ?? "{}")["X-CSRF-Token"];
      const r = await fetch(`${document.body.getAttribute("data-prefs")}/${k}`, { method: "POST", headers: { "Content-Type": "application/json", "X-CSRF-Token": token }, body: JSON.stringify(d) });
      return r.status;
    },
    [key, doc] as const,
  );
}
async function resetAll(page: Page, path: string, key: string) {
  if (!page.url().startsWith(app.url)) await page.goto(app.url + path);
  expect(await postPref(page, key, {})).toBe(204);
  await page.goto(app.url + path);
}

test("dragging a column edge resizes it; the width survives a reload and a second browser, and is in the HTML", async ({ page, browser }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  await resetAll(page, "/devices", "table.devices");
  const before = await width(page, "devices", "Firmware");
  const handle = page.getByRole("separator", { name: "Resize column Firmware" });
  const box = (await handle.boundingBox())!;
  const done = saved(page, "table.devices");
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await page.mouse.down();
  await page.mouse.move(box.x + box.width / 2 + 20, box.y + box.height / 2, { steps: 5 });
  await page.mouse.move(box.x + box.width / 2 + 40, box.y + box.height / 2, { steps: 5 });
  await page.mouse.up();
  await done;
  const dragged = await width(page, "devices", "Firmware");
  expect(dragged).toBeGreaterThan(before + 25);

  await page.reload();
  // the page lays the saved widths out again when its fonts are in (tables.js enhance): wait for that
  await expect.poll(async () => Math.abs((await width(page, "devices", "Firmware")) - dragged)).toBeLessThan(3);

  // already in the server's HTML: no layout jump after load
  const html = await (await page.request.get(app.url + "/devices", { headers: adam })).text();
  expect(html).toMatch(/<table[^>]*class="[^"]*tfix[^"]*"[^>]*data-table="devices"|<table[^>]*data-table="devices"[^>]*class="[^"]*tfix/);
  expect(html).toMatch(/<col data-id="firmware" width="[0-9.]+%">/);

  const second = await otherBrowser(browser, adam);
  await second.page.goto(app.url + "/devices");
  expect(Math.abs((await width(second.page, "devices", "Firmware")) - dragged)).toBeLessThan(3);
  await second.ctx.close();

  // another user keeps the defaults
  const other = await otherBrowser(browser, eve);
  await other.page.goto(app.url + "/devices");
  await expect(other.page.locator('table[data-table="devices"]')).not.toHaveClass(/tfix/);
  await other.ctx.close();
  await expectNoOverflow(page, "devices with a saved layout");
});

test("keyboard: arrow keys change the width, Home resets the column", async ({ page, browser }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/audit");
  await resetAll(page, "/audit", "table.audit");
  const handle = page.getByRole("separator", { name: "Resize column User" });
  const start = await width(page, "audit", "User");
  await handle.focus();
  const done = saved(page, "table.audit");
  for (let i = 0; i < 5; i++) await page.keyboard.press("ArrowRight");
  await done;
  const wider = await width(page, "audit", "User");
  expect(wider).toBeGreaterThan(start + 40);
  await expect(handle).toBeFocused();

  const second = await otherBrowser(browser, adam);
  await second.page.goto(app.url + "/audit");
  expect(Math.abs((await width(second.page, "audit", "User")) - wider)).toBeLessThan(3);
  await second.ctx.close();

  const reset = saved(page, "table.audit");
  await handle.focus();
  await page.keyboard.press("Home");
  await reset;
  const home = await width(page, "audit", "User");
  expect(home).toBeLessThan(wider - 20);
  // keyboard also shrinks, down to the minimum and not below
  const shrunk = saved(page, "table.audit");
  for (let i = 0; i < 40; i++) await page.keyboard.press("ArrowLeft");
  await shrunk;
  expect(await width(page, "audit", "User")).toBeGreaterThanOrEqual(55);
});

test("Columns menu hides and shows columns, and Reset to default restores the table", async ({ page, browser }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/audit");
  await resetAll(page, "/audit", "table.audit");
  const menu = page.locator('table[data-table="audit"]').locator("xpath=ancestor::*[contains(@class,'tablewrap')]").first().locator("details.colmenu");
  await menu.locator("summary").click();
  await expect(menu.getByRole("checkbox", { name: "When" })).toBeDisabled(); // the identifying column cannot be hidden
  if (shots) {
    mkdirSync(shots, { recursive: true });
    await page.screenshot({ path: join(shots, "audit-columns-menu-1280.png"), fullPage: false });
  }
  const done = saved(page, "table.audit");
  await menu.getByRole("checkbox", { name: "Target" }).uncheck();
  await done;
  await expect(headCell(page, "audit", "Target")).toBeHidden();
  await expectNoOverflow(page, "audit with a column hidden");

  await page.reload();
  await expect(headCell(page, "audit", "Target")).toBeHidden();
  const second = await otherBrowser(browser, adam);
  await second.page.goto(app.url + "/audit");
  await expect(headCell(second.page, "audit", "Target")).toBeHidden();
  await second.ctx.close();

  // show it again
  await page.locator("details.colmenu summary").first().click();
  const again = saved(page, "table.audit");
  await page.locator("details.colmenu").first().getByRole("checkbox", { name: "Target" }).check();
  await again;
  await expect(headCell(page, "audit", "Target")).toBeVisible();
  await page.locator("details.colmenu").first().getByRole("checkbox", { name: "Target" }).uncheck();

  // reset
  await page.locator("details.colmenu[open] [data-cols-reset], details.colmenu [data-cols-reset]").first().click();
  await page.waitForLoadState("load");
  await expect(headCell(page, "audit", "Target")).toBeVisible();
  await expect(page.locator('table[data-table="audit"]')).not.toHaveClass(/tfix/);
});

test("the page size is saved with the other preferences", async ({ page, browser }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/audit");
  await resetAll(page, "/audit", "pager");
  await expect(page.locator(".pager-size .is-current")).toHaveText("25");
  await page.locator(".pager-size").getByRole("link", { name: "100" }).click(); // the save races the navigation: no response to wait for
  await expect(page).toHaveURL(/per=100/);
  await page.waitForTimeout(300); // let the keepalive save land
  await page.goto(app.url + "/audit"); // no per= in the URL
  await expect(page.locator(".pager-size .is-current")).toHaveText("100");
  const second = await otherBrowser(browser, adam);
  await second.page.goto(app.url + "/audit");
  await expect(second.page.locator(".pager-size .is-current")).toHaveText("100");
  await second.ctx.close();
  const other = await otherBrowser(browser, eve);
  await other.page.goto(app.url + "/audit");
  await expect(other.page.locator(".pager-size .is-current")).toHaveText("25");
  await other.ctx.close();
  await resetAll(page, "/audit", "pager");
});

test("the endpoint refuses a missing token, unknown columns and oversize bodies", async ({ page }) => {
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  expect((await page.request.post(`${app.url}/prefs/table.devices`, { data: { h: ["family"] } })).status()).toBe(403); // no token
  expect(await postPref(page, "table.devices", { h: ["nope"] })).toBe(400);
  expect(await postPref(page, "table.devices", { h: ["name"] })).toBe(400);
  expect(await postPref(page, "table.devices", { w: { name: 10 }, x: 1 })).toBe(400);
  expect(await postPref(page, "table.devices", { h: ["a".repeat(10_000)] })).toBe(413);
  expect(await postPref(page, "table.devices", {})).toBe(204);
});

test("phone width: saved columns do not change the dense list", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  expect(await postPref(page, "table.devices", { h: ["family", "uptime"], w: { name: 40, status: 10 } })).toBe(204);
  await page.reload();
  const table = page.locator('table[data-table="devices"]');
  expect(await table.evaluate((t) => getComputedStyle(t).tableLayout)).not.toBe("fixed");
  await expect(page.locator("details.colmenu").first()).toBeHidden();
  await expect(page.locator(".colhandle").first()).toBeHidden();
  await expectNoOverflow(page, "devices on a phone with saved columns");
  if (shots) {
    mkdirSync(shots, { recursive: true });
    await page.screenshot({ path: join(shots, "devices-375-saved-columns.png"), fullPage: true });
  }
  await page.setViewportSize({ width: 1280, height: 800 });
  await resetAll(page, "/devices", "table.devices");
});

test("tooltips: chips explain themselves and a cut-off value shows its full text", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  await resetAll(page, "/devices", "table.devices");
  await expect(page.locator(".dv-st.t-ok").first()).toHaveAttribute("title", /^Online: /, { timeout: 40_000 });
  const family = page.locator("td.c-fam").first();
  await expect(family).toHaveAttribute("title", /only firmware built for this chip/i);
  // every badge in the device list carries an explanation
  const bare = await page.locator("table[data-table=devices] .bdg:not([title])").count();
  expect(bare).toBe(0);

  // narrow the Status column until its text is cut off, then hover the cell
  const handle = page.getByRole("separator", { name: "Resize column Status" });
  await handle.focus();
  for (let i = 0; i < 25; i++) await page.keyboard.press("ArrowLeft");
  const cell = page.locator('table[data-table="devices"] tbody td[data-label="Status"]').first();
  await cell.hover({ position: { x: 2, y: 2 } });
  await expect(cell).toHaveAttribute("data-tip", /\S/);
  expect(await cell.getAttribute("title")).toBeNull(); // one bubble only, no native title
  expect(await cell.evaluate((c) => c.scrollWidth > c.clientWidth)).toBe(true);

  await page.goto(app.url + "/settings/system");
  const help = await page.locator(".sys-tab:not([hidden]) .sf").count();
  expect(help).toBeGreaterThan(0);
  await resetAll(page, "/devices", "table.devices");
});

// A live list update replaces the table. Why: it may be fetched before the 400 ms debounced
// save of a column width reaches the server, and it removes the focused resize handle; the
// user must keep both the width they just set and the keyboard focus (found as a flaky
// "tooltips" test: the remaining arrow presses went nowhere).
test("a list update during keyboard resizing keeps the new width and the focused handle", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  await resetAll(page, "/devices", "table.devices");
  const th = page.locator('table[data-table="devices"] thead th', { hasText: "Status" });
  const handle = page.getByRole("separator", { name: "Resize column Status" });
  await handle.focus();
  const w0 = (await th.boundingBox())!.width;
  for (let i = 0; i < 5; i++) await page.keyboard.press("ArrowLeft");
  const w1 = (await th.boundingBox())!.width;
  expect(w1).toBeLessThan(w0 - 20);

  // the update arrives inside the save debounce: the server still has the old layout
  // listen before the request goes out: a fast answer must not slip past the wait
  const reloaded = page.waitForResponse((r) => r.url().includes("/devices/table"));
  await page.evaluate(() => {
    const t = document.getElementById("devtable")!;
    (window as any).htmx.ajax("GET", t.getAttribute("hx-get")!, { target: t, swap: "innerHTML" });
  });
  await reloaded;
  await expect.poll(async () => Math.abs((await th.boundingBox())!.width - w1)).toBeLessThan(1.5);
  await expect(page.getByRole("separator", { name: "Resize column Status" })).toBeFocused();

  for (let i = 0; i < 5; i++) await page.keyboard.press("ArrowLeft");
  expect((await th.boundingBox())!.width).toBeLessThan(w1 - 20);
  await resetAll(page, "/devices", "table.devices");
});

// A device event reloads the list (sse:device, 500 ms after a burst) and replaces the table, its
// handles and the one under the mouse. Why: when that lands while the user drags a column edge, the
// drag must go on and the width must still be saved; before, the release went to the removed handle,
// nothing was saved and the next update put the old width back (found as a flaky "dragging" test in
// the full CI, where the simulated devices come online during the first test).
test("a list update during a mouse drag keeps resizing and saves the width", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  await resetAll(page, "/devices", "table.devices");
  const before = await width(page, "devices", "Firmware");
  const box = (await page.getByRole("separator", { name: "Resize column Firmware" }).boundingBox())!;
  const x = box.x + box.width / 2,
    y = box.y + box.height / 2;
  const done = saved(page, "table.devices");
  await page.mouse.move(x, y);
  await page.mouse.down();
  await page.mouse.move(x + 20, y, { steps: 5 });

  const reloaded = page.waitForResponse((r) => r.url().includes("/devices/table"));
  await page.evaluate(() => {
    const t = document.getElementById("devtable")!;
    t.querySelector("table")!.setAttribute("data-old", "");
    (window as any).htmx.ajax("GET", t.getAttribute("hx-get")!, { target: t, swap: "innerHTML" });
  });
  await reloaded;
  // the table (and the handle the drag started on) has really been replaced
  await expect(page.locator('#devtable table[data-table="devices"]:not([data-old])')).toHaveCount(1);

  await page.mouse.move(x + 40, y, { steps: 5 });
  await page.mouse.up();
  await done;
  const dragged = await width(page, "devices", "Firmware");
  expect(dragged).toBeGreaterThan(before + 25);
  await expect(page.locator("body")).not.toHaveClass(/is-resizing/);
  await page.reload();
  // the page lays the saved widths out again when its fonts are in (tables.js enhance): wait for that
  await expect.poll(async () => Math.abs((await width(page, "devices", "Firmware")) - dragged)).toBeLessThan(3);
  await resetAll(page, "/devices", "table.devices");
});

// Why: two list updates overlap. The first was answered by the server before the user's new width
// was saved (old layout), the second left after the save. The first response then landed and was
// judged by the SECOND request's start ("the server had it already"), so the old layout stayed on
// screen until the second answer came (seen as the flaky mouse-drag test: width read in between).
// Every response is judged by its own start. The width is read in the swap's own task, right after the old answer lands.
test("a late answer to an older list request does not undo a width saved since", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  await resetAll(page, "/devices", "table.devices");
  const th = page.locator('table[data-table="devices"] thead th', { hasText: "Status" });
  const handle = page.getByRole("separator", { name: "Resize column Status" });

  // the first list request is answered with the old layout but delivered only when released
  let release!: () => void;
  const gate = new Promise<void>((r) => (release = r));
  let held!: () => void;
  const isHeld = new Promise<void>((r) => (held = r));
  let first = true;
  await page.route(/\/devices\/table/, async (route) => {
    if (!first) return route.continue();
    first = false;
    const old = await route.fetch();
    held();
    await gate;
    await route.fulfill({ response: old, headers: { ...old.headers(), "x-held": "1" } });
  });
  // a different source element each time: htmx queues requests of one element behind each other
  const ajax = (source: "devtable" | "body") =>
    page.evaluate((src) => {
      const t = document.getElementById("devtable")!;
      const from = src === "body" ? document.body : t;
      // not returned: htmx.ajax's promise settles with the swap, and the first one is held back
      void (window as any).htmx.ajax("GET", t.getAttribute("hx-get")!, { source: from, target: t, swap: "innerHTML" });
    }, source);
  await ajax("devtable");
  await isHeld;

  await handle.focus();
  const w0 = (await th.boundingBox())!.width;
  const done = saved(page, "table.devices");
  for (let i = 0; i < 5; i++) await page.keyboard.press("ArrowLeft");
  await done;
  const w1 = (await th.boundingBox())!.width;
  expect(w1).toBeLessThan(w0 - 20);

  // a second request leaves after the save; then the old answer lands
  await page.evaluate(() => {
    (window as any).__widths = [];
    document.addEventListener("htmx:afterSwap", (e) => {
      const h = [...document.querySelectorAll('table[data-table="devices"] thead th')].find((x) => x.textContent?.trim() === "Status");
      const xhr = (e as CustomEvent).detail.xhr as XMLHttpRequest;
      (window as any).__widths.push({ held: xhr.getResponseHeader("x-held") === "1", w: h ? h.getBoundingClientRect().width : 0 });
    });
  });
  type Swap = { held: boolean; w: number };
  const swaps = () => page.evaluate(() => (window as any).__widths as Swap[]);
  await ajax("body");
  // release the old answer only after a newer one has landed (an extra SSE refresh counts too)
  await expect.poll(async () => (await swaps()).some((x) => !x.held)).toBe(true);
  release();
  await expect.poll(async () => (await swaps()).some((x) => x.held)).toBe(true);
  const old = (await swaps()).find((x) => x.held)!;
  expect(Math.abs(old.w - w1)).toBeLessThan(1.5);
  await resetAll(page, "/devices", "table.devices");
});

// The other half of the flaky "tooltips" test: a list update that lands after the handle got
// focus but before the first arrow key (no layout was set yet, so nothing is restored by width).
// The new table's handles are only created in the same settle step, so the focus has to wait for
// them; before, the arrow presses went to the page and the column never narrowed.
test("a list update before the first arrow key keeps the focused handle", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + "/devices");
  await resetAll(page, "/devices", "table.devices");
  const th = page.locator('table[data-table="devices"] thead th', { hasText: "Status" });
  const handle = page.getByRole("separator", { name: "Resize column Status" });
  await handle.focus();
  const w0 = (await th.boundingBox())!.width;

  // swap the table and look at the focus in the very task the swap settles in
  const focusedAfterSwap = await page.evaluate(
    () =>
      new Promise<string>((resolve) => {
        document.addEventListener("htmx:afterSettle", () => queueMicrotask(() => resolve(document.activeElement?.getAttribute("aria-label") ?? "none")), { once: true });
        const t = document.getElementById("devtable")!;
        (window as any).htmx.ajax("GET", t.getAttribute("hx-get")!, { target: t, swap: "innerHTML" });
      }),
  );
  expect(focusedAfterSwap).toBe("Resize column Status");

  for (let i = 0; i < 5; i++) await page.keyboard.press("ArrowLeft");
  expect((await th.boundingBox())!.width).toBeLessThan(w0 - 20);
  await resetAll(page, "/devices", "table.devices");
});

test("the three main tables render at desktop and phone width (screenshots with UX_SHOTS)", async ({ page }, testInfo) => {
  const dir = shots ?? testInfo.outputPath("shots");
  mkdirSync(dir, { recursive: true });
  await page.setExtraHTTPHeaders(adam);
  for (const [name, w, h] of [["1280", 1280, 800], ["375", 375, 812]] as const) {
    await page.setViewportSize({ width: w, height: h });
    for (const [label, path] of [["devices", "/devices"], ["audit", "/audit"], ["transactions", "/transactions"]]) {
      await page.goto(app.url + path);
      await page.waitForLoadState("networkidle");
      await expectNoOverflow(page, `${label} at ${name}`);
      await page.screenshot({ path: join(dir, `${label}-${name}.png`), fullPage: true });
    }
  }
});

// v0.6.1 hotfix: hovering a page once copied the WHOLE page text into a native
// title (the cut-off check climbed from a container), and a second, native
// bubble showed next to the custom tooltip. Tooltips explain, they never list
// data: only cells get the cut-off text, every tooltip is at most 160
// characters, and no element carries both a title and the custom tooltip.
test("tooltips: never a page dump, never two bubbles, at most 160 characters", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  for (const path of ["/", "/devices", "/policy", "/policy/io", "/firmware", "/transactions", "/audit", "/settings/system", "/devices/new", "/discovery/scan", "/about"]) {
    const res = await page.goto(app.url + path);
    if (!res || res.status() >= 400) continue;
    await page.waitForLoadState("networkidle");
    // hover the containers and a spread of points over the page
    for (const sel of ["main", "main table", "main table tbody tr", "header"]) {
      const el = page.locator(sel).first();
      if (await el.count()) await el.hover({ force: true }).catch(() => {});
    }
    for (const y of [120, 260, 400, 540, 680]) for (const x of [80, 400, 760, 1100]) await page.mouse.move(x, y);
    const bad = await page.evaluate(() => {
      const out: string[] = [];
      for (const el of Array.from(document.querySelectorAll("[title], [data-tip]"))) {
        const t = el.getAttribute("data-tip") ?? el.getAttribute("title") ?? "";
        if (el.hasAttribute("title") && el.hasAttribute("data-tip")) out.push(`both: ${el.tagName}`);
        if (t.length > 160) out.push(`long (${t.length}): ${el.tagName}.${el.className}`);
        if (el.hasAttribute("data-autotitle") && !el.closest("td, th")) out.push(`auto outside a cell: ${el.tagName}`);
      }
      const tip = document.querySelector(".tooltip:not([hidden])");
      if (tip && (tip.textContent ?? "").length > 160) out.push("tooltip text over 160");
      return out;
    });
    expect(bad, path).toEqual([]);
  }
});

// v0.7.0: an (i) button inside a <label> becomes the label's control when it
// comes before the input (the first labelable element wins), so clicking the
// label text and screen readers would hit the button. Every label that wraps
// a form field must point at that field.
test("labels point at their own field, not at an (i) button", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(adam);
  for (const path of ["/devices/new", "/updates/new", "/settings/system", "/policy/rules/new", "/firmware", "/discovery/scan", "/settings/system/snapshots", "/setup"]) {
    const res = await page.goto(app.url + path);
    if (!res || res.status() >= 400) continue;
    const bad = await page.evaluate(() =>
      Array.from(document.querySelectorAll("label")).flatMap((l) => {
        const field = l.querySelector("input:not([type=hidden]), select, textarea");
        return field && l.control !== field ? [`${(l.textContent ?? "").trim().slice(0, 40)}`] : [];
      }),
    );
    expect(bad, path).toEqual([]);
  }
});
