import { test, expect } from "../support/fixtures";
import { realisticPolicyRules } from "../support/seed";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// QA v0.9.0 follow-up, packages K and L: a section title is a title and not a half-working toggle,
// and the policy pages read the same on a phone as on a desktop. CSS decides all of it, so it is
// checked on the computed styles.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_POLICY_RULES: JSON.stringify([...JSON.parse(realisticPolicyRules), { command: "PowerOnState", want: "2", note: "e2e target" }]),
    },
  });
  expect(app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)])).toContain("created: lamp-1m");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});
test.use({ extraHTTPHeaders: { "X-authentik-username": "adam", "X-authentik-groups": "adm" } });

// Why: the setup used to be four folds in two looks (framed and unframed, chevrons that
// contradicted the content). Design v2 makes it one card without folds (P8): nothing to open,
// nothing hidden, at every width.
test("Setup is one card without folds", async ({ page }) => {
  for (const width of [1280, 375]) {
    await page.setViewportSize({ width, height: 900 });
    await page.goto(app.url + "/setup");
    await expect(page.locator("main details")).toHaveCount(0);
    const bg = await page.locator(".auth-card").evaluate((c) => getComputedStyle(c).backgroundColor);
    expect(bg, "the card is not transparent").not.toBe("rgba(0, 0, 0, 0)");
    await expect(page.getByRole("button", { name: "Finish setup" })).toBeVisible();
  }
});

// Why: the policy overview is tiles, tabs and one table (design v2, N3), not folds: nothing is
// hidden behind a chevron on a desktop, and the rule page's cards are titled surfaces.
test("desktop: the policy overview is tiles, tabs and a table without folds", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/policy");
  await expect(page.locator(".dev-tile")).toHaveCount(5);
  await expect(page.locator("nav.utabs")).toBeVisible();
  await expect(page.locator("section#rules table.dense tbody tr").first()).toBeVisible();
  await page.locator("td.p1 a").first().click();
  for (const id of ["#dc-rule", "#dc-devs"]) {
    const sec = page.locator(`section:has(${id})`);
    await expect(sec.locator("h2").first(), `${id} title`).toBeVisible();
    const bg = await sec.evaluate((c) => getComputedStyle(c).backgroundColor);
    expect(bg, `${id} background`).not.toBe("rgba(0, 0, 0, 0)");
  }
});

// Why: the Compliance column is a fixed 230 px (approved design); when the bar took the whole rest
// of the row, "Local timezone" and "All devices · 27" wrapped onto two lines.
test("desktop: the compliance column stays 230 px so rule names and scopes do not wrap", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/policy");
  const cell = page.locator('section#rules td[data-label="Compliance"]:has(.health-bar)').first();
  await expect(cell).toBeVisible();
  const w = (await cell.boundingBox())!.width;
  expect(w, "compliance cell width").toBeLessThanOrEqual(232);
  for (const label of ["Rule", "Applies to"]) {
    const c = page.locator(`section#rules td[data-label="${label}"]`).first();
    const lines = await c.evaluate((e) => {
      const r = document.createRange();
      r.selectNodeContents(e);
      return new Set([...r.getClientRects()].map((q) => Math.round(q.top))).size;
    });
    expect(lines, `${label} stays on one line`).toBe(1);
  }
});

// Why: on a phone the rule table becomes cards and nothing scrolls sideways; each rule card keeps
// its name, its setting and its "⋯" menu on screen, and the drag handle (no drag on a phone) is gone.
test("phone: the rule table becomes cards and a rule card keeps its menu", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto(app.url + "/policy");
  const sw = await page.evaluate(() => document.documentElement.scrollWidth);
  expect(sw).toBeLessThanOrEqual(375);
  const card = page.locator("section#rules tbody tr").first();
  await expect(card.locator("td.p1 a")).toHaveText("Keep power state");
  await expect(card.locator("td[data-label=Setting]")).toContainText("SetOption0");
  await expect(card.locator(".pol-grip")).toBeHidden();
  const box = (await card.boundingBox())!;
  const menu = (await card.getByLabel(/^Actions for/).boundingBox())!;
  expect(menu.x + menu.width).toBeLessThanOrEqual(box.x + box.width + 0.5);
});

// Why: the Note had only a fold title as its label once; it is a labelled field at every width.
test("desktop: the rule page's Note has a visible label", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/policy/rules/new?command=SetOption0");
  const note = page.locator("#rule-form textarea[name=note]");
  await expect(note).toBeVisible();
  const label = note.locator("xpath=preceding-sibling::span[contains(@class,'ufield-label')]");
  await expect(label).toHaveText("Note");
  expect((await label.boundingBox())!.width, "visible label text").toBeGreaterThan(10);
});

// Why: an actual value in the rule page's devices card is one value; it must not break between its
// parts while a long device name has room to wrap. (A value too long for the card wraps instead, see
// policy-long-value.spec.ts, so this checks the rendered height, not white-space: nowrap.)
test("desktop: a devices-card value stays on one line", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/policy");
  await page.getByRole("button", { name: "Run now" }).click();
  await page.locator("td.p1 a").last().click(); // the e2e target rule (PowerOnState 2)
  const val = page.locator("#rule-devices td[data-label=Actual] .mono").first();
  await expect(val).toBeVisible({ timeout: 60_000 });
  const { height, font } = await val.evaluate((el) => ({ height: el.getBoundingClientRect().height, font: parseFloat(getComputedStyle(el).fontSize) }));
  expect(height, "a short value is one line (a second line would double the height)").toBeLessThan(font * 2);
});

// Why: the Make compliant plan (the page without JavaScript) keeps every card and its footer
// inside the screen on a phone.
test("phone: the make compliant plan stays inside the screen", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 812 });
  await page.goto(app.url + "/policy");
  await page.getByRole("button", { name: "Run now" }).click();
  await page.locator("form:has(button[data-dialog=dlg-fix])").evaluate((f: HTMLFormElement) => f.submit()); // the no-JS post
  await expect(page.locator(".pol-fix-card").first()).toBeVisible({ timeout: 15_000 });
  for (const b of await page.locator(".pol-fix-card, .pol-fix-foot").all()) {
    const r = (await b.boundingBox())!;
    expect(r.x).toBeGreaterThanOrEqual(0);
    expect(r.x + r.width).toBeLessThanOrEqual(375);
  }
});
