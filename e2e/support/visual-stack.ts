import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Browser } from "@playwright/test";
import { designFixturePath, designPolicyRules, deviceIds, isMedia, loadDesignFixture, mediaFixture, pageAdmin, seedDesignV2, seedRunningTransaction, type DesignFixture } from "./seed";
import { App, Devenv, freePort, startApp, startDevenv } from "./stack";
import type { StackKind } from "./visual-entries";

// The app instances the visual comparison renders. support/visual-global.ts starts one per kind the
// selected entries need (a run of a few screens never starts the others), once for all workers.

export interface VisualStack {
  kind: StackKind;
  app: App;
  /** {dN} (app id of fixture device dN) and {txn} (a transaction id) for entry routes */
  placeholders: Map<string, string>;
  stop(): Promise<void>;
}

const headerEnv = {
  TASMOFLEET_AUTH_MODE: "header",
  TASMOFLEET_TRUSTED_PROXIES: "127.0.0.1/32",
  TASMOFLEET_OPERATOR_GROUPS: "ops",
  TASMOFLEET_ADMIN_GROUPS: "adm",
};

/** localPassword is the fake password of the accounts of the "local" stack. */
export const localPassword = "design-fixture-password-1";

/** mutating: the stack of the entries that change data (a copy of the fleet without the frozen running transaction). */
export async function startVisualStack(kind: StackKind, browser: Browser, mutating = false): Promise<VisualStack> {
  switch (kind) {
    case "fleet":
      return startFleet(browser, mutating);
    case "empty":
      return plain(kind, await startApp({ env: headerEnv }));
    case "local": {
      const app = await startApp({ env: { ...headerEnv, TASMOFLEET_TRUSTED_PROXIES: "127.0.0.2/32" } });
      app.cli(["user", "passwd", "--create", "--role", "admin", "maren"], localPassword + "\n");
      app.cli(["user", "passwd", "--create", "--role", "operator", "--must-change", "ali"], localPassword + "\n");
      return plain(kind, app);
    }
    case "setup": {
      // {setuptoken}: the token of the setup link in the log (N12), for the step-1 entries
      const app = await startApp({ bare: true, scheme: "https" });
      const token = new URL(app.setupLink() || "/", app.url).searchParams.get("token") ?? "";
      if (!token) throw new Error("the fresh install printed no setup link");
      return { ...plain(kind, app), placeholders: new Map([["setuptoken", token]]) };
    }
  }
}

function plain(kind: StackKind, app: App): VisualStack {
  return { kind, app, placeholders: new Map(), stop: () => app.stop() };
}

async function startFleet(browser: Browser, mutating: boolean): Promise<VisualStack> {
  let fx = loadDesignFixture();
  let fixturePath = designFixturePath;
  let mediaDir = "";
  if (isMedia) {
    // the README pictures show a cleaned-up copy of the fleet, written next to the run
    fx = mediaFixture(fx);
    mediaDir = mkdtempSync(join(tmpdir(), "tasmofleet-media-"));
    fixturePath = join(mediaDir, "media-fixture.json");
    writeFileSync(fixturePath, JSON.stringify(fx));
  }
  const env = await startDevenv(["-design-fixture", fixturePath]);
  const fwPort = await freePort();
  const app = await startApp({
    env: {
      ...headerEnv,
      TASMOFLEET_MQTT_URL: env.vars.TASMOFLEET_MQTT_URL,
      TASMOFLEET_POLL_INTERVAL: "5s",
      TASMOFLEET_FIRMWARE_UPSTREAM_OVERRIDE: env.vars.DEVENV_UPSTREAM_GITHUB_URL,
      TASMOFLEET_POLICY_RULES: designPolicyRules(fx),
      TASMOFLEET_FIRMWARE_LISTEN: `127.0.0.1:${fwPort}`,
      TASMOFLEET_FIRMWARE_BASE_URL: `http://127.0.0.1:${fwPort}`,
    },
  });
  const stop = async () => {
    await app.stop();
    await env.stop();
    if (mediaDir) rmSync(mediaDir, { recursive: true, force: true });
  };
  const page = await browser.newPage({ extraHTTPHeaders: pageAdmin, viewport: { width: 1280, height: 900 } });
  try {
    importFleet(app, env, fx);
    await seedDesignV2(page, app, env, fx);
    const ids = await deviceIds(page, app);
    const placeholders = new Map<string, string>();
    for (const d of fx.devices) placeholders.set(d.id, ids.get(d.name) ?? "");
    // The frozen running transaction would start for real on the first submission, which the
    // mutating stack's entries make: it gets none.
    if (!mutating) await seedRunningTransaction(app, ids, fx);
    await page.goto(app.url + "/transactions");
    const href = await page.locator('a[href*="/transactions/"]').first().getAttribute("href");
    if (href) placeholders.set("txn", href.split("/").pop()!);
    placeholders.set("backup", await newestBackup(app, ids.get(fx.devices.find((d) => d.id === "d2")!.name)!));
    placeholders.set("control", env.vars.DEVENV_CONTROL_URL); // the devenv's switches (firmware feed states)
    return { kind: "fleet", app, placeholders, stop };
  } catch (e) {
    await stop();
    throw e;
  } finally {
    await page.close();
  }
}

/**
 * importFleet imports the fixture's devices through the TasmoAdmin CSV, as the other specs do. The
 * reachable ones are probed (identity, chip family); the two "unknown" devices have nothing
 * listening, so they are imported separately without a probe.
 */
function importFleet(app: App, env: Devenv, fx: DesignFixture): void {
  const dirs: string[] = [];
  const write = (names: string[], noProbe: boolean) => {
    const dir = mkdtempSync(join(tmpdir(), "tasmofleet-design-"));
    dirs.push(dir);
    const rows = names.map((name, i) => {
      const d = env.devices[name];
      const [host, port] = d.addr.split(":");
      return `${i + 1},"${name}",${host},,,bulb_1,1,1,0,0,1,${port},"${name}",0,${d.topic},0`;
    });
    writeFileSync(join(dir, "devices.csv"), rows.join("\n") + "\n");
    app.cli(["import", "tasmoadmin", ...(noProbe ? ["--no-probe"] : []), dir]);
  };
  try {
    write(fx.devices.filter((d) => d.status !== "unknown").map((d) => d.name), false);
    write(fx.devices.filter((d) => d.status === "unknown").map((d) => d.name), true);
  } finally {
    for (const d of dirs) rmSync(d, { recursive: true, force: true });
  }
}

/** newestBackup is the id of the newest config dump of a device (the restorable file of its newest run), read from the app's database. */
async function newestBackup(app: App, deviceId: string): Promise<string> {
  const { DatabaseSync } = await import("node:sqlite");
  const db = new DatabaseSync(join(app.dataDir, "tasmofleet.db"), { readOnly: true });
  try {
    const row = db.prepare("SELECT id FROM backups WHERE device_id = ? AND kind = 'dmp' ORDER BY taken_at DESC, id DESC LIMIT 1").get(Number(deviceId)) as { id: number } | undefined;
    return row ? String(row.id) : "";
  } finally {
    db.close();
  }
}
