import {
  test,
  expect,
  type APIRequestContext,
  type Page,
} from "@playwright/test";
import {
  copyFileSync,
  mkdirSync,
  rmSync,
  writeFileSync,
} from "node:fs";
import { dirname, join, posix } from "node:path";
import { root } from "../support/stack";
import { isMedia, pageAdmin } from "../support/seed";
import { pickForUpdate, startUpdate, txnState } from "../support/updates";
import { startVisualStack, type VisualStack } from "../support/visual-stack";

// make demo / make demo-media: the static demo published on GitHub Pages and the README media
// (docs/media). Not part of CI's e2e: it renders the design fixture fleet (60 simulated devices),
// saves every page an admin can reach with plain links as static HTML and takes the screenshots and
// the animation of the README. Only simulator and fixture values reach the output; the address
// rewrite below replaces the loopback ports and private fixture ranges with documentation ranges.

const out = process.env.DEMO_OUT ?? join(root, "dist/demo");
const media = join(root, "docs/media");
const demoAssets = join(root, "tools/demo");
const maxPages = 450;

/** Device pages with all their tabs; every other device has its overview page only. */
const deepDevices = ["d1", "d2", "d3", "d4", "d5", "d6", "d8", "d22", "d31"];

/** Query keys whose pages are distinct screens worth saving (tabs, views); other queries (sorting, filters) are not crawled. */
const keptQueryKeys = new Set([
  "tab",
  "view",
  "all",
  "ignored",
  "release",
  "new",
]);

/** Paths never crawled: actions, streams, files, the sign-out. */
const skipped =
  /^\/(static|events|metrics|healthz|readyz|prefs|logout|auth|login|setup|nav)(\/|$)|\/(download|export|events|stream|raw|dmp|json)(\/|$)|\.(dmp|json|bin|gz|csv|txt)$/;

test.describe.configure({ mode: "parallel" });

test("static demo site", async ({ browser }) => {
  test.setTimeout(30 * 60_000);
  const stack = await startVisualStack("fleet", browser);
  try {
    const context = await browser.newContext({
      extraHTTPHeaders: pageAdmin,
      viewport: { width: 1280, height: 860 },
    });
    const page = await context.newPage();
    await setTheme(page, stack, "light");
    const deep = new Set(
      deepDevices
        .map((d) => stack.placeholders.get(d))
        .filter(Boolean) as string[],
    );
    await crawl(context.request, stack, deep);
    if (process.env.DEMO_MEDIA) await screenshots(page, stack);
    await context.close();
  } finally {
    await stack.stop();
  }
});

test("README animation", async ({ browser }) => {
  test.setTimeout(20 * 60_000);
  const stack = await startVisualStack("fleet", browser, true);
  const videoDir = join(root, "e2e/test-results/demo-video");
  rmSync(videoDir, { recursive: true, force: true });
  try {
    const setup = await browser.newContext({ extraHTTPHeaders: pageAdmin });
    await setTheme(await setup.newPage(), stack, "light");
    await setup.close();
    const context = await browser.newContext({
      extraHTTPHeaders: pageAdmin,
      viewport: { width: 1280, height: 760 },
      recordVideo: { dir: videoDir, size: { width: 1280, height: 760 } },
    });
    // pages and htmx answers get the static site's address rewrite (not the SSE stream, which never ends)
    const addr = addressRewriter();
    await context.route(
      (u) => u.origin === new URL(stack.app.url).origin && !u.pathname.startsWith("/static/") && !u.pathname.startsWith("/events"),
      async (r) => {
        const res = await r.fetch({ maxRedirects: 0 });
        const type = res.headers()["content-type"] ?? "";
        if (!type.startsWith("text/html")) return r.fulfill({ response: res });
        return r.fulfill({ response: res, body: addr(await res.text()) });
      },
    );
    const page = await context.newPage();
    const started = Date.now();
    await page.goto(stack.app.url + "/");
    await page.waitForTimeout(2500);
    // the dashboard's way to the new update: the Updates entry of the navigation
    await page
      .getByRole("navigation")
      .getByRole("link", { name: /^Updates/ })
      .first()
      .click();
    await page.waitForTimeout(800);
    await pickForUpdate(page, stack.app.url, []);
    for (const n of ["Attic Dimmer", "Attic Fan", "Basement LED Strip", "Basement Switch"]) {
      await page.getByLabel(`Select ${n}`, { exact: true }).check();
      await page.waitForTimeout(450);
    }
    await page.waitForTimeout(800);
    await page.getByRole("button", { name: "Review update" }).click();
    await page.waitForTimeout(2000);
    await page
      .getByRole("button", { name: "Start update", exact: true })
      .click();
    await expect(page).toHaveURL(/\/transactions\/\d+$/);
    await expect(txnState(page)).toHaveText("Completed", { timeout: 300_000 });
    await page.waitForTimeout(3000);
    const seconds = (Date.now() - started) / 1000;
    const video = page.video();
    await context.close();
    const webm = await video!.path();
    console.log(`demo video: ${seconds.toFixed(0)} s, ${webm}`);
    copyFileSync(webm, join(root, "e2e/test-results/demo.webm"));
  } finally {
    await stack.stop();
  }
});

/** setTheme stores the admin's theme as the user menu does (POST /prefs/theme). */
async function setTheme(
  page: Page,
  stack: VisualStack,
  theme: "light" | "dark",
): Promise<void> {
  await page.goto(stack.app.url + "/");
  const token = await page.evaluate(
    () =>
      JSON.parse(document.body.getAttribute("hx-headers") ?? "{}")[
        "X-CSRF-Token"
      ] ?? "",
  );
  const r = await page.request.post(stack.app.url + "/prefs/theme", {
    data: JSON.stringify(theme),
    headers: { "Content-Type": "application/json", "X-CSRF-Token": token },
  });
  expect(r.ok(), `theme ${theme}: ${r.status()}`).toBe(true);
}

/**
 * screenshots takes the README's pictures from the static site just written, so they carry its
 * documentation addresses instead of the simulator's loopback ports. The demo banner is hidden.
 */
async function screenshots(page: Page, stack: VisualStack): Promise<void> {
  mkdirSync(media, { recursive: true });
  await page.route("http://demo.invalid/**", (r) => {
    const path = decodeURIComponent(new URL(r.request().url()).pathname);
    return r.fulfill({ path: join(out, path.endsWith("/") ? path + "index.html" : path) });
  });
  const shot = async (route: string, file: string, theme = "light") => {
    await page.goto("http://demo.invalid/" + fileOf(new URL("http://demo.invalid" + route)));
    await page.addStyleTag({ content: ".demo-banner{display:none}" });
    await page.evaluate((t) => document.documentElement.setAttribute("data-theme", t), theme);
    await page.waitForTimeout(500);
    await page.screenshot({ path: join(media, file) });
  };
  const ph = (k: string) => stack.placeholders.get(k) ?? "";
  await shot("/", "dashboard.png", "dark");
  await shot("/devices", "devices.png");
  await liveShots(page, stack, ph("d2"));
}

/**
 * liveShots shoots the two pages that need the running stack: the device Settings tab (read from the
 * simulator by htmx after the page loads) and the rule editor with the catalogue's suggestions open.
 * The address rewrite is the static site's.
 */
async function liveShots(page: Page, stack: VisualStack, d2: string): Promise<void> {
  const addr = addressRewriter();
  await page.unroute("http://demo.invalid/**");
  await page.route(
    (u) => u.origin === new URL(stack.app.url).origin && !u.pathname.startsWith("/events"),
    async (r) => {
      const res = await r.fetch({ maxRedirects: 0 });
      if (!(res.headers()["content-type"] ?? "").startsWith("text/html")) return r.fulfill({ response: res });
      return r.fulfill({ response: res, body: addr(await res.text()) });
    },
  );
  const theme = (t: string) => page.evaluate((v) => document.documentElement.setAttribute("data-theme", v), t);
  await page.goto(stack.app.url + `/devices/${d2}/settings`);
  await expect(page.getByText("Reading the settings from the device")).toBeHidden({ timeout: 60_000 });
  await theme("dark");
  await page.waitForTimeout(800);
  await page.screenshot({ path: join(media, "device-settings.png") });
  await page.goto(stack.app.url + "/policy/rules/new?command=Timezone");
  await page.locator("input[name='name']").fill("Home Assistant discovery");
  const cmd = page.locator("#rule-command");
  await cmd.fill("");
  await cmd.dispatchEvent("change");
  await cmd.pressSequentially("SetOpt");
  await expect(page.locator("#cmd-sug")).toBeVisible();
  await theme("light");
  await page.waitForTimeout(500);
  await page.screenshot({ path: join(media, "policy-editor.png") });
}

// ---------------------------------------------------------------------------
// Crawler
// ---------------------------------------------------------------------------

/** fileOf maps an app URL (path and query) to the file of the static site. */
function fileOf(u: URL): string {
  const path = u.pathname.replace(/^\/+|\/+$/g, "");
  const q = [...u.searchParams.entries()]
    .map(([k, v]) => `${k}-${v}`)
    .join("-")
    .replace(/[^A-Za-z0-9._-]/g, "_");
  const dir = path + (q ? (path ? "/" : "") + "_" + q : "");
  return dir ? `${dir}/index.html` : "index.html";
}

/** keep normalises a link to a crawlable URL key, or returns "" for a link the static site cannot serve. */
function keep(u: URL, origin: string, deep: Set<string>): string {
  if (u.origin !== origin || skipped.test(u.pathname)) return "";
  for (const k of u.searchParams.keys()) if (!keptQueryKeys.has(k)) return "";
  const dev = /^\/devices\/(\d+)\/./.exec(u.pathname);
  if (dev && !deep.has(dev[1])) return "";
  u.hash = "";
  return u.pathname + u.search;
}

async function crawl(
  request: APIRequestContext,
  stack: VisualStack,
  deep: Set<string>,
): Promise<void> {
  const base = new URL(stack.app.url);
  const ph = (s: string) =>
    s.replace(/\{(\w+)\}/g, (_, k) => stack.placeholders.get(k) ?? "");
  const seeds = [
    "/",
    "/devices",
    "/devices/new",
    "/updates/new",
    "/transactions",
    "/transactions/{txn}",
    "/firmware",
    "/policy",
    "/policy/io",
    "/policy/rules/new",
    "/policy/rules/r1",
    "/discovery",
    "/discovery?ignored=1",
    "/discovery/scan",
    "/import/tasmoadmin",
    "/audit",
    "/users",
    "/users/new",
    "/about",
    "/whats-new",
    "/styleguide",
    "/account",
    "/devices/{d2}",
    "/devices/{d2}/backups",
    "/devices/{d2}/backups/diff",
    "/devices/{d2}/backups/diff?all=1",
    "/devices/{d2}/settings",
    "/devices/{d2}/console",
    "/devices/{d2}/drift",
    "/backups/{backup}/restore",
    ...[
      "general",
      "network",
      "mqtt",
      "scan",
      "backups",
      "firmware",
      "signin",
      "advanced",
    ].map((t) => `/settings/system?tab=${t}`),
  ].map(ph);
  const pages = new Map<string, string>(); // URL key -> raw HTML
  const redirects = new Map<string, string>();
  const queue = [...seeds];
  const seen = new Set(queue);
  while (queue.length && pages.size < maxPages) {
    const key = queue.shift()!;
    const r = await request.get(base.origin + key, { maxRedirects: 0 });
    if (r.status() >= 300 && r.status() < 400) {
      const to = new URL(r.headers()["location"] ?? "/", base.origin + key);
      const k = keep(to, base.origin, deep);
      if (k) {
        redirects.set(key, k);
        if (!seen.has(k)) (seen.add(k), queue.push(k));
      }
      continue;
    }
    if (
      r.status() !== 200 ||
      !(r.headers()["content-type"] ?? "").startsWith("text/html")
    ) {
      console.log(
        `demo: skipped ${key} (${r.status()} ${r.headers()["content-type"]})`,
      );
      continue;
    }
    const html = await r.text();
    pages.set(key, html);
    for (const m of html.matchAll(/\bhref="([^"]+)"/g)) {
      const u = new URL(unescape(m[1]), base.origin + key);
      const k = keep(u, base.origin, deep);
      if (k && !seen.has(k)) (seen.add(k), queue.push(k));
    }
  }
  if (queue.length)
    console.log(`demo: page budget reached, ${queue.length} links left out`);
  expect(pages.size, "pages crawled").toBeGreaterThan(60);

  rmSync(out, { recursive: true, force: true });
  mkdirSync(join(out, "static"), { recursive: true });
  const addr = addressRewriter();
  const assets = new Set<string>();
  const target = (key: string) => redirects.get(key) ?? key;
  for (const [key, html] of pages) {
    const file = fileOf(new URL(base.origin + key));
    const rel = (to: string) => posix.relative(posix.dirname(file), to) || to;
    let s = html.replace(
      /\b(href|src|action)="([^"]*)"/g,
      (all, attr: string, value: string) => {
        const raw = unescape(value);
        if (/^(#|data:|mailto:|javascript:)/.test(raw) || raw === "")
          return all;
        const u = new URL(raw, base.origin + key);
        if (u.origin !== base.origin) return all; // outside links (GitHub, Tasmota docs) stay
        if (u.pathname.startsWith("/static/")) {
          const name = u.pathname.slice("/static/".length);
          assets.add(name);
          return `${attr}="${rel("static/" + name)}"`;
        }
        if (attr === "action") return `action="#"`;
        const k = keep(new URL(u), base.origin, deep);
        if (k && pages.has(target(k)))
          return `${attr}="${rel(fileOf(new URL(base.origin + target(k))))}${u.hash}"`;
        return `${attr}="#" data-demo-off`;
      },
    );
    s = neutralise(s);
    s = s.replace(
      /<\/head>/,
      `<link rel="stylesheet" href="${rel("demo/demo.css")}">\n<script src="${rel("demo/demo.js")}" defer></script>\n</head>`,
    );
    s = s.replace(
      /(<body\b[^>]*>)/,
      `$1\n<div class="demo-banner" role="note">Static demo &mdash; actions are disabled. <a href="https://github.com/nofuturekid/tasmofleet">Source and install</a></div>`,
    );
    // a device's own pages show its bare IP (the simulator's Status 5) next to host:port
    s = addr(s, /^\/devices\/\d+/.test(key) ? /127\.0\.0\.1:(\d{2,5})/.exec(html)?.[1] : undefined);
    mkdirSync(dirname(join(out, file)), { recursive: true });
    writeFileSync(join(out, file), s);
  }
  // the stylesheet's fonts and the manifest's icons are not in the pages
  for (const f of [
    "geist-latin.woff2",
    "geist-latin-ext.woff2",
    "geist-mono-latin.woff2",
    "geist-mono-latin-ext.woff2",
    "icon-192.png",
    "icon-512.png",
    "favicon.ico",
  ])
    assets.add(f);
  for (const name of assets) {
    const r = await request.get(`${base.origin}/static/${name}`);
    expect(r.status(), `asset ${name}`).toBe(200);
    let body = await r.body();
    if (name.endsWith(".css"))
      body = Buffer.from(body.toString("utf8").replace(/\?v=[0-9a-f]{8}/g, ""));
    writeFileSync(join(out, "static", name), body);
  }
  mkdirSync(join(out, "demo"), { recursive: true });
  for (const f of ["demo.js", "demo.css"])
    copyFileSync(join(demoAssets, f), join(out, "demo", f));
  writeFileSync(join(out, ".nojekyll"), "");
  console.log(
    `demo: ${pages.size} pages, ${assets.size} assets, ${redirects.size} redirects -> ${out}`,
  );
}

function unescape(s: string): string {
  return s
    .replace(/&amp;/g, "&")
    .replace(/&#34;|&quot;/g, '"')
    .replace(/&#39;/g, "'");
}

/** neutralise removes what needs the server: htmx requests, the SSE stream, the CSRF token, the theme's save URL. */
function neutralise(html: string): string {
  return html
    .replace(
      /\s(hx-(get|post|put|patch|delete|trigger|target|swap|select|sync|include|vals|indicator|confirm|params|push-url)|sse-connect|sse-swap|data-prefs)="[^"]*"/g,
      "",
    )
    .replace(/\shx-headers="[^"]*"/g, "")
    .replace(/\shx-ext="sse"/g, "")
    .replace(/(<input[^>]*name="csrf"[^>]*value=")[^"]*"/g, '$1"');
}

/**
 * addressRewriter replaces the simulator's loopback addresses (127.0.0.1:<port>, one per device and
 * service), the fixtures' private 192.168.20.x and 172.31.x addresses and *.lab.home names with documentation
 * ranges (RFC 5737) and example.internal,
 * the same address for the same input on every page. A bare 127.0.0.1 is the device's own address on its
 * pages and 192.0.2.1 elsewhere; loopback CIDRs (trusted proxies) stay.
 */
function addressRewriter(): (s: string, ownPort?: string) => string {
  const loop = new Map<string, string>();
  const ip = (port: string) => {
    if (!loop.has(port)) loop.set(port, `${isMedia ? "192.168.1" : "192.0.2"}.${10 + loop.size}`);
    return loop.get(port)!;
  };
  // The README pictures (DEMO_MEDIA) show one ordinary home network (192.168.1.x, home.lan) instead of the
  // documentation ranges; short addresses keep the device list's Address column from truncating. A bare
  // 127.0.0.1 away from a device's own pages is a device's former address (Address changed on the
  // dashboard), so each one gets an address of its own.
  let bare = 0;
  const other = () => (isMedia ? `192.168.1.${150 + bare++}` : "192.0.2.1");
  return (s, ownPort) =>
    s
      .replace(/127\.0\.0\.1:(\d{2,5})/g, (_, port: string) => ip(port))
      .replace(/127\.0\.0\.1(?![/\d])/g, () => (ownPort ? ip(ownPort) : other()))
      .replace(/\b192\.168\.20\.(\d{1,3})\b/g, isMedia ? "192.168.1.$1" : "198.51.100.$1")
      // the device captures (testdata/fixtures) and the app's placeholders use 172.31.x and lab.home
      .replace(/\b172\.31\.\d{1,3}\.(\d{1,3})\b/g, isMedia ? "192.168.1.$1" : "203.0.113.$1")
      .replace(/\b([a-z0-9-]+)\.(<wbr>)?lab\.(<wbr>)?home\b/g, isMedia ? "$1.$2home.$3lan" : "$1.$2example.$3internal") // long values carry <wbr> after dots
      .replace(/\/tmp\/tasmofleet-[A-Za-z0-9_-]+/g, "/data");
}
