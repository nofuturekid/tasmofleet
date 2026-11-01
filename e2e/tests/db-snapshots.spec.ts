import { readFileSync } from "node:fs";
import { test, expect } from "../support/fixtures";
import { App, startApp } from "../support/stack";

// System, Snapshots: the database download, the stored snapshots and the snapshot schedule (issues #301, #410).
// A local admin (break-glass login) confirms with the password; a proxy-header admin, who has no
// password to give, confirms with a checkbox.
let app: App;
test.beforeAll(async () => {
  app = await startApp({ env: { TASMOFLEET_AUTH_MODE: "header", TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32", TASMOFLEET_ADMIN_GROUPS: "adm" } });
  app.cli(["user", "passwd", "--create", "--role", "admin", "snapper"], "e2e-fake-password-1\n");
});
test.afterAll(async () => {
  await app?.stop();
});

async function signIn(page: import("@playwright/test").Page) {
  await page.goto(app.url + "/login");
  await page.getByLabel("Username").fill("snapper");
  await page.getByLabel("Password").fill("e2e-fake-password-1");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Fleet overview" })).toBeVisible();
}

// Why: the file holds every secret of the installation, so it is only handed out after the
// password, it is a real SQLite file, and secret.key comes with it only when asked for.
test("download: password first, then a SQLite file; the key only inside a .tar when ticked", { tag: "@core" }, async ({ page }) => {
  await signIn(page);
  await page.goto(app.url + "/settings/system/snapshots");
  const card = page.locator("#card-download");
  await expect(card).toContainText("The file holds secrets. Keep it like a password.");
  await expect(card.locator("input[name='include_key']")).not.toBeChecked();

  await card.getByRole("button", { name: "Download snapshot" }).click();
  const stepup = page.locator("dialog#stepup");
  await expect(stepup).toBeVisible();
  await expect(stepup).toContainText("Downloading a snapshot of the database");
  await stepup.getByLabel("Password").fill("e2e-fake-password-1");
  await stepup.getByRole("button", { name: "Confirm" }).click();
  await expect(page).toHaveURL(/\/settings\/system\/snapshots/);

  const [plain] = await Promise.all([page.waitForEvent("download"), page.locator("#card-download").getByRole("button", { name: "Download snapshot" }).click()]);
  expect(plain.suggestedFilename()).toMatch(/^tasmofleet-\d{8}-\d{4}Z\.db$/);
  expect(readFileSync((await plain.path())!).subarray(0, 15).toString()).toBe("SQLite format 3");

  await page.locator("#card-download input[name='include_key']").check();
  const [withKey] = await Promise.all([page.waitForEvent("download"), page.locator("#card-download").getByRole("button", { name: "Download snapshot" }).click()]);
  expect(withKey.suggestedFilename()).toMatch(/^tasmofleet-\d{8}-\d{4}Z\.tar$/);
  const tar = readFileSync((await withKey.path())!);
  expect(tar.includes("secret.key")).toBe(true);
  expect(tar.includes("tasmofleet.db")).toBe(true);

  await page.goto(app.url + "/audit");
  await expect(page.locator("table.audittable").getByText("Database snapshot download").first()).toBeVisible();
});

// Why (#387): the person ticked "Include secret.key" and clicked Download. After the password they must not
// have to tick and click again (and a forgotten tick yields a file that cannot be read later): the page
// comes back with the box still ticked and the Download button focused, so one key press finishes it.
// A download started by the redirect itself is not used: browsers may block it without a user gesture.
test("download: the key choice survives the password step-up", { tag: "@core" }, async ({ page }) => {
  await signIn(page);
  await page.goto(app.url + "/settings/system/snapshots");
  const card = page.locator("#card-download");
  await card.locator("input[name='include_key']").check();
  await card.getByRole("button", { name: "Download snapshot" }).click();
  const stepup = page.locator("dialog#stepup");
  await expect(stepup).toBeVisible();
  await stepup.getByLabel("Password").fill("e2e-fake-password-1");
  await stepup.getByRole("button", { name: "Confirm" }).click();

  await expect(page).toHaveURL(/\/settings\/system\/snapshots.*dlkey=1/);
  await expect(page.locator("#card-download input[name='include_key']")).toBeChecked();
  await expect(page.locator("#btn-dl")).toBeFocused();
  const [withKey] = await Promise.all([page.waitForEvent("download"), page.keyboard.press("Enter")]);
  expect(withKey.suggestedFilename()).toMatch(/^tasmofleet-\d{8}-\d{4}Z\.tar$/);
  expect(readFileSync((await withKey.path())!).includes("secret.key")).toBe(true);
});

// Why: the schedule is off until switched on; switching it on takes a snapshot at once (like the
// other schedules), it is listed with Download and Delete, and the card says when it ran.
test("schedule: switch on, a snapshot appears, it can be downloaded and deleted", async ({ page }) => {
  await signIn(page);
  await page.goto(app.url + "/settings/system/snapshots");
  await expect(page.locator("#card-stored")).toContainText("No snapshots yet.");
  await expect(page.locator("input[name='sw.SNAPSHOT_INTERVAL']")).not.toBeChecked();

  await page.locator("[data-key='SNAPSHOT_SCHEDULE'] .swtrack").click();
  await expect(page.locator("input[name='v.SNAPSHOT_INTERVAL']")).toHaveValue("24h");
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByText("Saved.")).toBeVisible();

  await expect(async () => {
    await page.goto(app.url + "/settings/system/snapshots");
    await expect(page.locator("#card-stored .sys-orphan")).toHaveCount(1);
  }).toPass({ timeout: 20_000 });
  await expect(page.locator("#card-snapshots")).toContainText("Last snapshot");
  await expect(page.locator("#card-stored .sys-orphan")).toContainText("Latest");

  // A stored snapshot is as secret as a fresh one: this session has not confirmed yet.
  await page.locator("#card-stored .sys-orphan").getByRole("link", { name: "Download" }).click();
  const stepup = page.locator("dialog#stepup");
  await expect(stepup).toBeVisible();
  await stepup.getByLabel("Password").fill("e2e-fake-password-1");
  await stepup.getByRole("button", { name: "Confirm" }).click();
  await expect(page).toHaveURL(/\/settings\/system\/snapshots/);
  const [dl] = await Promise.all([page.waitForEvent("download"), page.locator("#card-stored .sys-orphan").getByRole("link", { name: "Download" }).click()]);
  expect(dl.suggestedFilename()).toMatch(/^tasmofleet-\d{8}-\d{4}Z\.db$/);

  await page.locator("#card-stored .sys-orphan").getByRole("link", { name: "Delete" }).click();
  const dlg = page.locator("dialog#dlg-bkdel");
  await expect(dlg).toBeVisible();
  await dlg.getByRole("button", { name: "Delete snapshot" }).click();
  await expect(page.getByText("Snapshot deleted.")).toBeVisible();
  await expect(page.locator("#card-stored .sys-orphan")).toHaveCount(0);
});

// Why: a proxy-header admin is not blocked, but the download is never one click: a page with a
// required checkbox comes first, and only the ticked, CSRF-protected POST sends the file.
test("header admin: confirm with the checkbox, then the download starts", async ({ page }) => {
  await page.setExtraHTTPHeaders({ "X-authentik-username": "adam", "X-authentik-groups": "adm" });
  await page.goto(app.url + "/settings/system/snapshots");
  await page.locator("#card-download").getByRole("button", { name: "Download snapshot" }).click();
  await expect(page.getByText("I understand this file holds secrets.")).toBeVisible();
  const [file] = await Promise.all([page.waitForEvent("download"), (async () => {
    await page.getByLabel("I understand this file holds secrets.").check();
    await page.getByRole("button", { name: "Download", exact: true }).click();
  })()]);
  expect(file.suggestedFilename()).toMatch(/^tasmofleet-\d{8}-\d{4}Z\.db$/);
});
