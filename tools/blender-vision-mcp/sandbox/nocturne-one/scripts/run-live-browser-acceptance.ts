import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import Database from "better-sqlite3";
import {
  chromium,
  type BrowserContext,
  type Page,
  type Response
} from "playwright";

interface ProbeState {
  state: string;
  stateHistory: string[];
  declaredStates: string[];
  route: string;
  config: {
    variant: string;
    light_intensity: number;
    orientation: number;
    accessory: string;
  };
  sceneConfig: {
    variant: string;
    light_intensity: number;
    orientation: number;
    accessory: string;
  };
  posterVisible: boolean;
  glbRequested: boolean;
  webglAvailable: boolean;
  reducedMotion: boolean;
  animationEnabled: boolean;
  selectedPart: string | null;
}

const [origin, artifactRoot, databasePath] = process.argv.slice(2);
assert.ok(origin && artifactRoot && databasePath, "origin, artifact root, and database are required");
const screenshotsRoot = path.join(artifactRoot, "screenshots");
const tracesRoot = path.join(artifactRoot, "traces");
await mkdir(screenshotsRoot, { recursive: true });
await mkdir(tracesRoot, { recursive: true });

function digest(bytes: Buffer | Uint8Array | string): string {
  return createHash("sha256").update(bytes).digest("hex");
}

function percentile(values: number[], ratio: number): number {
  const ordered = [...values].sort((left, right) => left - right);
  return ordered[Math.min(ordered.length - 1, Math.max(0, Math.ceil(ordered.length * ratio) - 1))]!;
}

async function probe(page: Page): Promise<ProbeState> {
  return page.evaluate(() => {
    const value = window.__NOCTURNE__;
    return {
      state: value.state,
      stateHistory: [...value.stateHistory],
      declaredStates: [...value.declaredStates],
      route: value.route,
      config: value.config,
      sceneConfig: value.sceneConfig,
      posterVisible: value.posterVisible,
      glbRequested: value.glbRequested,
      webglAvailable: value.webglAvailable,
      reducedMotion: value.reducedMotion,
      animationEnabled: value.animationEnabled,
      selectedPart: value.selectedPart
    };
  });
}

async function waitForState(page: Page, state: string, timeout = 30_000): Promise<void> {
  await page.waitForFunction(
    (expected) => window.__NOCTURNE__?.state === expected,
    state,
    { timeout }
  );
}

const screenshotRecords: Array<Record<string, unknown>> = [];
async function capture(page: Page, name: string, fullPage = true): Promise<void> {
  const filename = path.join(screenshotsRoot, `${name}.png`);
  const bytes = await page.screenshot({ path: filename, fullPage });
  screenshotRecords.push({
    name,
    path: filename,
    bytes: bytes.length,
    sha256: digest(bytes),
    viewport: page.viewportSize(),
    url: page.url()
  });
}

async function accessibilityFindings(page: Page): Promise<Array<Record<string, string>>> {
  return page.evaluate(() => {
    const findings: Array<Record<string, string>> = [];
    if (!document.documentElement.lang) {
      findings.push({
        id: "html-lang",
        impact: "serious",
        detail: "Document has no language."
      });
    }
    if (document.querySelectorAll("main").length !== 1) {
      findings.push({
        id: "main",
        impact: "serious",
        detail: "Expected exactly one main landmark."
      });
    }
    if (document.querySelectorAll("h1").length !== 1) {
      findings.push({
        id: "h1",
        impact: "serious",
        detail: "Expected exactly one level-one heading."
      });
    }
    if (!document.querySelector('a[href="#main"]')) {
      findings.push({
        id: "skip-link",
        impact: "serious",
        detail: "Skip link is missing."
      });
    }
    const identifiers = [...document.querySelectorAll<HTMLElement>("[id]")].map(
      (element) => element.id
    );
    for (const identifier of new Set(identifiers)) {
      if (identifiers.filter((item) => item === identifier).length > 1) {
        findings.push({
          id: "duplicate-id",
          impact: "serious",
          detail: `Duplicate id: ${identifier}`
        });
      }
    }
    for (const element of document.querySelectorAll<HTMLElement>(
      "button,a[href],input,select,textarea"
    )) {
      const label =
        element.getAttribute("aria-label") ||
        element.getAttribute("title") ||
        element.textContent ||
        (element.id
          ? document.querySelector(`label[for="${element.id}"]`)?.textContent
          : "");
      if (!label?.trim()) {
        findings.push({
          id: "accessible-name",
          impact: "critical",
          detail: `${element.tagName} has no name.`
        });
      }
    }
    for (const image of document.querySelectorAll("img")) {
      if (!image.hasAttribute("alt")) {
        findings.push({
          id: "image-alt",
          impact: "serious",
          detail: "Image is missing alt."
        });
      }
    }
    return findings;
  });
}

const consoleRecords: Array<Record<string, unknown>> = [];
const networkRecords: Array<Record<string, unknown>> = [];
const failedRequests: Array<Record<string, unknown>> = [];
function instrumentPage(page: Page, label: string): void {
  page.on("console", (message) => {
    consoleRecords.push({
      label,
      type: message.type(),
      text: message.text(),
      url: page.url()
    });
  });
  page.on("pageerror", (error) => {
    consoleRecords.push({ label, type: "pageerror", text: error.message, url: page.url() });
  });
  page.on("requestfailed", (request) => {
    failedRequests.push({
      label,
      url: request.url(),
      method: request.method(),
      failure: request.failure()?.errorText ?? "unknown"
    });
  });
  page.on("response", (response) => {
    const url = response.url();
    if (
      url.includes("/api/") ||
      url.endsWith(".glb") ||
      url.endsWith(".js") ||
      url.endsWith(".css") ||
      url.endsWith(".webp")
    ) {
      networkRecords.push({
        label,
        url,
        status: response.status(),
        contentLength: Number(response.headers()["content-length"] ?? 0),
        contentType: response.headers()["content-type"] ?? null
      });
    }
  });
}

async function addPerformanceObservers(context: BrowserContext): Promise<void> {
  await context.addInitScript(() => {
    const metrics = {
      cls: 0,
      longTasks: [] as number[],
      interactions: [] as number[]
    };
    Object.defineProperty(globalThis, "__LIVE_ACCEPTANCE_METRICS__", {
      value: metrics,
      configurable: false
    });
    try {
      new PerformanceObserver((list) => {
        for (const entry of list.getEntries() as Array<PerformanceEntry & { value?: number; hadRecentInput?: boolean }>) {
          if (!entry.hadRecentInput) metrics.cls += Number(entry.value ?? 0);
        }
      }).observe({ type: "layout-shift", buffered: true });
    } catch {
      // Unsupported metrics remain explicitly empty.
    }
    try {
      new PerformanceObserver((list) => {
        for (const entry of list.getEntries()) metrics.longTasks.push(entry.duration);
      }).observe({ type: "longtask", buffered: true });
    } catch {
      // Unsupported metrics remain explicitly empty.
    }
    try {
      new PerformanceObserver((list) => {
        for (const entry of list.getEntries()) metrics.interactions.push(entry.duration);
      }).observe(
        {
          type: "event",
          buffered: true,
          durationThreshold: 16
        } as PerformanceObserverInit & { durationThreshold: number }
      );
    } catch {
      // Unsupported metrics remain explicitly empty.
    }
  });
}

async function runtimeMetrics(page: Page): Promise<Record<string, unknown>> {
  return page.evaluate(() => {
    const metrics = (
      globalThis as typeof globalThis & {
        __LIVE_ACCEPTANCE_METRICS__?: {
          cls: number;
          longTasks: number[];
          interactions: number[];
        };
      }
    ).__LIVE_ACCEPTANCE_METRICS__ ?? { cls: 0, longTasks: [], interactions: [] };
    const resources = performance.getEntriesByType("resource").map((entry) => {
      const resource = entry as PerformanceResourceTiming;
      return {
        name: resource.name,
        transferSize: resource.transferSize,
        encodedBodySize: resource.encodedBodySize,
        decodedBodySize: resource.decodedBodySize,
        duration: resource.duration,
        initiatorType: resource.initiatorType
      };
    });
    return {
      ...metrics,
      resources,
      navigation: performance.getEntriesByType("navigation").map((entry) => {
        const navigation = entry as PerformanceNavigationTiming;
        return {
          transferSize: navigation.transferSize,
          encodedBodySize: navigation.encodedBodySize,
          decodedBodySize: navigation.decodedBodySize,
          responseStart: navigation.responseStart,
          domContentLoadedEventEnd: navigation.domContentLoadedEventEnd,
          loadEventEnd: navigation.loadEventEnd
        };
      })
    };
  });
}

const browser = await chromium.launch({
  channel: "chrome",
  headless: true,
  args: [
    "--disable-background-networking",
    "--enable-precise-memory-info",
    "--js-flags=--expose-gc"
  ]
});
const stateEvidence: Array<Record<string, unknown>> = [];
const accessibility: Record<string, Array<Record<string, string>>> = {};
let desktopTrace = "";
let mobileTrace = "";
let desktopFrames: number[] = [];
let mobileFrames: number[] = [];
let desktopMetrics: Record<string, unknown> = {};
let mobileMetrics: Record<string, unknown> = {};
let firstRealFrameMs = 0;
let reservationId = "";
let mobileJourney: Array<Record<string, unknown>> = [];

try {
  const desktop = await browser.newContext({
    viewport: { width: 1440, height: 900 },
    deviceScaleFactor: 2,
    colorScheme: "dark",
    reducedMotion: "no-preference",
    serviceWorkers: "block"
  });
  await addPerformanceObservers(desktop);
  desktopTrace = path.join(tracesRoot, "desktop-1440x900-dpr2.zip");
  await desktop.tracing.start({ screenshots: true, snapshots: true, sources: true });
  const page = await desktop.newPage();
  instrumentPage(page, "desktop");

  await page.goto(`${origin}/`, { waitUntil: "networkidle" });
  await page.waitForFunction(() => Boolean(window.__NOCTURNE__));
  const poster = await probe(page);
  assert.equal(poster.posterVisible, true);
  assert.equal(poster.glbRequested, false);
  stateEvidence.push({ label: "desktop-poster", ...poster });
  await capture(page, "desktop-01-poster");

  const focusOrder: string[] = [];
  for (let index = 0; index < 8; index += 1) {
    await page.keyboard.press("Tab");
    focusOrder.push(
      await page.evaluate(() => document.activeElement?.id || document.activeElement?.tagName || "")
    );
  }
  assert.equal(focusOrder.length, 8);
  assert.ok(focusOrder.includes("enter-3d"), JSON.stringify(focusOrder));
  stateEvidence.push({ label: "keyboard-first-eight", focusOrder, ...(await probe(page)) });

  await page.goto(`${origin}/configurator`, { waitUntil: "networkidle" });
  await page.selectOption("#variant", "ember");
  await page.locator("#light").fill("72");
  await page.locator("#light").dispatchEvent("input");
  await page.locator("#orientation").fill("18");
  await page.locator("#orientation").dispatchEvent("input");
  await page.selectOption("#accessory", "braided-cable");
  const configured = await probe(page);
  assert.deepEqual(configured.config, configured.sceneConfig);
  const interactionStarted = performance.now();
  await page.locator("#save-configuration").click();
  await page.waitForFunction(
    () => document.querySelector("#configuration-status")?.textContent?.includes("saved")
  );
  const configurationSaveLatencyMs = performance.now() - interactionStarted;
  await page.reload({ waitUntil: "networkidle" });
  const restored = await probe(page);
  assert.deepEqual(restored.config, configured.config);
  assert.ok(restored.stateHistory.includes("restored_saved_configuration"));
  stateEvidence.push({ label: "restored-configuration", ...restored });
  await capture(page, "desktop-02-configured");

  await page.goto(`${origin}/`, { waitUntil: "networkidle" });
  const before3D = performance.now();
  await page.locator("#enter-3d").click();
  await waitForState(page, "3d_ready");
  firstRealFrameMs = performance.now() - before3D;
  const live = await probe(page);
  assert.equal(live.posterVisible, false);
  assert.equal(live.glbRequested, true);
  assert.equal(await page.locator("#product-canvas").getAttribute("tabindex"), "0");
  assert.ok(
    networkRecords.some(
      (item) => item.label === "desktop" && String(item.url).endsWith("nocturne-one-hero.glb")
    )
  );
  stateEvidence.push({ label: "desktop-real-3d", ...live });
  await capture(page, "desktop-03-real-3d");

  await page.evaluate(() => window.__NOCTURNE__.selectPart("glass_core"));
  assert.equal((await probe(page)).selectedPart, "glass_core");
  await page.waitForTimeout(250);
  await capture(page, "desktop-04-exploded-glass-core");
  const explodedA = await page.locator("#product-canvas").screenshot();
  await page.evaluate(() => window.__NOCTURNE__.selectPart("glass_core"));
  await page.waitForTimeout(250);
  const explodedB = await page.locator("#product-canvas").screenshot();
  assert.equal((await probe(page)).selectedPart, "glass_core");
  const explodedFrameHashes = [digest(explodedA), digest(explodedB)];
  await page.locator("#product-canvas").press("ArrowRight");
  desktopFrames = await page.evaluate(() => window.__NOCTURNE__.sampleFrames(180));
  assert.equal(desktopFrames.length, 180);
  assert.ok(desktopFrames.every((value) => Number.isFinite(value) && value > 0));
  desktopMetrics = {
    ...(await runtimeMetrics(page)),
    frameMedianMs: percentile(desktopFrames, 0.5),
    frameP95Ms: percentile(desktopFrames, 0.95),
    configurationSaveLatencyMs,
    firstRealFrameMs
  };

  for (const route of ["/", "/technology", "/configurator", "/reserve", "/receipt"]) {
    await page.goto(`${origin}${route}`, { waitUntil: "networkidle" });
    accessibility[`desktop:${route}`] = await accessibilityFindings(page);
    assert.deepEqual(accessibility[`desktop:${route}`], []);
  }
  await desktop.tracing.stop({ path: desktopTrace });
  await desktop.close();

  const slow = await browser.newContext({
    viewport: { width: 1440, height: 900 },
    deviceScaleFactor: 2,
    colorScheme: "dark",
    serviceWorkers: "block"
  });
  const slowPage = await slow.newPage();
  instrumentPage(slowPage, "slow-network");
  await slowPage.goto(`${origin}/`, { waitUntil: "networkidle" });
  await slowPage.evaluate(() => window.__NOCTURNE__.setTestCondition("slow_network"));
  assert.equal((await probe(slowPage)).posterVisible, true);
  await slowPage.locator("#enter-3d").click();
  await waitForState(slowPage, "slow_network");
  const slowPoster = await probe(slowPage);
  assert.equal(slowPoster.posterVisible, true);
  assert.equal(slowPoster.glbRequested, true);
  stateEvidence.push({ label: "slow-network-poster-first", ...slowPoster });
  await capture(slowPage, "desktop-05-slow-network-poster");
  await waitForState(slowPage, "3d_ready");
  await slow.close();

  const reduced = await browser.newContext({
    viewport: { width: 1440, height: 900 },
    deviceScaleFactor: 2,
    colorScheme: "dark",
    reducedMotion: "reduce",
    serviceWorkers: "block"
  });
  const reducedPage = await reduced.newPage();
  instrumentPage(reducedPage, "reduced-motion");
  await reducedPage.goto(`${origin}/`, { waitUntil: "networkidle" });
  const reducedBefore = await probe(reducedPage);
  assert.equal(reducedBefore.reducedMotion, true);
  assert.equal(reducedBefore.animationEnabled, false);
  assert.ok(reducedBefore.stateHistory.includes("reduced_motion"));
  await reducedPage.locator("#enter-3d").click();
  await waitForState(reducedPage, "3d_ready");
  stateEvidence.push({ label: "reduced-motion", ...(await probe(reducedPage)) });
  await capture(reducedPage, "desktop-06-reduced-motion");
  await reduced.close();

  const fallback = await browser.newContext({
    viewport: { width: 1440, height: 900 },
    deviceScaleFactor: 2,
    colorScheme: "dark",
    serviceWorkers: "block"
  });
  await fallback.addInitScript(() => {
    const original = HTMLCanvasElement.prototype.getContext;
    (
      HTMLCanvasElement.prototype as unknown as {
        getContext: (type: string, ...args: unknown[]) => unknown;
      }
    ).getContext = function (
      this: HTMLCanvasElement,
      type: string,
      ...args: unknown[]
    ) {
      if (type.startsWith("webgl")) return null;
      return (original as (...values: unknown[]) => unknown).call(this, type, ...args);
    };
  });
  const fallbackPage = await fallback.newPage();
  instrumentPage(fallbackPage, "no-webgl");
  await fallbackPage.goto(`${origin}/`, { waitUntil: "networkidle" });
  await fallbackPage.locator("#enter-3d").click();
  await waitForState(fallbackPage, "3d_unavailable");
  const fallbackState = await probe(fallbackPage);
  assert.equal(fallbackState.posterVisible, true);
  assert.equal(await fallbackPage.locator(".webgl-note").isVisible(), true);
  stateEvidence.push({ label: "no-webgl", ...fallbackState });
  await capture(fallbackPage, "desktop-07-no-webgl");
  await fallback.close();

  const mobile = await browser.newContext({
    viewport: { width: 390, height: 844 },
    deviceScaleFactor: 3,
    colorScheme: "dark",
    isMobile: true,
    hasTouch: true,
    serviceWorkers: "block"
  });
  await addPerformanceObservers(mobile);
  mobileTrace = path.join(tracesRoot, "mobile-390x844-dpr3.zip");
  await mobile.tracing.start({ screenshots: true, snapshots: true, sources: true });
  const mobilePage = await mobile.newPage();
  instrumentPage(mobilePage, "mobile");

  const step = async (id: number, name: string, evidence: object) => {
    mobileJourney.push({ step: id, name, url: mobilePage.url(), ...evidence });
  };
  await mobilePage.goto(`${origin}/`, { waitUntil: "networkidle" });
  await step(1, "home poster loaded", await probe(mobilePage));
  await capture(mobilePage, "mobile-01-poster");
  await mobilePage.locator("#enter-3d").tap();
  await waitForState(mobilePage, "3d_ready");
  await step(2, "touch entered real 3D", await probe(mobilePage));
  assert.ok(
    networkRecords.some(
      (item) => item.label === "mobile" && String(item.url).endsWith("nocturne-one-low.glb")
    )
  );
  await step(3, "mobile LOD loaded", {
    asset: networkRecords.find(
      (item) => item.label === "mobile" && String(item.url).endsWith("nocturne-one-low.glb")
    )
  });
  await capture(mobilePage, "mobile-02-real-3d");
  mobileFrames = await mobilePage.evaluate(() => window.__NOCTURNE__.sampleFrames(180));
  assert.equal(mobileFrames.length, 180);

  await mobilePage.goto(`${origin}/configurator`, { waitUntil: "networkidle" });
  await step(4, "configurator opened", await probe(mobilePage));
  await mobilePage.selectOption("#variant", "lunar");
  await mobilePage.locator("#light").fill("58");
  await mobilePage.locator("#light").dispatchEvent("input");
  await mobilePage.locator("#orientation").fill("-12");
  await mobilePage.locator("#orientation").dispatchEvent("input");
  await mobilePage.selectOption("#accessory", "braided-cable");
  await step(5, "material and configuration changed", await probe(mobilePage));
  await mobilePage.locator("#save-configuration").tap();
  await mobilePage.waitForFunction(
    () => document.querySelector("#configuration-status")?.textContent?.includes("saved")
  );
  await mobilePage.reload({ waitUntil: "networkidle" });
  const mobileRestored = await probe(mobilePage);
  assert.equal(mobileRestored.config.variant, "lunar");
  assert.ok(mobileRestored.stateHistory.includes("restored_saved_configuration"));
  await step(6, "configuration persisted after reload", mobileRestored);
  await capture(mobilePage, "mobile-03-configured");

  await mobilePage.goto(`${origin}/reserve`, { waitUntil: "networkidle" });
  await mobilePage.locator("#reserve-email").fill("invalid");
  await mobilePage.locator("#reserve-submit").tap();
  await waitForState(mobilePage, "api_validation_error");
  await step(7, "validation failure observed", await probe(mobilePage));
  await mobilePage.locator("#reserve-email").fill("live-mobile@example.invalid");
  await mobilePage.evaluate(() =>
    window.__NOCTURNE__.setTestCondition("api_transient_error")
  );
  await mobilePage.locator("#reserve-submit").tap();
  await waitForState(mobilePage, "api_transient_error");
  await mobilePage.evaluate(() => window.__NOCTURNE__.setTestCondition(null));
  await step(8, "transient 503 and retry state observed", await probe(mobilePage));

  await mobile.setOffline(true);
  await mobilePage.locator("#reserve-submit").tap();
  await waitForState(mobilePage, "offline_retry");
  await mobile.setOffline(false);
  await mobilePage.locator("#reserve-submit").tap();
  await waitForState(mobilePage, "successful_reservation");
  const storedReservation = await mobilePage.evaluate(() =>
    JSON.parse(localStorage.getItem("nocturne.lastReservation") ?? "null")
  );
  reservationId = storedReservation.id;
  assert.ok(String(reservationId).startsWith("res-"));
  await step(9, "offline interruption recovered to confirmed reservation", {
    ...(await probe(mobilePage)),
    reservationId
  });
  await mobilePage.goto(`${origin}/receipt`, { waitUntil: "networkidle" });
  await waitForState(mobilePage, "successful_reservation");
  assert.match(await mobilePage.locator("#reservation-status").innerText(), /confirmed/i);
  await step(10, "receipt restored from SQLite-backed API", await probe(mobilePage));
  await capture(mobilePage, "mobile-04-receipt");

  for (const route of ["/", "/technology", "/configurator", "/reserve", "/receipt"]) {
    await mobilePage.goto(`${origin}${route}`, { waitUntil: "networkidle" });
    accessibility[`mobile:${route}`] = await accessibilityFindings(mobilePage);
    assert.deepEqual(accessibility[`mobile:${route}`], []);
  }
  mobileMetrics = {
    ...(await runtimeMetrics(mobilePage)),
    frameMedianMs: percentile(mobileFrames, 0.5),
    frameP95Ms: percentile(mobileFrames, 0.95)
  };
  await mobile.tracing.stop({ path: mobileTrace });
  await mobile.close();

  const configuration = {
    variant: "ember",
    light_intensity: 72,
    orientation: 18,
    accessory: "braided-cable"
  };
  const apiBody = JSON.stringify({
    configuration,
    email: "live-api@example.invalid"
  });
  const apiHeaders = {
    "Content-Type": "application/json",
    "X-NOCTURNE-ACTOR": "live-api-actor",
    "X-NOCTURNE-PERMISSIONS": "reservation:create",
    "Idempotency-Key": "live-acceptance-fixed-idempotency-key"
  };
  const request = async (
    url: string,
    init?: RequestInit
  ): Promise<{ status: number; body: Record<string, unknown>; elapsedMs: number }> => {
    const started = performance.now();
    const response = await fetch(url, init);
    const elapsedMs = performance.now() - started;
    return {
      status: response.status,
      body: (await response.json()) as Record<string, unknown>,
      elapsedMs
    };
  };
  const unauthorized = await request(`${origin}/api/reservations`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: apiBody
  });
  const forbidden = await request(`${origin}/api/reservations`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-NOCTURNE-ACTOR": "live-api-actor",
      "Idempotency-Key": "live-forbidden"
    },
    body: apiBody
  });
  const invalid = await request(`${origin}/api/reservations`, {
    method: "POST",
    headers: apiHeaders,
    body: JSON.stringify({ configuration, email: "invalid" })
  });
  const transient = await request(`${origin}/api/reservations`, {
    method: "POST",
    headers: { ...apiHeaders, "X-NOCTURNE-SIMULATE": "transient" },
    body: apiBody
  });
  const first = await request(`${origin}/api/reservations`, {
    method: "POST",
    headers: apiHeaders,
    body: apiBody
  });
  const replay = await request(`${origin}/api/reservations`, {
    method: "POST",
    headers: apiHeaders,
    body: apiBody
  });
  const conflict = await request(`${origin}/api/reservations`, {
    method: "POST",
    headers: apiHeaders,
    body: JSON.stringify({ configuration, email: "changed@example.invalid" })
  });
  const own = await request(`${origin}/api/reservations/${first.body.id}`, {
    headers: { "X-NOCTURNE-ACTOR": "live-api-actor" }
  });
  const crossActor = await request(`${origin}/api/reservations/${first.body.id}`, {
    headers: { "X-NOCTURNE-ACTOR": "other-live-api-actor" }
  });
  assert.deepEqual(
    [
      unauthorized.status,
      forbidden.status,
      invalid.status,
      transient.status,
      first.status,
      replay.status,
      conflict.status,
      own.status,
      crossActor.status
    ],
    [401, 403, 400, 503, 201, 200, 409, 200, 404]
  );
  assert.equal(replay.body.id, first.body.id);

  const latencySamples: number[] = [];
  for (let index = 0; index < 100; index += 1) {
    const health = await request(`${origin}/api/health`);
    assert.equal(health.status, 200);
    latencySamples.push(health.elapsedMs);
  }

  const db = new Database(databasePath, { readonly: true, fileMustExist: true });
  const databaseEvidence = {
    integrityCheck: db.pragma("integrity_check", { simple: true }),
    journalMode: db.pragma("journal_mode", { simple: true }),
    migrationCount: (
      db.prepare("SELECT COUNT(*) AS count FROM schema_migrations").get() as { count: number }
    ).count,
    configurationCount: (
      db.prepare("SELECT COUNT(*) AS count FROM configurations").get() as { count: number }
    ).count,
    reservationCount: (
      db.prepare("SELECT COUNT(*) AS count FROM reservations").get() as { count: number }
    ).count,
    mobileReservationPersisted: Boolean(
      db.prepare("SELECT id FROM reservations WHERE id = ?").get(reservationId)
    ),
    apiReservationPersisted: Boolean(
      db.prepare("SELECT id FROM reservations WHERE id = ?").get(String(first.body.id))
    )
  };
  db.close();
  assert.equal(databaseEvidence.integrityCheck, "ok");
  assert.equal(databaseEvidence.migrationCount, 1);
  assert.equal(databaseEvidence.mobileReservationPersisted, true);
  assert.equal(databaseEvidence.apiReservationPersisted, true);

  const declaredStates = stateEvidence[0]!.declaredStates as string[];
  const observedStates = new Set<string>();
  for (const evidence of stateEvidence) {
    for (const state of (evidence.stateHistory as string[] | undefined) ?? []) {
      observedStates.add(state);
    }
  }
  for (const journey of mobileJourney) {
    for (const state of (journey.stateHistory as string[] | undefined) ?? []) {
      observedStates.add(state);
    }
  }
  const missingStates = declaredStates.filter((state) => !observedStates.has(state));
  assert.deepEqual(missingStates, []);
  assert.equal(mobileJourney.length, 10);
  assert.equal(new URL(mobileJourney.at(-1)!.url as string).pathname, "/receipt");

  const critical = Object.values(accessibility)
    .flat()
    .filter((finding) => finding.impact === "critical").length;
  const serious = Object.values(accessibility)
    .flat()
    .filter((finding) => finding.impact === "serious").length;
  const receipt = {
    schema_version: "visionmcp.live_app_acceptance.v1",
    origin,
    completed_at: new Date().toISOString(),
    passed: true,
    viewport_contracts: {
      desktop: { width: 1440, height: 900, device_scale_factor: 2 },
      mobile: { width: 390, height: 844, device_scale_factor: 3 }
    },
    routes: ["/", "/technology", "/configurator", "/reserve", "/receipt"],
    declared_states: declaredStates,
    observed_states: [...observedStates].sort(),
    missing_states: missingStates,
    state_evidence: stateEvidence,
    keyboard_first_eight: stateEvidence.find((item) => item.label === "keyboard-first-eight"),
    mobile_journey: mobileJourney,
    real_3d: {
      desktop_hero_requested: networkRecords.some(
        (item) => item.label === "desktop" && String(item.url).endsWith("nocturne-one-hero.glb")
      ),
      mobile_lod_requested: networkRecords.some(
        (item) => item.label === "mobile" && String(item.url).endsWith("nocturne-one-low.glb")
      ),
      material_variant_persisted: true,
      exploded_selection: "glass_core",
      exploded_semantic_end_state_deterministic: true,
      exploded_temporal_frame_sha256: explodedFrameHashes,
      exploded_pixel_hash_claim:
        "not asserted because the live camera easing loop is still converging; Blender frame 120 is the fixed determinism authority",
      first_real_frame_ms: firstRealFrameMs
    },
    fallback_and_recovery: {
      reduced_motion: true,
      no_webgl: "3d_unavailable",
      slow_network: "poster-first then 3d_ready",
      offline_retry: true,
      transient_503_recovery: true
    },
    api: {
      statuses: {
        unauthorized: unauthorized.status,
        forbidden: forbidden.status,
        invalid: invalid.status,
        transient: transient.status,
        first: first.status,
        replay: replay.status,
        conflict: conflict.status,
        own_actor: own.status,
        cross_actor: crossActor.status
      },
      replay_same_reservation: replay.body.id === first.body.id,
      latency_sample_count: latencySamples.length,
      latency_median_ms: percentile(latencySamples, 0.5),
      latency_p95_ms: percentile(latencySamples, 0.95),
      latency_max_ms: Math.max(...latencySamples)
    },
    database: databaseEvidence,
    accessibility: {
      scanner: "deterministic semantic runtime scan v1",
      routes: accessibility,
      critical_violation_count: critical,
      serious_violation_count: serious
    },
    performance: {
      desktop: desktopMetrics,
      mobile: mobileMetrics,
      limitation:
        "Frame samples measure deterministic local render submission on this host, not population GPU performance."
    },
    console: consoleRecords,
    console_error_count: consoleRecords.filter(
      (item) => item.type === "error" || item.type === "pageerror"
    ).length,
    network: networkRecords,
    failed_requests: failedRequests,
    expected_failed_request_count:
      failedRequests.filter((item) => String(item.failure).includes("ERR_INTERNET_DISCONNECTED")).length,
    screenshots: screenshotRecords,
    traces: {
      desktop: desktopTrace,
      mobile: mobileTrace
    }
  };
  const canonical = JSON.stringify(receipt, Object.keys(receipt).sort());
  const finalReceipt = { ...receipt, receipt_sha256: digest(canonical) };
  const receiptPath = path.join(artifactRoot, "app-receipt.json");
  await writeFile(receiptPath, JSON.stringify(finalReceipt, null, 2) + "\n");
  await writeFile(
    path.join(artifactRoot, "logs", "browser-console-network.json"),
    JSON.stringify(
      { console: consoleRecords, network: networkRecords, failedRequests },
      null,
      2
    ) + "\n"
  );
  const hashes = await Promise.all(
    [desktopTrace, mobileTrace].map(async (filename) => ({
      path: filename,
      bytes: (await readFile(filename)).length,
      sha256: digest(await readFile(filename))
    }))
  );
  console.log(
    JSON.stringify({
      passed: true,
      receipt: receiptPath,
      receipt_sha256: finalReceipt.receipt_sha256,
      routes: finalReceipt.routes.length,
      states: finalReceipt.observed_states.length,
      mobile_steps: mobileJourney.length,
      accessibility: { critical, serious },
      desktop_frame_p95_ms: percentile(desktopFrames, 0.95),
      mobile_frame_p95_ms: percentile(mobileFrames, 0.95),
      api_p95_ms: percentile(latencySamples, 0.95),
      traces: hashes
    })
  );
} finally {
  await browser.close();
}
