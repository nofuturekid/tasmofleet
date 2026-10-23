import { test, expect } from "../support/fixtures";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";

// The device overview (Thomas's preview feedback, items 7d, 7e, 9): sibling cards never touch
// (they keep the gap token), and the header's power control looks like its neighbours in every
// state and theme (the generic button[aria-pressed=true] rule once made it a light pill).
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
      const gap = parseFloat(getComputedStyle(document.querySelector(".devgrid")!).getPropertyValue("--card-gap")) || 12;
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

for (const scheme of ["dark", "light"] as const) {
  for (const [device, state] of [["esp32-dimmer", "on"], ["lamp-1m", "off"]] as const) {
    test(`header power control looks like its neighbours: ${state}, ${scheme} theme`, async ({ page }) => {
      await page.setExtraHTTPHeaders(adam);
      await page.emulateMedia({ colorScheme: scheme });
      await page.setViewportSize({ width: 1440, height: 900 });
      const href = await deviceHref(page, device);
      await page.goto(app.url + href);
      const power = page.locator(".devhd-pwr");
      await expect(power).toBeVisible();
      await expect(power).toHaveAttribute("aria-pressed", state === "on" ? "true" : "false");
      const look = (l: import("@playwright/test").Locator) => l.evaluate((e) => { const s = getComputedStyle(e); return { bg: s.backgroundColor, border: s.borderTopColor, color: s.color, h: e.getBoundingClientRect().height }; });
      const mine = await look(power);
      const neighbour = await look(page.getByRole("button", { name: "Back up" }));
      expect(mine).toEqual(neighbour);
    });
  }
}

// Item 10: the overview shows a short list and links to the complete ones.
test("the device overview links to the complete state history and to the device's transactions", async ({ page }) => {
  await page.setExtraHTTPHeaders(adam);
  const href = await deviceHref(page, "esp32-dimmer");
  await page.goto(app.url + href);
  await page.getByRole("link", { name: "All state changes" }).click();
  await expect(page).toHaveURL(/\/devices\/\d+\/history$/);
  await expect(page.getByRole("heading", { name: "State changes" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Back to the overview" })).toBeVisible();
  await page.goto(app.url + href);
  await page.getByRole("link", { name: "All transactions" }).click();
  await expect(page).toHaveURL(/\/transactions\?device=\d+$/);
  await expect(page.getByRole("heading", { level: 1 })).toContainText("Transactions of esp32-dimmer");
});
