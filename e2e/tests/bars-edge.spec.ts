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

// Why: the page must not jump sideways between a short and a long page (v0.10.1). With the scrollbar
// always drawn, the content column has the same left edge and width on both, and when a short page
// grows (a swap adds rows) nothing moves.
for (const width of [1280, 1920]) {
  test(`the content column does not move between short and long pages (${width}px)`, { tag: "@core" }, async ({ page }) => {
    await page.setViewportSize({ width, height: 600 });
    const column = () =>
      page.evaluate(() => {
        const r = document.querySelector("header.top .shell-in")!.getBoundingClientRect();
        const m = document.querySelector("main")!.getBoundingClientRect();
        return { left: r.left, width: r.width, mainLeft: m.left, mainWidth: m.width, scrolls: document.documentElement.scrollHeight > window.innerHeight };
      });
    await page.goto(app.url + "/users");
    const short = await column();
    await page.goto(app.url + "/styleguide");
    const long = await column();
    console.log(`${width}: short`, JSON.stringify(short), "long", JSON.stringify(long));
    expect([short.scrolls, long.scrolls]).toEqual([false, true]);
    expect({ ...short, scrolls: 0 }).toEqual({ ...long, scrolls: 0 });
    await page.goto(app.url + "/users");
    await page.evaluate(() => {
      const d = document.createElement("div");
      d.style.height = "3000px";
      document.querySelector("main")!.appendChild(d);
    });
    const grown = await column();
    expect(grown.scrolls).toBe(true);
    expect({ ...grown, scrolls: 0 }).toEqual({ ...short, scrolls: 0 });
  });
}
