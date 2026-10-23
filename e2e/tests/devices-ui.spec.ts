import { mkdirSync } from "node:fs";
import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// The devices table (v0.5.1 feedback): row actions stay inside their rows on
// every width, the Columns button sits in the toolbar, columns sort on the
// server and the choice is remembered, tooltips are short and readable.
// UX_SHOTS=<dir> keeps screenshots.
const shots = process.env.UX_SHOTS;
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
let app: App;
let env: Devenv;

test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
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
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

async function open(page: Page, width: number, path = "/devices") {
  await page.setViewportSize({ width, height: 900 });
  await page.setExtraHTTPHeaders(adam);
  await page.goto(app.url + path);
  // wait for the first health reports: uptime and Wi-Fi come with them (a phone shows cards, not the table)
  await expect(page.locator(width < 720 ? "li.dcard" : "table[data-table=devices] tbody tr").first()).toBeVisible();
  // the CLI import runs after the server started; the tracker picks the devices up within one tick (5 s), then polls at once
  await page.locator("table[data-table=devices] .pwr").first().waitFor({ state: "attached", timeout: 20_000 });
}

async function shot(page: Page, name: string) {
  if (!shots) return;
  mkdirSync(shots, { recursive: true });
  await page.screenshot({ path: `${shots}/${name}.png`, fullPage: true });
}

for (const width of [375, 800, 1280]) {
  // Item 1: the toggle used to poke out above and past its row on a phone. A phone shows the
  // device cards (design v2), a wider screen the table rows.
  test(`row actions lie inside their row at ${width}px`, async ({ page }) => {
    await open(page, width);
    await expect(page.locator(".pwr").filter({ visible: true }).first()).toBeVisible({ timeout: 30_000 });
    const bad = await page.evaluate((rowSel) => {
      const out: string[] = [];
      const rows = document.querySelectorAll(rowSel);
      if (rows.length === 0) out.push(`no ${rowSel}`);
      for (const row of rows) {
        const r = row.getBoundingClientRect();
        for (const b of row.querySelectorAll<HTMLElement>("button, .button, input[type=checkbox], label.hit")) {
          const q = b.getBoundingClientRect();
          if (q.width === 0 || q.height === 0 || !b.checkVisibility()) continue;
          const eps = 0.5;
          if (q.top < r.top - eps || q.bottom > r.bottom + eps || q.left < r.left - eps || q.right > r.right + eps)
            out.push(`${b.className || b.tagName}: ${JSON.stringify(q)} outside row ${JSON.stringify(r)}`);
        }
      }
      return out;
    }, width < 720 ? "li.dcard" : "table[data-table=devices] tbody tr");
    expect(bad).toEqual([]);
    await expectNoOverflow(page, `devices at ${width}`);
    await shot(page, `devices-${width}`);
  });
}

test("phone: the power button sits on the name's line of its card", async ({ page }) => {
  await open(page, 375);
  const miss = await page.evaluate(() => {
    // the phone card puts name and power on the first line (QA #67, prototype card): the button spans the name line's centre
    const card = [...document.querySelectorAll("li.dcard")].find((c) => c.querySelector(".pwr"))!;
    const n = card.querySelector(".dcard-name")!.getBoundingClientRect(), b = card.querySelector(".pwr")!.getBoundingClientRect();
    const firstLine = n.top + 9; // half of the name's first line (13.5 px, line height 1.3)
    return firstLine < b.top || firstLine > b.bottom ? `name line at ${firstLine}, button ${b.top}-${b.bottom}` : "";
  });
  expect(miss).toBe("");
});

test("Columns button shares the toolbar's vertical centre and height", async ({ page }) => {
  await open(page, 1280);
  // the toolbar's first row: search, filter chips, Chip and Tag, and Columns at its end (design v2)
  const items = page.locator(".dv-q, .dv-chip, .dv-pick, .dv-cols details.colmenu > summary");
  await expect(items.last()).toBeVisible();
  // One in-page measurement: #devtable reloads 0.5 s after the first health reports, and separate calls
  // could read the toolbar from the old table and the frame from a node the swap had already removed.
  const { boxes, tableRight } = await page.evaluate(() => {
    const sel = ".dv-q, .dv-chip, .dv-pick, .dv-cols details.colmenu > summary";
    const boxes = [...document.querySelectorAll(sel)]
      .filter((e) => e.getBoundingClientRect().width > 0)
      .map((e) => { const r = e.getBoundingClientRect(); return { c: r.top + r.height / 2, h: r.height, right: r.right }; });
    const frame = document.querySelector(".tablewrap[data-key=devices] > .tablescroll");
    return { boxes, tableRight: frame ? frame.getBoundingClientRect().right : NaN };
  });
  expect(boxes.length).toBeGreaterThan(1);
  for (const b of boxes) {
    expect(Math.abs(b.c - boxes[0].c)).toBeLessThanOrEqual(1);
    expect(Math.abs(b.h - boxes[0].h)).toBeLessThanOrEqual(1);
  }
  // right-aligned at the edge of the table's frame (the table scrolls inside it)
  expect(Math.abs(boxes[boxes.length - 1].right - tableRight)).toBeLessThanOrEqual(4);
  await shot(page, "devices-1280-columns");
  // another table (no toolbar): the button has a bar of its own, right-aligned
  await page.goto(app.url + "/audit");
  const sum = page.locator(".tabletools summary").first();
  await expect(sum).toBeVisible();
  const gap = await page.evaluate(() => {
    const r = (e: Element | null) => e!.getBoundingClientRect().right;
    return Math.abs(r(document.querySelector(".tabletools summary")) - r(document.querySelector("table[data-table=audit]")));
  });
  expect(gap).toBeLessThanOrEqual(4);
});

// Design v2 (P2): Last restart is shown like the prototype's; hiding it is saved, and Reset brings it back.
test("Uptime and Wi-Fi are separate columns; Last restart is shown until hidden", async ({ page }) => {
  await open(page, 1280);
  const head = page.locator("table[data-table=devices] thead th");
  await expect(head.filter({ hasText: /^Uptime$/ })).toBeVisible();
  await expect(head.filter({ hasText: /^Wi-Fi$/ })).toBeVisible();
  await expect(page.locator("table[data-table=devices] thead th", { hasText: "Vitals" })).toHaveCount(0);
  await expect(head.filter({ hasText: /Last restart/ })).toBeVisible();
  await page.locator("details.colmenu > summary").first().click();
  const saved = page.waitForResponse((r) => r.url().endsWith("/prefs/table.devices") && r.status() === 204);
  await page.getByRole("checkbox", { name: "Last restart" }).uncheck();
  await saved;
  await expect(head.filter({ hasText: /Last restart/ })).toBeHidden();
  await page.reload();
  await expect(head.filter({ hasText: /Last restart/ })).toBeHidden();
  await page.locator("details.colmenu > summary").first().click();
  const reset = page.waitForResponse((r) => r.url().endsWith("/prefs/table.devices"));
  await page.getByRole("button", { name: "Reset to default" }).click();
  await reset;
  await page.waitForLoadState();
  await expect(head.filter({ hasText: /Last restart/ })).toBeVisible();
});

test("sorting: header links sort on the server, flip, keep filters and are remembered", async ({ page }) => {
  await open(page, 1280);
  const link = page.locator("th a.sortlink", { hasText: "Name" });
  await expect(link).toHaveAttribute("href", /sort=name&dir=desc/); // name is the default sort: next click flips
  const namesOf = () => page.locator("table[data-table=devices] tbody td[data-label=Name] a").allTextContents();
  const asc = await namesOf();
  expect(asc.length).toBeGreaterThan(2);
  await link.click();
  await expect(page).toHaveURL(/sort=name&dir=desc/);
  expect(await namesOf()).toEqual([...asc].reverse());
  await expect(page.locator("th", { has: page.locator("a.sortlink") }).filter({ hasText: "Name" })).toHaveAttribute("aria-sort", "descending");
  // remembered: a plain visit shows the last sort
  await page.goto(app.url + "/devices");
  expect(await namesOf()).toEqual([...asc].reverse());
  // filters survive the sort links
  await page.goto(app.url + "/devices?q=a");
  await expect(page.locator("th a.sortlink", { hasText: "Address" })).toHaveAttribute("href", /q=a&sort=address&dir=asc/);
  // the SSE refresh keeps the order (the table fragment carries the sort)
  const frag = await (await page.request.get(app.url + "/devices/table?sort=name&dir=desc", { headers: adam })).text();
  expect(frag).toContain('aria-sort="descending"');
  await page.request.get(app.url + "/devices?sort=name&dir=asc", { headers: adam });
});

test("header tooltips are one short sentence; the tooltip is narrow and readable", async ({ page }) => {
  await open(page, 1280);
  for (const th of await page.locator("table[data-table=devices] thead th[title]").all()) {
    const t = (await th.getAttribute("title"))!;
    expect(t.length, t).toBeLessThanOrEqual(80);
    expect(t).not.toMatch(/drag|resize|click|more than|%/i);
  }
  const th = page.locator("table[data-table=devices] thead th", { hasText: /^Firmware$/ }); // a header left of the pinned right columns: further right it may sit under them at 1280
  await th.hover();
  const tip = page.locator(".tooltip");
  await expect(tip).toBeVisible();
  await expect(tip).toHaveText("Installed Tasmota version and build.");
  const { w, fs, lh } = await tip.evaluate((e) => { const cs = getComputedStyle(e); return { w: e.getBoundingClientRect().width, fs: parseFloat(cs.fontSize), lh: parseFloat(cs.lineHeight) }; });
  expect(w).toBeLessThanOrEqual(32 * fs * 0.62 + 40); // ~32ch
  expect(lh / fs).toBeGreaterThanOrEqual(1.3);
  for (const scheme of ["light", "dark"] as const) {
    await page.emulateMedia({ colorScheme: scheme });
    const [fg, bg] = await tip.evaluate((e) => { const cs = getComputedStyle(e); return [cs.color, cs.backgroundColor]; });
    const lum = (c: string) => { const [r, g, b] = c.match(/\d+/g)!.slice(0, 3).map((x) => { const v = +x / 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; }); return 0.2126 * r + 0.7152 * g + 0.0722 * b; };
    const ratio = (Math.max(lum(fg), lum(bg)) + 0.05) / (Math.min(lum(fg), lum(bg)) + 0.05);
    expect(ratio, scheme).toBeGreaterThanOrEqual(7);
  }
});


// Item 9: double-click (or Enter) on a handle fits the column to its widest content, capped at 40ch, and saves it.
test("autofit: double-click and Enter on a resize handle fit the column; the width survives a reload", async ({ page }) => {
  await open(page, 1280);
  const cellsFit = () =>
    page.evaluate(() => {
      const th = [...document.querySelectorAll("table[data-table=devices] thead th")].find((t) => t.textContent!.trim() === "Address")!;
      const n = (th as HTMLTableCellElement).cellIndex;
      const cells = [...document.querySelectorAll<HTMLTableCellElement>("table[data-table=devices] tbody tr")].map((r) => r.cells[n]).filter(Boolean);
      const probe = document.createElement("span");
      probe.style.cssText = "position:absolute;visibility:hidden;white-space:nowrap";
      th.appendChild(probe);
      probe.textContent = "0".repeat(40);
      const cap = probe.getBoundingClientRect().width;
      probe.remove();
      return { width: th.getBoundingClientRect().width, clipped: cells.filter((c) => c.scrollWidth > c.clientWidth + 1).length, cap };
    });
  // make the column too narrow first
  const handle = page.getByRole("separator", { name: "Resize column Address" });
  await handle.focus();
  const narrowed = page.waitForResponse((r) => r.url().endsWith("/prefs/table.devices") && r.status() === 204);
  for (let i = 0; i < 12; i++) await page.keyboard.press("ArrowLeft");
  await narrowed;
  const narrow = await cellsFit();
  expect(narrow.clipped).toBeGreaterThan(0);
  const done = page.waitForResponse((r) => r.url().endsWith("/prefs/table.devices") && r.status() === 204);
  await handle.dblclick();
  await done;
  const fitted = await cellsFit();
  expect(fitted.width).toBeGreaterThan(narrow.width);
  expect(fitted.clipped === 0 || fitted.width >= fitted.cap - 2, `clipped ${fitted.clipped}, width ${fitted.width}, cap ${fitted.cap}`).toBe(true);
  await page.reload();
  expect(Math.abs((await cellsFit()).width - fitted.width)).toBeLessThan(3);
  // keyboard: Enter fits again after a manual change
  await page.getByRole("separator", { name: "Resize column Address" }).focus();
  for (let i = 0; i < 6; i++) await page.keyboard.press("ArrowLeft");
  const enter = page.waitForResponse((r) => r.url().endsWith("/prefs/table.devices") && r.status() === 204);
  await page.keyboard.press("Enter");
  await enter;
  const again = await cellsFit();
  expect(again.clipped === 0 || again.width >= again.cap - 2, `after Enter: width ${again.width}, clipped ${again.clipped}`).toBe(true);
  expect(again.width).toBeGreaterThan(narrow.width); // the manual step left it narrower than that
  await expectNoOverflow(page, "devices after autofit");
});

// Item 10: column order, by drag and by the move buttons; saved, rendered by the server. Name stays first
// and Actions last (fixed to their edge, D19); Power is pinned right and stays before Actions.
// A save is followed by a reload of the page: poll until the new order shows (a read during the reload is just retried).
const heads = (page: Page) => page.locator("table[data-table=devices] thead th").evaluateAll((t) => t.map((x) => x.textContent!.trim()).filter((x) => x && x !== "Select" && x !== "Actions")).catch(() => [] as string[]);
const headsAre = (page: Page, want: string[], n = want.length) => expect.poll(async () => (await heads(page)).slice(0, n)).toEqual(want);

test("reorder: dragging a header and the move buttons change the order; it survives a reload; Reset restores it", async ({ page }) => {
  await open(page, 1280);
  await page.goto(app.url + "/devices");
  await page.locator("details.colmenu > summary").first().click();
  await page.getByRole("button", { name: "Reset to default" }).click();
  await headsAre(page, ["Name", "Status"]);
  const base = await heads(page);
  expect(base[0]).toBe("Name");
  expect(base[base.length - 1]).toBe("Power");

  // drag Address onto the left half of Status
  const address = page.locator("table[data-table=devices] thead th", { hasText: /^Address$/ });
  const status = page.locator("table[data-table=devices] thead th", { hasText: /^Status/ });
  const sb = (await status.boundingBox())!;
  const saved = page.waitForResponse((r) => r.url().endsWith("/prefs/table.devices") && r.status() === 204);
  await address.dragTo(status, { targetPosition: { x: 6, y: sb.height / 2 } });
  await saved;
  await headsAre(page, ["Name", "Address", "Status"]);
  let now = await heads(page);
  expect(now[now.length - 1]).toBe("Power");
  // the rows moved with their headers
  const firstRow = await page.locator("table[data-table=devices] tbody tr").first().locator("td").evaluateAll((tds) => tds.map((t) => (t as HTMLElement).dataset.label));
  expect(firstRow.slice(0, 4)).toEqual(["Select", "Name", "Address", "Status"]);
  // rendered by the server: the HTML already has the order (no jump)
  const html = await (await page.request.get(app.url + "/devices", { headers: adam })).text();
  const thead = html.slice(html.indexOf("<thead"), html.indexOf("</thead>"));
  expect(thead.indexOf(">Address")).toBeGreaterThan(0);
  expect(thead.indexOf(">Address")).toBeLessThan(thead.indexOf(">Status"));
  await page.reload();
  await headsAre(page, ["Name", "Address", "Status"]);
  await expectNoOverflow(page, "devices reordered");

  // keyboard: the move buttons of the Columns menu; Name and Actions cannot be moved
  await page.locator("details.colmenu > summary").first().click();
  await expect(page.locator("[data-moveid=name], [data-moveid=actions]")).toHaveCount(0);
  const saved2 = page.waitForResponse((r) => r.url().endsWith("/prefs/table.devices") && r.status() === 204);
  await page.getByRole("button", { name: "Move Status up" }).focus();
  await page.keyboard.press("Enter");
  await saved2;
  await headsAre(page, ["Name", "Status", "Address"]);
  await page.reload();
  await headsAre(page, ["Name", "Status", "Address"]);

  // a column fixed to its edge in the prefs is refused by the server
  const bad = await page.evaluate(async () => {
    const token = JSON.parse(document.body.getAttribute("hx-headers") ?? "{}")["X-CSRF-Token"];
    const r = await fetch(`${document.body.getAttribute("data-prefs")}/table.devices`, { method: "POST", headers: { "Content-Type": "application/json", "X-CSRF-Token": token }, body: JSON.stringify({ o: ["actions", "status"] }) });
    return r.status;
  });
  expect(bad).toBe(400);

  // Reset restores the default order
  await page.locator("details.colmenu > summary").first().click();
  await page.getByRole("button", { name: "Reset to default" }).click();
  await headsAre(page, base);
});

test("device settings: no flash choices, one hint next to Save", async ({ page }) => {
  await open(page, 1280);
  const href = (await page.locator("table[data-table=devices] td[data-label=Name] a").first().getAttribute("href"))!;
  for (const width of [375, 1280]) {
    await page.setViewportSize({ width, height: 900 });
    await page.goto(app.url + href + "/settings");
    await expect(page.locator("nav.utabs a[aria-current=page]")).toHaveText("Settings");
    await expect(page.locator("input[name=confirm], input[name=save]")).toHaveCount(0);
    await expect(page.getByText("Applied immediately and saved to the device once.")).toBeVisible();
    await expectNoOverflow(page, `device settings at ${width}`);
    await shot(page, `device-settings-${width}`);
  }
});

// The list reloads itself on server events. A selection must not freeze it (stale states for
// as long as devices stay selected): the swap runs and the selection and the bulk bar count
// survive it.
test("a list update is applied while rows are selected and keeps the selection", async ({ page }) => {
  await open(page, 1280);
  const rows = page.locator("table[data-table=devices] tbody tr");
  const link = rows.first().locator("td[data-label=Name] a").first();
  const href = (await link.getAttribute("href"))!;
  const newName = "renamed-while-selected";
  await rows.nth(0).locator("input[name=id]").check();
  await rows.nth(1).locator("input[name=id]").check();
  await expect(page.locator("[data-bulkbar]").first()).toBeVisible();
  const countBefore = (await page.locator("[data-bar-label]").first().textContent())?.replace(/\s+/g, " ").trim();

  // another browser renames the device: the server sends a device event
  const other = await page.context().newPage();
  await other.setExtraHTTPHeaders(adam);
  await other.goto(app.url + href + "/edit");
  await other.locator("input[name=name]").fill(newName);
  await other.getByRole("button", { name: /Save changes/ }).click();
  await other.close();

  await expect(page.locator(`table[data-table=devices] a:text-is("${newName}")`)).toBeVisible({ timeout: 10_000 });
  await expect(page.locator('table[data-table=devices] input[name=id]:checked')).toHaveCount(2);
  await expect(page.locator("[data-bar-label]").first()).toHaveText(countBefore!);
  await expect(page.locator("[data-bulkbar]").first()).toBeVisible();
});

// A row menu that is open must not be closed by a reload; the held update runs when it closes.
test("a list update held back by an open row menu is applied when it closes", async ({ page }) => {
  await open(page, 1280);
  const rows = page.locator("table[data-table=devices] tbody tr");
  const link = rows.first().locator("td[data-label=Name] a").first();
  const href = (await link.getAttribute("href"))!;
  const oldName = (await link.textContent())!.trim();
  const newName = "held-back-rename";
  await rows.first().locator("details > summary").last().click();
  await expect(rows.first().locator("details[open]")).toHaveCount(1);

  const refetched = page.waitForResponse((r) => r.url().includes("/devices/table"));
  const other = await page.context().newPage();
  await other.setExtraHTTPHeaders(adam);
  await other.goto(app.url + href + "/edit");
  await other.locator("input[name=name]").fill(newName);
  await other.getByRole("button", { name: /Save changes/ }).click();
  await other.close();

  await refetched;
  await page.waitForTimeout(500); // let a wrongly applied swap show
  await expect(page.locator(`table[data-table=devices] a:text-is("${oldName}")`)).toBeVisible();
  await expect(rows.first().locator("details[open]")).toHaveCount(1);

  await rows.first().locator("details > summary").last().click(); // close the menu
  await expect(page.locator(`table[data-table=devices] a:text-is("${newName}")`)).toBeVisible({ timeout: 1_500 });
});
