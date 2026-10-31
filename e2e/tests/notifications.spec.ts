import { mkdirSync } from "node:fs";
import { createServer, IncomingMessage, Server } from "node:http";
import { createServer as createTCP, Server as TCPServer, Socket } from "node:net";
import { join } from "node:path";
import { Page } from "@playwright/test";
import { test, expect } from "../support/fixtures";
import { App, freePort, startApp } from "../support/stack";

// Settings, Notifications tab (issue #249): events, a webhook, e-mail, a message preview, a status
// line and a "Send test" button per channel. The receivers here are local listeners; nothing leaves
// the machine. UI249_DIR makes the spec also write the screenshots that are compared with the mockup.
test.describe.configure({ mode: "serial" });

let app: App;
let hook: { server: Server; port: number; status: number; seen: { body: string; headers: IncomingMessage["headers"] }[] };
let smtp: { server: TCPServer; port: number; data: string[] };
const secret = "e2e-fake-hook-secret-7391";
const mailPass = "e2e-fake-mail-pass-4417";

test.beforeAll(async () => {
  app = await startApp({
    env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm", TASMOFLEET_PUBLIC_URL: "http://tasmofleet.local" },
  });
  const hookPort = await freePort();
  const seen: typeof hook.seen = [];
  hook = { port: hookPort, status: 200, seen, server: createServer() };
  hook.server.on("request", (req, res) => {
    let body = "";
    req.on("data", (c) => (body += c));
    req.on("end", () => {
      seen.push({ body, headers: req.headers });
      res.statusCode = hook.status;
      res.end();
    });
  });
  await new Promise<void>((ok) => hook.server.listen(hookPort, "127.0.0.1", ok));
  // The smallest SMTP server that accepts one message: no TLS, no login offered.
  const smtpPort = await freePort();
  smtp = { port: smtpPort, data: [], server: createTCP() };
  smtp.server.on("connection", (s: Socket) => {
    let inData = false;
    let msg = "";
    s.write("220 fake\r\n");
    s.on("data", (chunk) => {
      for (const line of chunk.toString().split("\r\n")) {
        if (inData) {
          if (line === ".") {
            inData = false;
            smtp.data.push(msg);
            s.write("250 queued\r\n");
          } else msg += line + "\n";
        } else if (/^EHLO/i.test(line)) s.write("250 fake\r\n");
        else if (/^(MAIL|RCPT)/i.test(line)) s.write("250 ok\r\n");
        else if (/^DATA/i.test(line)) {
          inData = true;
          msg = "";
          s.write("354 go\r\n");
        } else if (/^QUIT/i.test(line)) s.end("221 bye\r\n");
      }
    });
  });
  await new Promise<void>((ok) => smtp.server.listen(smtpPort, "127.0.0.1", ok));
});
test.afterAll(async () => {
  hook?.server.close();
  smtp?.server.close();
  await app?.stop();
});
// sendTest presses "Send test" of a card; the server allows one test per channel every 3 s.
const lastTest: Record<string, number> = {};
async function sendTest(page: Page, card: string) {
  const wait = (lastTest[card] ?? 0) + 3200 - Date.now();
  if (wait > 0) await page.waitForTimeout(wait);
  lastTest[card] = Date.now();
  await page.locator(card).getByRole("button", { name: "Send test" }).click();
}
const admin = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };

async function open(page: Page) {
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=notifications");
  await expect(page.locator("#card-notify-webhook")).toBeVisible();
}

// shoot writes the screenshots that go next to the mockup's, when UI249_DIR is set.
async function shoot(page: Page, state: string) {
  const dir = process.env.UI249_DIR;
  if (!dir) return;
  mkdirSync(dir, { recursive: true });
  for (const [w, h] of [[1280, 900], [375, 812]] as const)
    for (const scheme of ["light", "dark"] as const) {
      await page.setViewportSize({ width: w, height: h });
      await page.emulateMedia({ colorScheme: scheme, reducedMotion: "reduce" });
      // as tall as the page, so the fixed save bar and status bar sit at the bottom like in the mockup shots
      const tall = await page.evaluate(() => document.documentElement.scrollHeight);
      await page.setViewportSize({ width: w, height: Math.max(h, tall) });
      await page.waitForTimeout(200);
      await page.screenshot({ path: join(dir, `${state}-${w}-${scheme}.png`) });
    }
  await page.setViewportSize({ width: 1280, height: 900 });
}

// Why (PO, #249): the cards stack directly under each other per column, no row-aligned gap under
// the short Events card next to the tall Webhook card. On a phone it is one column.
test("the cards stack per column without gaps; one column on a phone", { tag: "@core" }, async ({ page }) => {
  await open(page);
  await expect(page.locator("#sec-notifications .cstacks .scard h2")).toHaveText(["Events", "Message", "Webhook", "E-mail (SMTP)"]);
  const box = async (id: string) => (await page.locator(id).boundingBox())!;
  const events = await box("#card-notify-events"), message = await box("#card-notify-message");
  const webhook = await box("#card-notify-webhook"), email = await box("#card-notify-email");
  expect(message.y - (events.y + events.height), "Message sits right under Events").toBeLessThan(24);
  expect(email.y - (webhook.y + webhook.height), "E-mail sits right under Webhook").toBeLessThan(24);
  expect(webhook.x, "Webhook is the wide column to the right").toBeGreaterThan(events.x + events.width - 1);
  expect(webhook.width).toBeGreaterThan(events.width * 1.5);
  await shoot(page, "a-empty");

  await page.setViewportSize({ width: 375, height: 812 });
  const xs = await Promise.all(["#card-notify-events", "#card-notify-webhook", "#card-notify-message", "#card-notify-email"].map(async (id) => (await box(id)).x));
  expect(new Set(xs.map((x) => Math.round(x))).size, "one column").toBe(1);
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
});

// Why: the whole point is that the receiver gets the message. The Home Assistant preset sends the
// same JSON; the secret header goes with it; and the secret must never come back to the browser.
test("webhook: save, the secret is not shown again, Send test reaches the receiver", { tag: "@core" }, async ({ page }) => {
  await open(page);
  await page.locator("[data-key='NOTIFY_WEBHOOK'] .swtrack").click();
  await page.getByLabel("Home Assistant").check();
  await page.locator("input[name='v.NOTIFY_WEBHOOK_URL']").fill(`http://127.0.0.1:${hook.port}/api/webhook/tf-notify-9c1f`);
  await page.getByLabel("Secret header").check();
  await page.locator("input[name='v.NOTIFY_WEBHOOK_SECRET']").fill(secret);
  // Send test works with the unsaved form.
  await sendTest(page, "#card-notify-webhook");
  await expect(page.locator("#card-notify-webhook .sys-test-res")).toHaveText("Webhook test: sent, HTTP 200");
  expect(hook.seen).toHaveLength(1);
  expect(JSON.parse(hook.seen[0].body)).toMatchObject({ event: "test", result: "ok", title: "TasmoFleet test notification" });
  expect(hook.seen[0].headers["authorization"]).toBe(secret);
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();

  await page.goto(app.url + "/settings/system?tab=notifications");
  const html = await page.content();
  expect(html).not.toContain(secret);
  await expect(page.locator("input[name='v.NOTIFY_WEBHOOK_SECRET']")).toHaveAttribute("placeholder", "unchanged (a value is stored)");
  await expect(page.locator("input[name='v.NOTIFY_WEBHOOK_URL']")).toHaveValue(/tf-notify-9c1f/);
  // The saved secret is used by the next test without being typed again.
  hook.seen.length = 0;
  await sendTest(page, "#card-notify-webhook");
  await expect(page.locator("#card-notify-webhook .sys-test-res")).toHaveText("Webhook test: sent, HTTP 200");
  expect(hook.seen[0].headers["authorization"]).toBe(secret);
});

// Why: a wrong address is the commonest mistake. The test must say what the receiver answered, in
// the danger tone, and the answer must not contain the address (a webhook id is a secret).
test("webhook: a refusal is shown as the receiver's answer", async ({ page }) => {
  await open(page);
  hook.status = 404;
  await sendTest(page, "#card-notify-webhook");
  const res = page.locator("#card-notify-webhook .sys-test-res");
  await expect(res).toHaveText("Webhook test: failed: HTTP 404");
  await expect(res).toHaveAttribute("role", "alert");
  await expect(res).not.toContainText("tf-notify-9c1f");
});

// Why: e-mail has its own settings, a status and a test; the password is write-only like the others.
test("e-mail: Send test delivers one message and the password stays write-only", { tag: "@core" }, async ({ page }) => {
  await open(page);
  await page.locator("[data-key='NOTIFY_SMTP'] .swtrack").click();
  await page.locator("input[name='v.NOTIFY_SMTP_HOST']").fill("127.0.0.1");
  await page.locator("input[name='v.NOTIFY_SMTP_PORT']").fill(String(smtp.port));
  await page.locator("select[name='v.NOTIFY_SMTP_SECURITY']").selectOption("none");
  await page.locator("input[name='v.NOTIFY_SMTP_PASSWORD']").fill(mailPass);
  await page.locator("input[name='v.NOTIFY_SMTP_FROM']").fill("TasmoFleet <tasmofleet@example.com>");
  await page.locator("input[name='v.NOTIFY_SMTP_TO']").fill("admin@example.com");
  await sendTest(page, "#card-notify-email");
  await expect(page.locator("#card-notify-email .sys-test-res")).toHaveText("E-mail test: sent, accepted by 127.0.0.1");
  expect(smtp.data).toHaveLength(1);
  expect(smtp.data[0]).toContain("Subject: TasmoFleet test notification");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();
  await page.goto(app.url + "/settings/system?tab=notifications");
  expect(await page.content()).not.toContain(mailPass);
  await expect(page.locator("#card-notify-email .sys-status")).toContainText("Last test OK");
});

// Why: an event reaches the channels without anyone pressing a button. A scheduled snapshot that
// cannot be written (a file is in the way of its folder) is the one failure this spec can cause for real; the
// webhook answers 404 and the status line must say so.
test("a failed snapshot is delivered, and the failed delivery shows in the status line", async ({ page }) => {
  hook.seen.length = 0;
  await page.setExtraHTTPHeaders(admin);
  await page.goto(app.url + "/settings/system?tab=backups");
  await page.locator("[data-key='SNAPSHOT_SCHEDULE'] .swtrack").click();
  // A directory under the name of the snapshot about to be written (this minute and the next two): renaming a
  // file onto a directory fails for every user, root included, and the settings page keeps listing fine.
  const stamp = (d: Date) => d.toISOString().replace(/[-:]/g, "").replace(/^(\d{8})T(\d{4}).*$/, "$1-$2Z");
  for (let i = 0; i < 3; i++) mkdirSync(join(app.dataDir, "snapshots", `tasmofleet-${stamp(new Date(Date.now() + i * 60_000))}.db`), { recursive: true });
  await page.getByRole("button", { name: "Save settings" }).click();
  await page.waitForURL(/ok=saved/); // the save is done before the next page is opened
  await expect(async () => {
    await page.goto(app.url + "/settings/system?tab=notifications");
    await expect(page.locator("#card-notify-webhook .sys-status")).toContainText("Last delivery failed");
  }).toPass({ timeout: 20_000 });
  expect(JSON.parse(hook.seen[0].body)).toMatchObject({ event: "snapshot.failed", result: "failed", title: "Database snapshot failed" });
  await expect(page.locator("#card-notify-webhook .sys-status")).toContainText("HTTP 404");
  await expect(page.locator("#card-notify-email .sys-status")).toContainText("Last delivery OK"); // the mail went through
  await sendTest(page, "#card-notify-webhook"); // the receiver still answers 404: the red test result next to the button
  await expect(page.locator("#card-notify-webhook .sys-test-res")).toHaveText("Webhook test: failed: HTTP 404");
  await shoot(page, "c-failed");
  hook.status = 200;
  await sendTest(page, "#card-notify-webhook");
  await expect(page.locator("#card-notify-webhook .sys-test-res")).toHaveText("Webhook test: sent, HTTP 200");
  await shoot(page, "b-configured");
});
