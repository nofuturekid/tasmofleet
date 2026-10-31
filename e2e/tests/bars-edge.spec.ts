import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// #395: the top bar and the status bar must span the whole window. With classic (non-overlay)
// scrollbars the stable scrollbar gutter once left an empty strip right of both bars. The scrollbars
// are drawn here (Playwright hides them by default), as on the maintainer's desktop.
let app: App;
test.beforeAll(async () => {
  app = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" },
  });
});
test.afterAll(async () => app?.stop());
test.use({
  extraHTTPHeaders: { "X-authentik-username": "adam", "X-authentik-groups": "adm" },
  launchOptions: { ignoreDefaultArgs: ["--hide-scrollbars"] },
});

for (const width of [1280, 1920]) {
  for (const [label, path] of [["a scrollable page", "/styleguide"], ["a short page", "/users"]] as const) {
    test(`both bars reach both window edges on ${label} (${width}px)`, { tag: "@core" }, async ({ page }) => {
      await page.setViewportSize({ width, height: 600 });
      await page.goto(app.url + path);
      await expect(page.locator("footer.sb")).toBeVisible();
      const m = await page.evaluate(() => {
        const r = (s: string) => document.querySelector(s)!.getBoundingClientRect();
        return {
          inner: window.innerWidth,
          client: document.documentElement.clientWidth,
          top: r("header.top .tb"),
          sb: r("footer.sb"),
          scrolls: document.documentElement.scrollHeight > window.innerHeight,
        };
      });
      expect(m.scrolls).toBe(path === "/styleguide");
      // Why: the strip right of the bars is a drawn scrollbar on every page. An empty reserved gutter
      // (scrollbar-gutter: stable on a short page) looked like the bars stopping early.
      expect(m.inner - m.client).toBeGreaterThan(0);
      for (const bar of [m.top, m.sb]) {
        expect(bar.left).toBe(0);
        expect(bar.right).toBe(m.client);
      }
    });
  }
}

// With overlay or hidden scrollbars (phones, macOS) there is no strip: the bars end at the window edge.
test.describe("without a scrollbar strip", () => {
  for (const width of [1280, 1920]) {
    test(`both bars end exactly at the window's inner width (${width}px)`, async ({ page }) => {
      await page.setViewportSize({ width, height: 600 });
      await page.goto(app.url + "/styleguide");
      await expect(page.locator("footer.sb")).toBeVisible();
      await page.evaluate(() => { document.documentElement.style.scrollbarWidth = "none"; }); // CSSOM, the CSP forbids <style>
      const m = await page.evaluate(() => ({
        inner: window.innerWidth,
        top: document.querySelector("header.top .tb")!.getBoundingClientRect(),
        sb: document.querySelector("footer.sb")!.getBoundingClientRect(),
      }));
      for (const bar of [m.top, m.sb]) {
        expect(bar.left).toBe(0);
        expect(bar.right).toBe(m.inner);
      }
    });
  }
});
