import { mkdirSync } from "node:fs";
import { join } from "node:path";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import { realisticPolicyRules } from "../support/seed";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// v0.7.0 shared components: one page header, one card, one badge, the tooltip
// rule, sorting of the remaining tables and the Users entry of the System menu.
// Set UX_SHOTS=<dir> to keep 1280/375 screenshots of a few pages.
const shots = process.env.UX_SHOTS;
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
const password = "e2e-fake-password-1";

let app: App;
let local: App;
let env: Devenv;
let deviceID = "1";

test.beforeAll(async ({ browser }) => {
  test.setTimeout(120_000);
  env = await startDevenv(["-fleet-size", "6", "-releases", "3"]);
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
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
  local = await startApp({ env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.2/32", TASMOFLEET_ADMIN_GROUPS: "adm" } });
  local.cli(["user", "passwd", "--create", "--role", "admin", "breakglass"], `${password}\n`);
  local.cli(["user", "passwd", "--create", "--role", "operator", "amy"], `${password}\n`);
  local.cli(["user", "passwd", "--create", "--role", "operator", "bob"], `${password}\n`);
  const p = await browser.newPage({ extraHTTPHeaders: admin });
  await p.goto(app.url + "/devices");
  const href = await p.locator("table tbody tr td a").first().getAttribute("href");
  deviceID = href?.match(/\/devices\/(\d+)/)?.[1] ?? "1";
  await p.close();
});
test.afterAll(async () => {
  await app?.stop();
  await local?.stop();
  await env?.stop();
});

const viewports = [
  { name: "desktop", width: 1280, height: 800 },
  { name: "phone", width: 375, height: 812 },
];

// Pages of the signed-in app (header auth, admin), by path.
const pagePaths = (id: string) => [
  "/", "/devices", "/devices/new", "/discovery", "/updates/new", "/transactions", "/firmware", "/policy", "/policy/io",
  "/audit", "/settings/system", "/about", "/setup", `/devices/${id}`, `/devices/${id}/edit`, `/devices/${id}/console`,
  `/devices/${id}/backups`, `/devices/${id}/settings`, "/devices/9999",
];

// Why: the title of a page jumped by up to 28 px between pages because the row
// height depended on a button on the right. One header component with a fixed
// title row keeps the title where the eye expects it, so the position must be
// identical, not "about the same".
for (const vp of viewports) {
  test(`page header: h1 top and left identical on every page, ${vp.name}`, async ({ page }) => {
    test.setTimeout(120_000);
    await page.setViewportSize(vp);
    await page.setExtraHTTPHeaders(admin);
    const pos: Record<string, string> = {};
    for (const path of pagePaths(deviceID)) {
      const res = await page.goto(app.url + path);
      expect(res?.status(), path).toBeLessThan(500);
      pos[path] = await page.evaluate(() => {
        const h = document.querySelector("main h1");
        if (!h) return "no h1";
        const r = h.getBoundingClientRect();
        // a page with a breadcrumb trail shows it above the title (design port); pages not ported yet keep p.crumb
        const head = document.querySelector("main .page-head");
        // pages ported to templ (design v2) have no .page-head: the shell's crumbs, then the title
        // the error page (design v2, P9) centres its own title block: a layout of its own
        if (document.querySelector("main .errpage")) return `error ${r.top.toFixed(2)}/${r.left.toFixed(2)}`;
        // the first-run setup is a page without navigation (main.bare): the card centred under the brand
        const kind = document.querySelector("main.bare") ? "bare" : !head ? "ported" : document.querySelector("main nav.crumb.trail") ? "trail" : document.querySelector("main .crumb") ? "crumb" : head.getBoundingClientRect().height > 70 ? "wrapped" : "none";
        return `${kind} ${r.top.toFixed(2)}/${r.left.toFixed(2)}`;
      });
      expect(await page.locator("main h1").count(), `${path} has exactly one h1`).toBe(1);
      expect(await page.locator("main > h1, main > section > h1").count(), `${path}: the h1 belongs in .page-head`).toBe(0);
    }
    // identical within each kind of header
    for (const kind of ["none", "trail", "crumb", "wrapped", "ported"]) {
      const same = Object.values(pos).filter((v) => v.startsWith(kind + " "));
      expect(new Set(same).size, `${kind}: h1 top/left differ between pages: ${JSON.stringify(pos)}`).toBeLessThanOrEqual(1);
    }
  });
}

// Why: titles were inside the box on some cards and in the frame on others. A
// card's title sits on its top border (like a fieldset legend), on every page,
// phone included.
for (const vp of viewports) {
  test(`card: every card has its title in the frame, ${vp.name}`, async ({ page }) => {
    test.setTimeout(120_000);
    await page.setViewportSize(vp);
    await page.setExtraHTTPHeaders(admin);
    let cards = 0;
    for (const path of pagePaths(deviceID)) {
      await page.goto(app.url + path);
      const bad = await page.evaluate(() => {
        const out: string[] = [];
        for (const c of document.querySelectorAll(".card")) {
          const title = c.querySelector(":scope > h2:first-child, :scope > .card-head, :scope > details > summary, :scope > legend");
          if (!title || !(title.textContent ?? "").trim()) { out.push(`card without a title: ${c.className}`); continue; }
          const cr = c.getBoundingClientRect(), tr = title.getBoundingClientRect();
          if (cr.height === 0) continue;
          // A collapsible card on a phone is one header row: its title is the first line of the card, not a legend on the border.
          if (getComputedStyle(title).position !== "absolute") {
            if (title.tagName === "SUMMARY" && tr.top - cr.top > 3) out.push(`"${(title.textContent ?? "").trim().slice(0, 30)}" header row is ${Math.round(tr.top - cr.top)}px below the card top`);
            else if (title.tagName !== "SUMMARY") out.push(`"${(title.textContent ?? "").trim().slice(0, 30)}" is not on the frame`);
            continue;
          }
          const mid = tr.top + tr.height / 2;
          if (Math.abs(mid - cr.top) > 3) out.push(`"${(title.textContent ?? "").trim().slice(0, 30)}" is ${Math.round(mid - cr.top)}px off the frame`);
        }
        for (const f of document.querySelectorAll("main fieldset")) {
          const lg = f.querySelector(":scope > legend");
          if (!lg || !(lg.textContent ?? "").trim()) out.push("fieldset without a legend");
        }
        return { out, n: document.querySelectorAll(".card").length };
      });
      expect(bad.out, path).toEqual([]);
      cards += bad.n;
    }
    // The legacy card (title on the frame) left with the last html/template pages of this list: the
    // policy pages became design v2 cards (W1). A legacy card that comes back is still checked above.
    expect(cards, "no legacy card is left on these pages").toBe(0);
  });
}

// Why: "✓ok" - glyph and word were glued where a table cell clamps the badge.
// The glyph has its own space, measured on the real pages.
test("badge: a gap between the icon and the text, app-wide", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(admin);
  let seen = 0;
  for (const path of ["/", "/devices", "/audit", "/transactions", "/policy", `/devices/${deviceID}`, "/firmware"]) {
    await page.goto(app.url + path);
    const res = await page.evaluate(() => {
      const bad: string[] = [];
      let n = 0;
      for (const b of document.querySelectorAll(".badge")) {
        const cs = getComputedStyle(b, "::before");
        if (cs.content === "none" || cs.content === "normal") continue;
        n++;
        const gap = parseFloat(cs.marginRight) || 0;
        const text = (b.textContent ?? "").trim();
        if (gap < 4 && getComputedStyle(b).display.includes("flex") === false) bad.push(`${text}: gap ${gap}px`);
        // the rendered distance between the glyph box and the first text box
        const r = document.createRange();
        const tn = [...b.childNodes].find((c) => c.nodeType === 3 && (c.textContent ?? "").trim());
        if (!tn) continue;
        r.setStart(tn, 0); r.setEnd(tn, 1);
        const tr = r.getBoundingClientRect(), br = b.getBoundingClientRect();
        if (tr.width > 0 && tr.left - br.left < 14) bad.push(`${text}: text starts ${Math.round(tr.left - br.left)}px from the badge edge`);
      }
      return { bad, n };
    });
    expect(res.bad, path).toEqual([]);
    seen += res.n;
  }
  expect(seen, "the pages show badges").toBeGreaterThan(5);
});

// Why: a tooltip once listed devices (the update's Preview, now Review update) and one page dump
// copied into a title. Server-rendered titles follow the same rule as the script's: at most
// 160 characters, 1-2 short sentences, and the button that goes on needs none.
test("tooltips: short, never data lists, none on the Review update button", async ({ page }) => {
  test.setTimeout(120_000);
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.setExtraHTTPHeaders(admin);
  const names = Object.keys(env.devices);
  for (const path of pagePaths(deviceID)) {
    await page.goto(app.url + path);
    const bad = await page.evaluate(() => {
      const out: string[] = [];
      for (const el of document.querySelectorAll("[title], [data-tip]")) {
        const t = el.getAttribute("title") ?? el.getAttribute("data-tip") ?? "";
        if (t.length > 160) out.push(`long (${t.length}): ${el.tagName}.${el.className}`);
        if ((t.match(/[.!?](\s|$)/g) ?? []).length > 2) out.push(`more than two sentences: ${t.slice(0, 40)}`);
        if (el.matches("button, a.button") && !el.matches(".info, .relay-btn") && el.textContent && /^(Preview|Start|Save|Delete|Add|Edit|Send)/.test(el.textContent.trim())) out.push(`self-explanatory button has a tooltip: ${el.textContent.trim()}`);
      }
      return out;
    });
    expect(bad, path).toEqual([]);
  }
  await page.goto(app.url + "/updates/new");
  for (const n of names.slice(0, 3)) await page.getByLabel(`Select ${n}`, { exact: true }).check();
  const preview = page.getByRole("button", { name: "Review update" });
  for (let i = 0; i < (await preview.count()); i++) {
    const t = await preview.nth(i).evaluate((e) => (e.getAttribute("title") ?? "") + (e.getAttribute("data-tip") ?? "") + (e.closest("[title]")?.getAttribute("title") ?? ""));
    for (const n of names) expect(t, "Review update tooltip must not list devices").not.toContain(n);
  }
  await preview.first().hover();
  await page.waitForTimeout(600);
  const shown = await page.locator(".tooltip:not([hidden])").allTextContents();
  for (const n of names) expect(shown.join(" ")).not.toContain(n);
});

// Why (design v2, P9): the users page is the prototype's list, not a sortable table: you first,
// then everybody by name, so the own row is always where you look for it.
test("users: the list puts you first, then everybody by name", async ({ page }) => {
  await page.goto(local.url + "/login");
  await page.getByLabel("Username").fill("breakglass");
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.goto(local.url + "/users");
  const users = async () => (await page.locator(".urow .urow-name > span:first-child").allTextContents()).map((s) => s.trim());
  await expect.poll(users).toEqual(["breakglass", "amy", "bob"]);
  await expect(page.locator("table")).toHaveCount(0);
});

// Why (finding 82): a disabled button is not a busy button. Select all/none on an empty list
// looked like they were loading forever; only a request in flight spins. The device list has no
// "Select all shown" button any more (design v2: the header box selects), so a disabled button of
// each kind is checked against the page's real stylesheet.
test("disabled buttons show no busy spinner", async ({ page }) => {
  await page.goto(local.url + "/login");
  await page.getByLabel("Username").fill("breakglass");
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.goto(local.url + "/devices");
  const contents = await page.evaluate(() => {
    const out: string[] = [];
    for (const cls of ["", "sm", "btn", "btn btn-sm", "btn btn-primary"]) {
      const b = document.createElement("button");
      b.className = cls;
      b.disabled = true;
      b.textContent = "Select all";
      document.querySelector("main")!.appendChild(b);
      out.push(getComputedStyle(b, "::after").content);
      b.remove();
    }
    return out;
  });
  expect(contents).toEqual(["none", "none", "none", "none", "none"]);
});

// Why (finding 88): the menu of your own row must not offer what would lock you out; the items
// stay visible, disabled, with the reason (design v2, P9). The server refuses those too
// (internal/auth tests); here only the controls are checked.
test("users: no Deactivate, Delete or role change on your own row", async ({ page }) => {
  await page.goto(local.url + "/login");
  await page.getByLabel("Username").fill("breakglass");
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await page.goto(local.url + "/users");
  const own = page.locator(".urow", { hasText: "breakglass" });
  const other = page.locator(".urow", { hasText: "amy" });
  // one menu is open at a time: the own row first, then the other
  await own.getByLabel("User actions").click();
  for (const name of ["Deactivate", /^Delete/, /^Make (admin|operator)/]) {
    await expect(own.getByRole("menuitem", { name })).toHaveAttribute("aria-disabled", "true");
  }
  await expect(own.getByText("You cannot delete your own account")).toBeVisible();
  await page.keyboard.press("Escape"); // the open menu lies over the next row
  await other.getByLabel("User actions").click();
  for (const name of ["Deactivate", "Delete…", "Make admin"]) {
    await expect(other.getByRole("menuitem", { name, exact: true })).not.toHaveAttribute("aria-disabled", "true");
  }
});

// Why: the Users page had no menu entry (only the address). Admins find it under System. The
// navigation is the same for every role (F4): an operator sees it too, and the page refuses them.
test("nav: System menu has Users for every role; the page enforces the role", async ({ page }) => {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/about");
  const users = page.locator("nav.sn-in a", { hasText: "Users" });
  await expect(users).toBeVisible();
  expect(await users.getAttribute("href")).toMatch(/\/users$/);
  await page.setExtraHTTPHeaders({ "X-authentik-username": "olivia", "X-authentik-groups": "ops" });
  await page.goto(app.url + "/devices");
  await expect(page.locator("nav.tb-groups a", { hasText: "System" })).toHaveCount(1);
  const r = await page.request.get(app.url + "/users", { headers: { "X-authentik-username": "olivia", "X-authentik-groups": "ops" } });
  expect(r.status()).toBe(403);
});

// Screenshots for the PR review.
for (const vp of viewports) {
  test(`screenshots: ${vp.name}`, async ({ page }) => {
    test.skip(!shots, "UX_SHOTS not set");
    mkdirSync(shots!, { recursive: true });
    await page.setViewportSize(vp);
    await page.setExtraHTTPHeaders(admin);
    for (const [name, path] of [["device", `/devices/${deviceID}`], ["about", "/about"], ["transactions", "/transactions"], ["dashboard", "/"], ["policy", "/policy"], ["firmware", "/firmware"]]) {
      await page.goto(app.url + path);
      await expectNoOverflow(page, name);
      await page.screenshot({ path: join(shots!, `${name}-${vp.name}.png`), fullPage: true });
    }
  });
}
