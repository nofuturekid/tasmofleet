import { test, expect } from "../support/fixtures";
import { join } from "node:path";
import { App, Devenv, freePort, startApp, startDevenv, writeTasmoAdminDir } from "../support/stack";
import { realisticPolicyRules } from "../support/seed";

// #128, #126: every timestamp is a <time datetime data-ts> that time.js shows in the browser's
// locale and time zone, whatever zone the server runs in. The server's text is only the fallback.
let app: App;
let env: Devenv;
test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  const fwPort = await freePort(); // the transaction pages come with the firmware server
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_POLICY_RULES: realisticPolicyRules, // the policy page draws its live-check line only with a rule
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
      TZ: "UTC",
    },
  });
  app.cli(["import", "tasmoadmin", writeTasmoAdminDir(env.devices)]);
  await seedFiveMinutesAgo();
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});

const zones = [
  { timezoneId: "Asia/Tokyo", locale: "en-GB" },
  { timezoneId: "America/Los_Angeles", locale: "de-DE" },
];

// The expected text is computed in the page, with the page's own Intl (the browser under test),
// not in Node: ICU data differs between the two.
const expectedInPage = (iso: string, withTitle: boolean) => {
  const o: Intl.DateTimeFormatOptions = { year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit" };
  return withTitle
    ? new Intl.DateTimeFormat(undefined, { ...o, timeZoneName: "short" }).format(new Date(iso))
    : new Intl.DateTimeFormat(undefined, o).format(new Date(iso));
};

for (const z of zones) {
  test(`the audit log shows times and tooltips in the browser's zone ${z.timezoneId} (${z.locale})`, async ({ browser }) => {
    const ctx = await browser.newContext(z);
    const page = await ctx.newPage();
    await page.setExtraHTTPHeaders({ "X-authentik-username": "adam", "X-authentik-groups": "adm" });
    await page.goto(app.url + "/audit");
    const t = page.locator("time[data-ts]").first();
    await expect(t).toBeVisible();
    const iso = (await t.getAttribute("datetime"))!;
    const [text, title] = await page.evaluate(
      ([i, f]) => [new Function("return (" + f + ")")()(i, false), new Function("return (" + f + ")")()(i, true)],
      [iso, expectedInPage.toString()] as [string, string],
    );
    // the server runs in UTC: a text equal to the server's would mean the browser's zone was ignored
    await expect(t).toHaveText(text);
    // the tooltip sits on the <time> and follows the browser too, with the zone's name
    await expect(t).toHaveAttribute("title", title);
    await ctx.close();
  });
}

// An HTMX or SSE swap brings server text again; time.js must repaint it (htmx:afterSettle).
test("a swapped-in timestamp is repainted in the browser's zone", async ({ browser }) => {
  const ctx = await browser.newContext(zones[0]);
  const page = await ctx.newPage();
  await page.setExtraHTTPHeaders({ "X-authentik-username": "adam", "X-authentik-groups": "adm" });
  await page.goto(app.url + "/audit");
  await expect(page.locator("time[data-ts]").first()).toBeVisible();
  const iso = "2026-03-04T22:15:09Z";
  const got = await page.evaluate(async (i) => {
    const host = document.createElement("div");
    host.id = "swap-host";
    document.body.appendChild(host);
    // @ts-expect-error htmx is a page global
    window.htmx.swap(host, `<time datetime="${i}" data-ts="datetime-s">SERVER-TEXT</time>`, { swapStyle: "innerHTML" });
    await new Promise((r) => setTimeout(r, 300));
    return host.textContent;
  }, iso);
  const want = await page.evaluate(([i, f]) => new Function("return (" + f + ")")()(i, false), [iso, expectedInPage.toString()] as [string, string]);
  expect(got).not.toBe("SERVER-TEXT");
  expect(got).toBe(want);
  await ctx.close();
});

// #137: "Today"/"Yesterday" is decided in the browser's zone, not the server's. The server runs in
// UTC; the browser sits in Kiritimati (UTC+14, no DST), so the same instant is a different day or
// a different clock time there. Everything an admin reads as relative is a <time data-ts="day">.
const seededAt = Date.now() - 5 * 60_000; // the instant every seeded row below carries

async function seedFiveMinutesAgo(): Promise<void> {
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const device = (db.prepare("SELECT id FROM devices ORDER BY id LIMIT 1").get() as { id: number }).id;
    // two finished transactions: the dashboard shows the newest as a card and the older ones as rows
    for (const batch of [900, 901]) {
      db.prepare("INSERT INTO ota_batches (id, created_by, created_at) VALUES (?, 'adam', ?)").run(batch, seededAt);
      const job = db.prepare(
        `INSERT INTO ota_jobs (batch_id, device_id, kind, commands, state, stage, target_release, target_variant, created_by, created_at, updated_at)
         VALUES (?, ?, 'command', 'Restart 1', 'succeeded', 'final', '', '', 'adam', ?, ?)`,
      ).run(batch, device, seededAt, seededAt + 1000);
      db.prepare("INSERT INTO ota_job_events (job_id, at, from_state, to_state, event, detail) VALUES (?, ?, 'queued', 'preflight', 'start', '')").run(
        Number(job.lastInsertRowid),
        seededAt,
      );
    }
    db.prepare(
      `INSERT INTO discovery_proposals (mac, kind, via, reason, name, address, port, topic, full_topic, existing_id, dismissed, first_seen, seen_at)
       VALUES ('aa:bb:cc:00:00:37', 'new', 'mqtt', '', 'Kitchen', '172.31.13.41', 80, 'kitchen_plug', 'tasmota/%topic%/%prefix%/', 0, 0, ?, ?)`,
    ).run(seededAt, seededAt);
  } finally {
    db.close();
  }
}

const kiritimati = { timezoneId: "Pacific/Kiritimati", locale: "en-GB" };

/** pageAt opens a page in Kiritimati whose clock is fixed at `browserNow` (ms since the epoch). */
async function pageAt(browser: import("@playwright/test").Browser, browserNow: number) {
  const ctx = await browser.newContext(kiritimati);
  const page = await ctx.newPage();
  await page.setExtraHTTPHeaders({ "X-authentik-username": "adam", "X-authentik-groups": "adm" });
  await page.clock.setFixedTime(browserNow);
  return { ctx, page };
}

// 1 h after the next local midnight in Kiritimati: the seeded instant is "Yesterday" there, while
// the server (UTC, real now) writes it as "Today".
const nextLocalDay = () => {
  const local = new Date(seededAt + 14 * 3600_000);
  return Date.UTC(local.getUTCFullYear(), local.getUTCMonth(), local.getUTCDate() + 1) - 14 * 3600_000 + 3600_000;
};
const sameLocalDay = () => seededAt + 60_000;

/** wanted is the text of a day stamp, computed with the page's own Intl: "<Today|Yesterday> HH:MM" in the browser's zone. */
const wanted = (page: import("@playwright/test").Page, iso: string, day: string) =>
  page.evaluate(([i, d]) => d + " " + new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit" }).format(new Date(i)), [iso, day] as [string, string]);

const dayStamps: [string, string][] = [
  ["the dashboard's transactions", "/|.txn-when time"],
  ["the transaction history", "/transactions|.upd-hwho time"],
];

for (const [name, where] of dayStamps) {
  const [path, selector] = where.split("|");
  for (const [label, now, day] of [
    ["Yesterday when the browser's day has moved on", nextLocalDay, "Yesterday"],
    ["Today within the browser's day", sameLocalDay, "Today"],
  ] as const) {
    test(`${name}: ${label}, whatever day the server (UTC) thinks it is`, async ({ browser }) => {
      const { ctx, page } = await pageAt(browser, now());
      await page.goto(app.url + path);
      const t = page.locator(selector).first();
      await expect(t).toBeVisible();
      await expect(t).toHaveText(await wanted(page, (await t.getAttribute("datetime"))!, day));
      await ctx.close();
    });
  }
}

// #422: the Stability card's recent restarts list spans days. Today's restart is the browser's clock
// time; an older one carries its date, or it reads like a restart from today.
test("Stability's recent restarts: the clock for today, the date for an older day", async ({ browser }) => {
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  let id: number;
  try {
    db.exec("PRAGMA busy_timeout=5000");
    id = (db.prepare("SELECT id FROM devices ORDER BY id LIMIT 1").get() as { id: number }).id;
    const ins = db.prepare("INSERT INTO device_boots (device_id, at, boot_count, reason) VALUES (?, ?, 1, 'Power On')");
    ins.run(id, seededAt);
    ins.run(id, seededAt - 3 * 24 * 3600_000);
  } finally {
    db.close();
  }
  const { ctx, page } = await pageAt(browser, sameLocalDay());
  await page.goto(app.url + "/devices/" + id);
  const times = page.locator("#stability .stab-boots .when time");
  await expect(times).toHaveCount(2);
  const [today, older] = await times.evaluateAll((els) =>
    els.map((e) => {
      const d = new Date(e.getAttribute("datetime")!);
      return [e.textContent, new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit" }).format(d), new Intl.DateTimeFormat(undefined, { day: "numeric", month: "short", hour: "2-digit", minute: "2-digit" }).format(d)];
    }),
  );
  expect(today[0], "today's restart is only the clock time").toBe(today[1]);
  expect(older[0], "an older restart carries its date").toBe(older[2]);
  expect(older[0]).not.toBe(older[1]);
  await ctx.close();
});

test("the transaction log's time is the browser's 24-hour clock", async ({ browser }) => {
  const { ctx, page } = await pageAt(browser, sameLocalDay());
  await page.goto(app.url + "/transactions/900");
  const t = page.locator("#log-lines .upd-log-at time").first();
  await expect(t).toBeVisible();
  const want = await page.evaluate(
    (i) => new Intl.DateTimeFormat(undefined, { hourCycle: "h23", hour: "2-digit", minute: "2-digit", second: "2-digit" }).format(new Date(i)),
    (await t.getAttribute("datetime"))!,
  );
  await expect(t).toHaveText(want);
  await ctx.close();
});

test("the policy page's pause time is the browser's clock", async ({ browser }) => {
  const ctx = await browser.newContext(kiritimati);
  const page = await ctx.newPage();
  await page.setExtraHTTPHeaders({ "X-authentik-username": "adam", "X-authentik-groups": "adm" });
  await page.goto(app.url + "/policy");
  await page.getByRole("button", { name: "Pause", exact: true }).click();
  const line = page.locator("#live-tile .dev-tile-s"); // the live-check tile says who paused and when
  await expect(line).toContainText("Live check paused by");
  const t = line.locator("time").first();
  const want = await page.evaluate(
    (i) => new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit" }).format(new Date(i)),
    (await t.getAttribute("datetime"))!,
  );
  await expect(t).toHaveText(want);
  // the server (UTC) wrote its own fallback; Kiritimati is 14 h away from it, so the zone is what the text proves
  const utc = await t.evaluate((el) => new Date(el.getAttribute("datetime")!).toISOString().slice(11, 16));
  expect(want.startsWith(utc), "the text must not be the server's UTC clock").toBe(false);
  expect(await t.evaluate((el) => (el as HTMLElement).dataset.ts)).toBe("time");
  await page.getByRole("button", { name: "Resume", exact: true }).click();
  await ctx.close();
});

// A page that stays open across the browser's midnight repaints "Today" as "Yesterday" by itself.
test("a day stamp turns from Today into Yesterday when the browser's midnight passes", async ({ browser }) => {
  const ctx = await browser.newContext(kiritimati);
  const page = await ctx.newPage();
  await page.setExtraHTTPHeaders({ "X-authentik-username": "adam", "X-authentik-groups": "adm" });
  // 3 s before the next local midnight; the page's timers are real, the clock is the test's
  const midnight = nextLocalDay() - 3600_000;
  await page.clock.install({ time: midnight - 3_000 });
  await page.goto(app.url + "/transactions");
  const t = page.locator(".upd-hwho time").first();
  await expect(t).toHaveText(/^Today /);
  await page.clock.runFor(5_000);
  await expect(t).toHaveText(/^Yesterday /);
  await ctx.close();
});
