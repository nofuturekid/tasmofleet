import { test, expect } from "../support/fixtures";
import { realisticPolicyRules } from "../support/seed";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// The device overview (maintainer feedback, items 7d, 9; #145): sibling cards never touch (they
// keep the gap token) and the overview links to the complete Activity list.
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
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_POLICY_RULES: realisticPolicyRules, // a rule makes the fourth tile (Policy), which halves the Reachability tile on a phone
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

async function deviceHref(page: import("@playwright/test").Page, name: string): Promise<string> {
  await page.goto(app.url + "/devices");
  // the first health reports carry the relay state the power control needs
  await page.locator("table[data-table=devices] .pwr:not([disabled])").first().waitFor({ state: "attached", timeout: 30_000 });
  return (await page.getByRole("link", { name, exact: true }).first().getAttribute("href"))!;
}

for (const width of [1440, 1100, 800, 600, 375]) {
  test(`overview cards keep the gap token between siblings at ${width}px`, async ({ page }) => {
    await page.setExtraHTTPHeaders(adam);
    await page.setViewportSize({ width, height: 900 });
    const href = await deviceHref(page, "esp32-dimmer");
    await page.goto(app.url + href);
    await expect(page.locator(".devcard").first()).toBeVisible();
    const bad = await page.evaluate(() => {
      const gap = parseFloat(getComputedStyle(document.querySelector(".devlive")!).getPropertyValue("--card-gap")) || 12;
      const cards = [...document.querySelectorAll(".devcard")].filter((c) => c.getBoundingClientRect().height > 0);
      const name = (c: Element) => c.querySelector("h2")?.textContent ?? "?";
      const out: string[] = [];
      for (const a of cards) {
        const r = a.getBoundingClientRect();
        for (const b of cards) {
          if (a === b) continue;
          const q = b.getBoundingClientRect();
          const sharesColumn = Math.min(r.right, q.right) - Math.max(r.left, q.left) > 1;
          const sharesRow = Math.min(r.bottom, q.bottom) - Math.max(r.top, q.top) > 1;
          if (sharesColumn && sharesRow) out.push(`${name(a)} overlaps ${name(b)}`);
          else if (sharesColumn && q.top >= r.bottom - 0.5 && q.top - r.bottom < gap - 0.5) out.push(`${name(b)} is ${(q.top - r.bottom).toFixed(1)}px under ${name(a)} (gap ${gap})`);
          else if (sharesRow && q.left >= r.right - 0.5 && q.left - r.right < gap - 0.5) out.push(`${name(b)} is ${(q.left - r.right).toFixed(1)}px beside ${name(a)} (gap ${gap})`);
        }
      }
      return out;
    });
    expect(bad).toEqual([]);
  });
}

// #420: the Reachability value "MQTT · HTTP" ran past its half-width tile on a phone. Layout guard:
// no tile text may reach past the tile it sits in, at the narrowest phone and the widest. The second
// run puts the longest Reachability wordings (#416, #435) into the tile: it must wrap, not overflow.
const reachWordings: [string, string][] = [
  ["MQTT · HTTP", "Connected · Reachable"],
  ["No live link", "HTTP not checked lately, last answered 3d 4h ago · MQTT not configured"],
  ["MQTT", "Connected · HTTP not checked lately, last answered 3d 4h ago"],
  ["Login refused", "HTTP not reachable, last answered 12d 23h ago"],
];
for (const width of [320, 375, 430, 768, 1280]) {
  for (const [value, sub] of reachWordings) {
    test(`overview tile values stay inside their tile at ${width}px: ${value}`, async ({ page }) => {
      await page.setExtraHTTPHeaders(adam);
      await page.setViewportSize({ width, height: 900 });
      const href = await deviceHref(page, "esp32-dimmer");
      await page.goto(app.url + href);
      await expect(page.locator(".dev-tile").first()).toBeVisible();
      await expect(page.locator(".dev-tile")).toHaveCount(4); // the phone layout with a half-width Reachability tile
      // freeze the live refresh (it would put the real wording back) and show the wording under test
      await page.evaluate(([v, s]) => {
        document.querySelector("#devlive")?.removeAttribute("hx-trigger");
        document.querySelector("[data-tile=reach] .dev-tile-v")!.textContent = v;
        document.querySelector("[data-tile=reach] .dev-tile-s")!.textContent = s;
      }, [value, sub]);
      await page.evaluate(() => document.fonts.ready);
      const bad = await page.evaluate(() => {
        const out: string[] = [];
        for (const tile of document.querySelectorAll(".dev-tile")) {
          const t = tile.getBoundingClientRect();
          for (const el of tile.querySelectorAll(".dev-tile-l, .dev-tile-v, .dev-tile-s")) {
            const range = document.createRange();
            range.selectNodeContents(el);
            for (const r of range.getClientRects()) {
              if (r.right > t.right + 0.5 || r.left < t.left - 0.5) {
                out.push(`${tile.getAttribute("data-tile")} ${el.className} "${el.textContent}" reaches ${Math.round(r.right)} past the tile edge ${Math.round(t.right)}`);
                break;
              }
            }
          }
        }
        return out;
      });
      expect(bad).toEqual([]);
    });
  }
}

// Item 10 (#145): the overview shows a short Activity list and links to the complete, paged one,
// and to the device's transactions.
test("the device overview links to the complete activity list", async ({ page }) => {
  await page.setExtraHTTPHeaders(adam);
  const href = await deviceHref(page, "esp32-dimmer");
  await page.goto(app.url + href);
  await page.getByRole("link", { name: "All activity" }).click();
  await expect(page).toHaveURL(/\/devices\/\d+\/history$/);
  await expect(page.getByRole("heading", { name: "Activity" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Back to the overview" })).toBeVisible();
});
