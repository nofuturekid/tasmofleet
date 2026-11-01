import type { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import {
  App,
  Devenv,
  freePort,
  startApp,
  startDevenv,
  writeTasmoAdminDir,
} from "../support/stack";

// QA v0.9.0 follow-up, packages B, C and H: global CSS and shell, native controls, dialogs.
// Classic scrollbars are switched on (Playwright hides them by default): the QA findings only
// show with a reserved or visible scrollbar. Components that no seeded page shows in the needed
// state are mounted into a sandbox on a real page, so the app's own stylesheet decides.
test.use({ launchOptions: { ignoreDefaultArgs: ["--hide-scrollbars"] } });

let app: App;
let env: Devenv;
test.beforeAll(async () => {
  env = await startDevenv();
  const fwPort = await freePort();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_URL,
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

const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

async function open(
  page: Page,
  path: string,
  width = 1280,
  scheme: "light" | "dark" = "light",
): Promise<void> {
  await page.emulateMedia({ colorScheme: scheme });
  await page.setExtraHTTPHeaders(admin);
  await page.setViewportSize({ width, height: 800 });
  await page.goto(app.url + path);
  await page.evaluate(() => document.fonts.ready);
}

/** mount adds markup to the page (classes only: inline styles would trip the CSP); set sizes through the CSSOM. */
async function mount(page: Page, html: string): Promise<void> {
  await page.evaluate((h) => {
    document.getElementById("sandbox")?.remove();
    const box = document.createElement("div");
    box.id = "sandbox";
    box.innerHTML = h;
    document.querySelector("main")!.append(box);
  }, html);
}

/** contrast is the WCAG ratio of two CSS colours (any syntax the browser parses). */
async function contrast(page: Page, fg: string, bg: string): Promise<number> {
  return page.evaluate(
    ([a, b]) => {
      const c = document
        .createElement("canvas")
        .getContext("2d", { willReadFrequently: true })!;
      const rgb = (col: string) => {
        c.clearRect(0, 0, 1, 1);
        c.fillStyle = "#000";
        c.fillStyle = col;
        c.fillRect(0, 0, 1, 1);
        return [...c.getImageData(0, 0, 1, 1).data].slice(0, 3);
      };
      const lum = ([r, g, bl]: number[]) => {
        const f = (v: number) =>
          (v /= 255) <= 0.03928
            ? v / 12.92
            : Math.pow((v + 0.055) / 1.055, 2.4);
        return 0.2126 * f(r) + 0.7152 * f(g) + 0.0722 * f(bl);
      };
      const [l1, l2] = [lum(rgb(a)), lum(rgb(b))].sort((x, y) => y - x);
      return (l1 + 0.05) / (l2 + 0.05);
    },
    [fg, bg],
  );
}

// Why: the flash once sat at top:1rem, over the brand and the menu button. As a toast (design
// v2) it sits bottom right inside the window, clear of the top bar, and a success fades away.
for (const width of [1280, 375]) {
  test(`the flash is a toast bottom right at ${width}px`, async ({ page }) => {
    await open(page, "/devices?ok=restarted", width);
    const toast = page.locator("#toasts .toast");
    await expect(toast).toContainText("Restart sent.");
    await expect(page.locator("main [data-flash]")).toHaveCount(0);
    const m = await page.evaluate(() => {
      const t = document
        .querySelector("#toasts .toast")!
        .getBoundingClientRect();
      return {
        right: innerWidth - t.right,
        bottom: innerHeight - t.bottom,
        left: t.left,
        top: t.top,
        bar: document.querySelector("header.top")!.getBoundingClientRect()
          .bottom,
      };
    });
    expect(m.right).toBeGreaterThanOrEqual(15);
    expect(m.bottom).toBeGreaterThanOrEqual(15);
    expect(m.left).toBeGreaterThanOrEqual(0);
    expect(m.top).toBeGreaterThan(m.bar);
    await expect(toast).toHaveCount(0, { timeout: 7_000 });
  });
}

// Why: the drawer rows are list rows: every link and Sign out start at the row padding, and the
// group headings are small capitals (the prototype's drawer).
test("the phone drawer: rows start at the row padding, group headings upper-case", async ({
  page,
}) => {
  await open(page, "/", 375);
  await page.getByLabel("Open navigation", { exact: true }).click();
  const m = await page.evaluate(() => {
    const btn = document.querySelector<HTMLElement>(
      "#nav-drawer .dr-foot form button",
    )!;
    const r = document.createRange();
    r.selectNodeContents(btn);
    return {
      gap: r.getBoundingClientRect().left - btn.getBoundingClientRect().left,
      label: getComputedStyle(document.querySelector("#nav-drawer .dr-label")!)
        .textTransform,
    };
  });
  expect(
    m.gap,
    "Sign out starts at the row padding, like the links",
  ).toBeLessThan(24);
  expect(m.label).toBe("uppercase");
});

// Why: a font that kerns a capital with the following space makes "HTTP scan" read "HTTPscan" (Archivo did, so
// 0.9.0 switched kerning off; Geist does not, and the prototype keeps the default kerning). The space must keep its width.
test("a space after a capital keeps its width", async ({ page }) => {
  await open(page, "/about");
  await mount(
    page,
    `<span class="kt">HTTP scan</span><span class="kt">HTTPscan</span>`,
  );
  const d = await page.evaluate(() => {
    const [a, b] = [...document.querySelectorAll<HTMLElement>("#sandbox .kt")];
    for (const e of [a, b]) {
      e.style.fontSize = "100px";
      e.style.fontWeight = "700";
      e.style.whiteSpace = "pre";
      e.style.position = "absolute";
    }
    return a.getBoundingClientRect().width - b.getBoundingClientRect().width;
  });
  expect(d, "the space is at least 18% of the font size wide").toBeGreaterThan(
    18,
  );
});

// Why: dark-mode contrast of dashboard zero tiles (2.75:1 with the disabled-text token) and of the
// sort icons (about 2:1); both need 3:1 or better (4.5:1 for text).
for (const scheme of ["light", "dark"] as const) {
  test(`zero tiles and sort icons are readable (${scheme})`, async ({
    page,
  }) => {
    await open(page, "/about", 1280, scheme);
    await mount(
      page,
      `<div class="stat zero"><span class="num">0</span></div><table><thead><tr><th><a class="sortlink"><span class="sortmark"></span></a></th></tr></thead></table>`,
    );
    const m = await page.evaluate(() => {
      const bg = getComputedStyle(
        document.querySelector("#sandbox .stat")!,
      ).backgroundColor;
      const th = getComputedStyle(document.querySelector("#sandbox th")!);
      const mark = getComputedStyle(
        document.querySelector("#sandbox .sortmark")!,
      );
      return {
        num: getComputedStyle(document.querySelector("#sandbox .stat .num")!)
          .color,
        bg,
        thColor: th.color,
        thBg: th.backgroundColor,
        opacity: Number(mark.opacity),
        page: getComputedStyle(document.body).backgroundColor,
      };
    });
    const surface = m.bg === "rgba(0, 0, 0, 0)" ? m.page : m.bg;
    expect(await contrast(page, m.num, surface)).toBeGreaterThanOrEqual(4.5);
    // the icon is the header colour at `opacity` over the page: blend it, then measure
    const blended = await page.evaluate(
      ([fg, bgc, o]) => {
        const c = document
          .createElement("canvas")
          .getContext("2d", { willReadFrequently: true })!;
        const px = (col: string) => {
          c.clearRect(0, 0, 1, 1);
          c.fillStyle = col;
          c.fillRect(0, 0, 1, 1);
          return [...c.getImageData(0, 0, 1, 1).data];
        };
        const [f, b] = [px(fg as string), px(bgc as string)];
        return `rgb(${[0, 1, 2].map((i) => Math.round(f[i] * (o as number) + b[i] * (1 - (o as number)))).join(",")})`;
      },
      [m.thColor, m.page, m.opacity] as const,
    );
    expect(
      await contrast(page, blended, m.page),
      "sort icon",
    ).toBeGreaterThanOrEqual(3);
  });
}

// Why: the select-all box sat 3-5px left of the row boxes.
test("the select-all box is on the row boxes' vertical line", async ({
  page,
}) => {
  await open(page, "/devices");
  await page.waitForSelector("tbody td.sel input");
  const m = await page.evaluate(() => {
    const centre = (e: Element) => {
      const r = e.getBoundingClientRect();
      return r.left + r.width / 2;
    };
    return {
      all: centre(document.querySelector("thead [data-select-all]")!),
      row: centre(document.querySelector("tbody td.sel input")!),
    };
  });
  expect(Math.abs(m.all - m.row)).toBeLessThan(1);
});

// Why: the divider of a confirmation card overshot the card's border by 4px on phones (the card
// has 12px side padding there, the divider's negative margin was 16px). The v2 confirmation page
// (ui.ConfirmPage) keeps its buttons inside the card's padding at every width.
test("the confirmation foot stays inside the card", async ({ page }) => {
  await open(page, "/devices");
  const href = await page
    .locator("tbody a[href*='/devices/']")
    .first()
    .getAttribute("href");
  for (const width of [375, 1280]) {
    await page.setViewportSize({ width, height: 800 });
    await page.goto(app.url + href!.replace(app.url, "") + "/restart"); // the delete page is no ConfirmPage (N2)
    const m = await page.evaluate(() => {
      const card = document.querySelector("section.confirm-card")!;
      const c = card.getBoundingClientRect();
      const f = card.querySelector(".udlg-foot")!.getBoundingClientRect();
      const pad = parseFloat(getComputedStyle(card).paddingLeft);
      return { dl: f.left - c.left, dr: c.right - f.right, pad };
    });
    expect(m.dl, `left at ${width}px`).toBeGreaterThanOrEqual(m.pad - 0.5);
    expect(m.dr, `right at ${width}px`).toBeGreaterThanOrEqual(m.pad - 0.5);
    expect(
      m.dr,
      `right at ${width}px: the buttons end at the padding`,
    ).toBeLessThan(m.pad + 3);
  }
});

// Why: an action row right under a table touched the table's last row.
test("an action row below a table has a gap", async ({ page }) => {
  await open(page, "/about");
  await mount(
    page,
    `<section class="card"><table><tbody><tr><td>x</td></tr></tbody></table><p class="actions"><a class="button">Export</a></p></section>`,
  );
  const gap = await page.evaluate(
    () =>
      document.querySelector("#sandbox p.actions")!.getBoundingClientRect()
        .top -
      document.querySelector("#sandbox table")!.getBoundingClientRect().bottom,
  );
  expect(gap).toBeGreaterThanOrEqual(12);
});

// Why: a wrapping checkbox label centred its box on the whole block; it belongs on the first line.
test("a wrapped checkbox label keeps its box on the first line (phone)", async ({
  page,
}) => {
  await open(page, "/about", 375);
  await mount(
    page,
    `<label class="inline"><input type="checkbox"> <span class="long">I understand that this device is backed up first, then receives the configuration of the backup and restarts, and that the old credentials come back.</span></label>`,
  );
  const m = await page.evaluate(() => {
    const r = document.createRange();
    r.selectNodeContents(document.querySelector("#sandbox .long")!);
    const line = r.getClientRects()[0];
    const box = document
      .querySelector("#sandbox input")!
      .getBoundingClientRect();
    return {
      boxMid: box.top + box.height / 2,
      lineMid: line.top + line.height / 2,
      lines: r.getClientRects().length,
    };
  });
  expect(m.lines).toBeGreaterThan(2);
  expect(Math.abs(m.boxMid - m.lineMid)).toBeLessThan(4);
});

// Why: native progress was blue, rounded and 140px wide; job bars and chips were pills; job cards had a shadow.
// Design v2 rounds bars and chips with the small token radii (4px, 7px) but they stay no pills.
test("native controls are flat and full width", async ({ page }) => {
  await open(page, "/about");
  await mount(
    page,
    `<section class="card"><progress max="10" value="4"></progress><progress aria-label="x"></progress><div class="job"></div><div class="bar bad"><i class="s2"></i></div><label class="chip"><input type="checkbox"> name</label><input type="file"><input type="checkbox" aria-invalid="true"></section>`,
  );
  const m = await page.evaluate(() => {
    const q = (s: string) =>
      getComputedStyle(document.querySelector("#sandbox " + s)!);
    const card = document
      .querySelector("#sandbox .card")!
      .getBoundingClientRect();
    const pr = document
      .querySelector("#sandbox progress")!
      .getBoundingClientRect();
    return {
      appearance: q("progress").appearance,
      radius: q("progress").borderRadius,
      width: pr.width,
      cardWidth: card.width,
      job: q(".job").boxShadow,
      bar: q(".bar").borderRadius,
      barFill: q(".bar i").borderRadius,
      barBadW:
        document.querySelector("#sandbox .bar.bad i")!.getBoundingClientRect()
          .width /
        document.querySelector("#sandbox .bar.bad")!.getBoundingClientRect()
          .width,
      chip: q("label.chip").borderRadius,
      file: getComputedStyle(
        document.querySelector("#sandbox input[type=file]")!,
        "::file-selector-button",
      ).height,
      invalidShadow: q("input[aria-invalid]").boxShadow,
    };
  });
  expect(m.appearance).toBe("none");
  expect(m.radius, "a progress bar has the small radius").toBe("4px");
  expect(m.width, "progress fills its card").toBeGreaterThan(m.cardWidth * 0.8);
  expect(m.job).toBe("none");
  expect(m.bar, "job bars are not pills").toBe("4px");
  expect(m.barFill).toBe("4px");
  expect(
    m.barBadW,
    "a failed job's bar is full, not an empty pill",
  ).toBeGreaterThan(0.9);
  expect(m.chip, "chips are control-radius, not pills").toBe("7px");
  expect(m.file, "the file button is a button.sm (28px)").toBe("28px");
  expect(m.invalidShadow, "no red bar through an invalid checkbox").toBe(
    "none",
  );
});

// Why: a field the server refused must stand out by its red frame (the prototype's error state,
// add-manual "attic heater"); the base input rule's border once outranked it, so only the red
// sentence below showed which input was wrong.
test("a refused form field has the danger border", async ({ page }) => {
  await open(page, "/about");
  await mount(
    page,
    `<label class="ufield"><span class="ufield-label">Address</span><input name="a" aria-invalid="true"></label><label class="ufield"><input name="b"></label>`,
  );
  const m = await page.evaluate(() => {
    const probe = document.createElement("span");
    probe.style.color = "var(--color-danger)"; // CSSOM, not an attribute in markup: no CSP issue
    document.getElementById("sandbox")!.append(probe);
    return {
      danger: getComputedStyle(probe).color,
      invalid: getComputedStyle(
        document.querySelector("#sandbox input[name=a]")!,
      ).borderTopColor,
      plain: getComputedStyle(document.querySelector("#sandbox input[name=b]")!)
        .borderTopColor,
    };
  });
  expect(m.invalid, "the refused field is framed in the danger colour").toBe(
    m.danger,
  );
  expect(m.plain, "a valid field keeps its normal border").not.toBe(m.danger);
});

// Why: the health line wrapped inside "wifi 36%" on phones. Since design v2 (P3) the signal is in the
// Wi-Fi tile (#145): its readings stay on one line each on a phone.
test("the Wi-Fi readings never split over two lines on a phone", async ({
  page,
}) => {
  await open(page, "/devices", 375);
  const href = await page
    .locator("a[href*='/devices/']")
    .filter({ hasText: /\S/ })
    .first()
    .getAttribute("href");
  const wifi = page.locator("[data-tile=wifi]");
  // the first poll has to land before there is a signal to show
  await expect(async () => {
    await page.goto(app.url + href!.replace(app.url, ""));
    await expect(wifi.locator(".dev-tile-v", { hasText: /dBm/ })).toHaveCount(1, {
      timeout: 1000,
    });
  }).toPass({ timeout: 30_000 });
  const n = await wifi.locator(".dev-tile-v").evaluateAll((els) =>
    els.map((e) => {
      const r = document.createRange();
      r.selectNodeContents(e);
      return [
        e.textContent,
        new Set([...r.getClientRects()].map((x) => Math.round(x.top))).size,
      ];
    }),
  );
  for (const [text, lines] of n) expect(lines, `${text}`).toBe(1);
});

// Why: the row Actions menu stayed open behind the modal it had just opened.
test("opening a dialog from a row menu closes the menu", async ({ page }) => {
  await open(page, "/devices");
  await page.waitForSelector("tbody details.rowmenu summary");
  await page.locator("tbody details.rowmenu summary").first().click();
  await expect(page.locator("details.rowmenu[open]")).toHaveCount(1);
  await page.locator("details.rowmenu[open] a[data-dialog]").first().click();
  await expect(page.locator("dialog#dlg-row[open]")).toHaveCount(1);
  await expect(page.locator("details.rowmenu[open]")).toHaveCount(0);
  // the dismissing button says "Cancel" unless the opener names it
  await expect(page.locator("dialog#dlg-row [data-fill=dismiss]")).toHaveText(
    "Cancel",
  );
});

// Why: "Cancel" beside "Cancel transaction" is ambiguous; the opener can name the dismiss button, and
// the shared dialog resets it for the next opener.
test("the shared dialog takes its dismiss label from the opener and resets it", async ({
  page,
}) => {
  await open(page, "/devices");
  await page.waitForSelector("tbody details.rowmenu summary");
  await page.evaluate(() =>
    document
      .querySelector("tbody details.rowmenu a[data-dialog]")!
      .setAttribute("data-dlg-dismiss", "Keep running"),
  );
  await page.locator("tbody details.rowmenu summary").first().click();
  await page.locator("details.rowmenu[open] a[data-dialog]").first().click();
  await expect(page.locator("dialog#dlg-row [data-fill=dismiss]")).toHaveText(
    "Keep running",
  );
  await page.locator("dialog#dlg-row [data-fill=dismiss]").click();
  await page.locator("tbody details.rowmenu summary").nth(1).click();
  await page.locator("details.rowmenu[open] a[data-dialog]").first().click();
  await expect(page.locator("dialog#dlg-row [data-fill=dismiss]")).toHaveText(
    "Cancel",
  );
});

// Why (v0.10.1): clicking through the menu made the page jump sideways, between the main pages
// and between a device's tabs alike. Without a reserved scrollbar gutter (dropped in 0.10.0) a
// page with a scrollbar laid out 15 px narrower than one without, so the top bar's content and
// the page header moved on every switch between a short and a long page. The gutter is kept on
// every page; the top bar's items and the page's crumb and title must sit at the same place on
// all of them.
test("the top bar and the page header do not move between pages", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await open(page, "/devices", 1280);
  const dev = (await page
    .locator("table[data-table=devices] td.p1 a")
    .first()
    .getAttribute("href"))!;
  const paths = [
    "/",
    "/devices",
    "/firmware",
    "/updates/new",
    "/transactions",
    "/policy",
    "/devices/new",
    "/discovery/scan",
    "/settings/system/snapshots",
    "/audit",
    "/users",
    "/settings/system",
    "/about",
    "/styleguide",
  ];
  for (const tab of [
    "",
    "/settings",
    "/console",
    "/backups",
    "/replace",
    "/edit",
  ])
    paths.push(dev + tab);
  await page.setViewportSize({ width: 1280, height: 720 });
  const seen: {
    path: string;
    scrolls: boolean;
    top: number[];
    head: number[];
  }[] = [];
  for (const path of paths) {
    await page.goto(path.startsWith("http") ? path : app.url + path);
    await page.evaluate(() => document.fonts.ready);
    seen.push({
      path,
      ...(await page.evaluate(() => ({
        scrolls:
          document.documentElement.scrollHeight >
          document.documentElement.clientHeight,
        top: Array.from(
          document.querySelectorAll(
            ".tb-in > .brand, .tb-in > .tb-groups, .tb-in > details.usermenu",
          ),
          (c) => Math.round(c.getBoundingClientRect().left * 2) / 2,
        ),
        head: Array.from(
          document.querySelectorAll("main .crumbs, main .crumb, main h1"),
          (c) => Math.round(c.getBoundingClientRect().left * 2) / 2,
        ).slice(0, 2),
      }))),
    });
  }
  expect(
    seen.some((s) => s.scrolls),
    "some pages scroll",
  ).toBe(true);
  expect(
    seen.some((s) => !s.scrolls),
    "some pages do not scroll",
  ).toBe(true);
  for (const s of seen) {
    expect(s.top, `top bar items on ${s.path}`).toEqual(seen[0].top);
    expect(s.head[s.head.length - 1], `title on ${s.path}`).toBe(
      seen[0].head[seen[0].head.length - 1],
    );
    if (s.head.length > 1)
      expect(s.head[0], `crumb on ${s.path}`).toBe(s.head[1]);
  }
});

// Why (v0.10.1): the fonts were the only static files without the ?v= hash, so every
// navigation revalidated them (a 304 round trip) while the page painted in the fallback font
// first and swapped to the real one afterwards: the text flashed and reflowed on every page. The font
// URLs are versioned and cached immutable, so the next page has Geist before its first paint. Geist
// Mono (addresses, versions, ids) is preloaded the same way and must also come from the cache.
test("the next page has its font from the cache before it paints", async ({
  page,
}) => {
  await page.addInitScript(() => {
    document.addEventListener("DOMContentLoaded", () => {
      (window as unknown as { __fontAtDCL: boolean }).__fontAtDCL =
        document.fonts.check("16px Geist", "a");
    });
  });
  await open(page, "/about");
  await page.goto(app.url + "/devices");
  const m = await page.evaluate(() => ({
    atDCL: (window as unknown as { __fontAtDCL: boolean }).__fontAtDCL,
    fonts: performance
      .getEntriesByType("resource")
      .filter((e) => /geist(-mono)?-latin\.woff2/.test(e.name))
      .map((e) => ({
        name: e.name,
        bytes: (e as PerformanceResourceTiming).transferSize,
      })),
  }));
  expect(
    m.fonts.length,
    "the page loads Geist and Geist Mono",
  ).toBeGreaterThanOrEqual(2);
  for (const f of m.fonts) {
    expect(f.name, "versioned font URL").toMatch(/\?v=[0-9a-f]{8}$/);
    expect(f.bytes, `${f.name} comes from the cache, without a request`).toBe(
      0,
    );
  }
  expect(m.atDCL, "Geist is ready when the page is parsed").toBe(true);
});

// Why (design v2, F2 review): v1 was square everywhere, so a framed box that never declared a radius looked
// right. In v2 every frame takes a token radius (control, card, panel, banner, code, badge); a box that still
// has square corners next to rounded buttons and cards is a v1 leftover. Tables and their cells stay square.
test("every framed box is rounded (no v1 square corners left)", async ({
  page,
}) => {
  for (const [path, width] of [
    ["/styleguide", 1280],
    ["/devices", 1280],
    ["/audit", 1280],
    ["/about", 375],
  ] as const) {
    await open(page, path, width);
    const square = await page.evaluate(() => {
      const out = new Set<string>();
      for (const e of document.querySelectorAll<HTMLElement>("body *")) {
        const cs = getComputedStyle(e);
        if (
          !e.getBoundingClientRect().width ||
          ["TABLE", "THEAD", "TBODY", "TR", "TD", "TH", "HR"].includes(
            e.tagName,
          )
        )
          continue;
        const framed = ["Top", "Right", "Bottom", "Left"].every(
          (s) =>
            parseFloat(cs.getPropertyValue(`border-${s.toLowerCase()}-width`)) >
              0 &&
            cs.getPropertyValue(`border-${s.toLowerCase()}-style`) !== "none",
        );
        if (framed && cs.borderTopLeftRadius === "0px")
          out.add(`${e.tagName.toLowerCase()}.${[...e.classList].join(".")}`);
      }
      return [...out];
    });
    expect(
      square,
      `${path} at ${width}px: framed boxes with square corners`,
    ).toEqual([]);
  }
});
