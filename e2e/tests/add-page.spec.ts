import { connect } from "node:net";
import { join } from "node:path";
import { test, expect } from "../support/fixtures";
import { expectNoOverflow } from "../support/layout";
import { App, Devenv, startApp, startDevenv } from "../support/stack";

// #410: Devices -> Add is one page with four boxes (Discovered + Network scan wide, Add manually +
// Import narrow), no tabs. Every way of adding a device still works from its box.
const adam = { "X-authentik-username": "adam", "X-authentik-groups": "adm" };
let app: App;
let env: Devenv;

const seeded = [
  { mac: "aa:bb:cc:00:02:01", name: "Shed light", topic: "shed_light", host: "shed-light-1a2b", ip: "127.0.0.21", seen: 600 },
  { mac: "aa:bb:cc:00:02:02", name: "Porch plug", topic: "porch_plug", host: "porch-plug-3c4d", ip: "127.0.0.22", seen: 60 },
];

function publishRetained(url: string, topic: string, payload: string): Promise<void> {
  const { hostname, port } = new URL(url);
  const str = (s: string) => Buffer.concat([Buffer.from([s.length >> 8, s.length & 255]), Buffer.from(s)]);
  const packet = (head: number, body: Buffer) => Buffer.concat([Buffer.from([head, body.length]), body]);
  return new Promise((ok, fail) => {
    const sock = connect(Number(port), hostname, () => {
      sock.write(packet(0x10, Buffer.concat([str("MQTT"), Buffer.from([4, 2, 0, 60]), str("e2e-add-page")])));
    });
    sock.once("data", () => {
      sock.write(packet(0x31, Buffer.concat([str(topic), Buffer.from(payload)])));
      sock.end(Buffer.from([0xe0, 0]), () => ok());
    });
    sock.once("error", fail);
  });
}

test.beforeAll(async () => {
  test.setTimeout(120_000);
  env = await startDevenv();
  app = await startApp({
    env: {
      TASMOFLEET_AUTH_MODE: "header",
      TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
      TASMOFLEET_ADMIN_GROUPS: "adm",
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
    },
  });
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"));
  try {
    db.exec("PRAGMA busy_timeout=5000");
    const now = Date.now();
    for (const d of seeded) {
      db.prepare(
        `INSERT INTO discovery_proposals (mac, kind, via, reason, name, address, port, topic, full_topic, existing_id, dismissed, first_seen, seen_at, hostname)
         VALUES (?, 'new', 'mqtt', '', ?, ?, 80, ?, 'tasmota/%topic%/%prefix%/', 0, 0, ?, ?, ?)`,
      ).run(d.mac, d.name, d.ip, d.topic, now - 3600_000, now - d.seen * 1000, d.host);
    }
  } finally {
    db.close();
  }
  for (const d of seeded) await publishRetained(env.vars.TASMOFLEET_MQTT_URL, `tele/${d.topic}/LWT`, "Offline");
});
test.afterAll(async () => {
  await app?.stop();
  await env?.stop();
});
test.beforeEach(async ({ page }) => {
  await page.setExtraHTTPHeaders(adam);
});

const box = (page: import("@playwright/test").Page, id: string) => page.locator(`#card-${id}`);

// Why: the four ways to add devices are one page, so there are fewer clicks and fewer menus: Discovered and
// Network scan in the wide column on the left, Add manually and Import in the narrow one on the right, no
// tabs, and the sub-navigation says List · Policy · Add.
test("four boxes in two columns, no tabs, the menu says List, Policy, Add", { tag: "@core" }, async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/devices/new");
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Add devices");
  await expect(page.locator("nav.utabs")).toHaveCount(0);
  const sub = page.getByRole("navigation", { name: "Devices" });
  await expect(sub.getByRole("link")).toHaveText([/^List$/, /^Policy$/, /^Add/]);
  await expect(page.locator("#crumbs")).toContainText("Devices");
  await expect(page.locator("#crumbs")).toContainText("Add");
  const b = async (id: string) => (await box(page, id).boundingBox())!;
  const [disc, scan, manual, imp] = [await b("discovered"), await b("scan"), await b("manual"), await b("import")];
  expect(disc.x).toBeLessThan(manual.x);
  expect(Math.abs(scan.x - disc.x), "Network scan under Discovered").toBeLessThan(2);
  expect(Math.abs(imp.x - manual.x), "Import under Add manually").toBeLessThan(2);
  expect(scan.y).toBeGreaterThan(disc.y + disc.height - 1);
  expect(imp.y).toBeGreaterThan(manual.y + manual.height - 1);
  expect(disc.width, "the Discovered box is the wide one").toBeGreaterThan(manual.width * 1.7);
  // the Discovered table has no MAC, Hostname or First seen column any more
  for (const gone of ["Hostname", "MAC", "First seen"]) await expect(box(page, "discovered").getByRole("columnheader", { name: gone })).toHaveCount(0);
  await expectNoOverflow(page, "Add devices");
});

// Why: on a phone the order is the one a person reads: Discovered, Add manually, Network scan, Import.
test("phone: Discovered, Add manually, Network scan, Import, one column", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 800 });
  await page.goto(app.url + "/devices/new");
  const ys: number[] = [];
  for (const id of ["discovered", "manual", "scan", "import"]) {
    const r = (await box(page, id).boundingBox())!;
    expect(r.width, `${id} fills the phone`).toBeGreaterThan(330);
    ys.push(r.y);
  }
  expect(ys).toEqual([...ys].sort((x, y) => x - y));
  await expectNoOverflow(page, "Add devices phone");
});

// Why: Ignore, Show ignored and Restore of a discovered device must keep working from the box, and "Add…" must
// take the person to the manual form with that device filled in (the form is in the same page).
test("Discovered: ignore, show ignored, restore, and Add… prefills the manual box", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/devices/new");
  const row = (name: string) => box(page, "discovered").getByRole("row").filter({ hasText: name });
  await row("Porch plug").getByRole("button", { name: "Ignore", exact: true }).click();
  await expect(page.getByText("Dismissed.")).toBeVisible();
  await expect(row("Porch plug")).toHaveCount(0);
  await box(page, "discovered").getByRole("checkbox", { name: /Show ignored/ }).click();
  await row("Porch plug").getByRole("button", { name: "Restore" }).click();
  await expect(page.getByText("Restored.")).toBeVisible();
  await row("Porch plug").getByRole("link", { name: "Add…" }).click();
  await expect(box(page, "manual").locator("input[name=address]")).toHaveValue("127.0.0.22");
  await expect(box(page, "manual")).toContainText("Prefilled from MQTT discovery");
});

// Why: Scan now stays one click, and the scan runs live where its results are: the results page. The box
// then says in one line what the last scan found, WHEN (a date stamp, not a bare clock time) and links all results.
test("Network scan: Scan now opens the live results, the box keeps a one-line summary with All results", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(app.url + "/devices/new");
  await box(page, "scan").getByLabel("Subnet").fill("127.0.0.0/30");
  await box(page, "scan").getByRole("button", { name: "Scan now" }).click();
  await expect(page).toHaveURL(/\/discovery\/scan\/[0-9a-f]+$/);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Network scan");
  await expect(page.getByText("Scan finished.")).toBeVisible({ timeout: 30_000 });
  await page.getByRole("link", { name: "Back to Add devices" }).click();
  await expect(page).toHaveURL(/\/devices\/new$/);
  const scan = box(page, "scan");
  await expect(scan).toContainText("Scan finished.");
  await expect(scan.locator("time[data-ts=day]")).toBeVisible();
  await expect(scan).toContainText("(in Discovered)");
  await expect(scan.locator("table")).toHaveCount(0);
  await scan.getByRole("link", { name: "All results" }).click();
  await expect(page).toHaveURL(/\/discovery\/scan\/[0-9a-f]+$/);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("Network scan");
});

// Why: the manual box tests a connection without storing anything and adds the device when asked.
test("Add manually: Test connection stores nothing, Add device does", async ({ page }) => {
  const [name, d] = Object.entries(env.devices)[0];
  const [host, port] = d.addr.split(":");
  await page.goto(app.url + "/devices/new");
  const manual = box(page, "manual");
  await manual.locator("input[name=address]").fill(host);
  await manual.getByText("More options").click();
  await manual.locator("input[name=port]").fill(port);
  await manual.locator("input[name=name]").fill(name);
  await manual.getByRole("button", { name: "Test connection" }).click();
  await expect(page.getByText("Nothing was added yet.")).toBeVisible();
  await manual.getByRole("button", { name: "Add device" }).click();
  await expect(page).toHaveURL(/\/devices\/\d+/); // the add went through before the list is opened
  await page.goto(app.url + "/devices");
  await expect(page.getByRole("link", { name, exact: true })).toBeVisible();
});
