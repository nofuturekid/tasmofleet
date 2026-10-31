import { test, expect } from "../support/fixtures";
import { PNG } from "pngjs";
import { App, startApp } from "../support/stack";

// #395: the top bar and the status bar must span the whole window. With classic (non-overlay)
// scrollbars the stable scrollbar gutter once left a strip right of both bars in the page colour. The scrollbars
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

// Why: the page keeps the scrollbar's room on every page (no sideways jump), so on a page that does not
// scroll the bars' own boxes end before the window edge (the maintainer measured 1686 of 1701). Their
// background and lines must continue into that strip: the pixel column next to the window edge looks
// like the one inside the bar on every bar row, and the extension never becomes a horizontal scrollbar.
for (const width of [1280, 1920]) {
  for (const [label, path] of [["a scrollable page", "/styleguide"], ["a short page", "/users"]] as const) {
    test(`both bars reach the window edge on ${label} (${width}px)`, { tag: "@core" }, async ({ page }) => {
      await page.setViewportSize({ width, height: 600 });
      await page.goto(app.url + path);
      await expect(page.locator("footer.sb")).toBeVisible();
      const m = await page.evaluate(() => {
        const r = (s: string) => document.querySelector(s)!.getBoundingClientRect();
        return { inner: window.innerWidth, top: r("header.top"), sn: r("header.top .sn"), sb: r("footer.sb"), scrolls: document.documentElement.scrollHeight > window.innerHeight };
      });
      expect(m.scrolls).toBe(path === "/styleguide");
      expect(m.top.left).toBe(0);
      expect(m.sb.left).toBe(0);
      expect(m.inner - m.top.right).toBeGreaterThan(0); // the strip is kept (no sideways jump)
      if (m.scrolls) return; // the scrollbar itself fills the strip
      const png = PNG.sync.read(await page.screenshot());
      const px = (x: number, y: number) => {
        const i = (Math.round(y) * png.width + x) * 4;
        return [...png.data.subarray(i, i + 4)].join(",");
      };
      const edge = m.inner - 3; // inside the strip, left of any scrollbar a screenshot omits
      const inside = m.inner - 60;
      // the fill only: the lines (1px) lie in the clipped strip and cannot be drawn there
      const rows: Record<string, number> = {
        "top bar": 20,
        "sub-navigation": m.sn.top + 20,
        "status bar": m.sb.top + 12,
      };
      for (const [name, y] of Object.entries(rows)) expect(px(edge, y), name).toBe(px(inside, y));
      // the extension is clipped, never scrollable
      expect(await page.evaluate(() => { window.scrollTo(300, 0); return window.scrollX; })).toBe(0);
      // and the page colour is still the page colour between the bars, only the strip is the bars' colour
      expect(px(5, 300)).not.toBe(px(edge, 300));
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
// gutter kept, the content column has the same left edge and width on both, and when a short page
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
