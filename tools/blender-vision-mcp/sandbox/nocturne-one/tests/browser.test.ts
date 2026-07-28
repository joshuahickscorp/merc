import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync, statSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import net from "node:net";
import { spawn } from "node:child_process";
import { chromium, type Page } from "playwright";

async function freePort(): Promise<number> {
  const server = net.createServer();
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  assert.ok(address && typeof address === "object");
  const port = address.port;
  await new Promise<void>((resolve, reject) =>
    server.close((error) => (error ? reject(error) : resolve()))
  );
  return port;
}

async function waitForHealth(origin: string): Promise<void> {
  const deadline = Date.now() + 20_000;
  while (Date.now() < deadline) {
    try {
      if ((await fetch(`${origin}/api/health`)).ok) return;
    } catch {
      // The process is still starting.
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error("Browser test server did not become healthy.");
}

async function accessibilityFindings(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const findings: string[] = [];
    if (!document.documentElement.lang) findings.push("html-lang");
    if (document.querySelectorAll("main").length !== 1) findings.push("main");
    if (document.querySelectorAll("h1").length !== 1) findings.push("h1");
    if (!document.querySelector('a[href="#main"]')) findings.push("skip-link");
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
      if (!label?.trim()) findings.push(`name:${element.tagName}`);
    }
    for (const image of document.querySelectorAll("img")) {
      if (!image.hasAttribute("alt")) findings.push("image-alt");
    }
    return findings;
  });
}

function frameMetrics(values: number[]): { medianFps: number; p95Ms: number } {
  const ordered = [...values].sort((first, second) => first - second);
  const median = ordered[Math.floor(ordered.length / 2)]!;
  const p95 = ordered[Math.min(ordered.length - 1, Math.ceil(ordered.length * 0.95) - 1)]!;
  return { medianFps: 1000 / median, p95Ms: p95 };
}

const workspace = mkdtempSync(path.join(tmpdir(), "nocturne-browser-test-"));
const port = await freePort();
const origin = `http://127.0.0.1:${port}`;
const requestedMemorySeconds = Math.max(
  0,
  Number(process.env.NOCTURNE_MEMORY_SECONDS ?? "0")
);
const server = spawn(
  process.execPath,
  ["--import", "tsx", "src/server/index.ts"],
  {
    cwd: process.cwd(),
    env: {
      ...process.env,
      HOST: "127.0.0.1",
      PORT: String(port),
      DATABASE_PATH: path.join(workspace, "browser.sqlite3"),
      NODE_ENV: "test"
    },
    stdio: ["ignore", "pipe", "pipe"]
  }
);
let serverLog = "";
server.stdout.on("data", (chunk) => (serverLog += String(chunk)));
server.stderr.on("data", (chunk) => (serverLog += String(chunk)));

try {
  await waitForHealth(origin);
  const browser = await chromium.launch({
    channel: "chrome",
    headless: true,
    args: [
      "--disable-background-networking",
      "--enable-precise-memory-info",
      "--js-flags=--expose-gc"
    ]
  });
  let desktopFrames = { medianFps: 0, p95Ms: Number.POSITIVE_INFINITY };
  let mobileFrames = { medianFps: 0, p95Ms: Number.POSITIVE_INFINITY };
  let memoryGrowthBytes = 0;
  let memorySampleCount = 0;
  try {
    const context = await browser.newContext({
      viewport: { width: 1280, height: 800 },
      colorScheme: "dark",
      reducedMotion: "no-preference"
    });
    const page = await context.newPage();
    const routes = ["/", "/technology", "/configurator", "/reserve", "/receipt"];
    for (const route of routes) {
      await page.goto(`${origin}${route}`, { waitUntil: "networkidle" });
      await page.waitForFunction(() => Boolean(window.__NOCTURNE__));
      assert.equal(await page.locator("main").count(), 1, `${route} main`);
      assert.equal(await page.locator("h1").count(), 1, `${route} h1`);
      assert.deepEqual(await accessibilityFindings(page), [], `${route} accessibility`);
      assert.equal(
        await page.evaluate(
          () =>
            document.documentElement.scrollWidth >
            document.documentElement.clientWidth + 1
        ),
        false,
        `${route} overflow`
      );
    }

    await page.goto(`${origin}/`, { waitUntil: "networkidle" });
    const first = await page.evaluate(() => ({
      poster: window.__NOCTURNE__.posterVisible,
      glb: window.__NOCTURNE__.glbRequested,
      state: window.__NOCTURNE__.state
    }));
    assert.deepEqual(
      { poster: first.poster, glb: first.glb },
      { poster: true, glb: false }
    );
    const keyboardTargets: string[] = [];
    for (let index = 0; index < 8; index += 1) {
      await page.keyboard.press("Tab");
      keyboardTargets.push(
        await page.evaluate(
          () => document.activeElement?.id || document.activeElement?.tagName || ""
        )
      );
    }
    assert.ok(keyboardTargets.includes("enter-3d"), JSON.stringify(keyboardTargets));
    assert.ok(new Set(keyboardTargets).size >= 3, JSON.stringify(keyboardTargets));
    assert.equal(await page.locator("#product-canvas").getAttribute("tabindex"), "-1");
    await page.locator("#enter-3d").click();
    await page.waitForFunction(
      () => window.__NOCTURNE__.state === "3d_ready",
      undefined,
      { timeout: 20_000 }
    );
    assert.equal(await page.locator("#product-canvas").getAttribute("tabindex"), "0");
    const frames = await page.evaluate(() =>
      window.__NOCTURNE__.sampleFrames(120)
    );
    assert.equal(frames.length, 120);
    assert.ok(frames.every((value) => Number.isFinite(value) && value > 0));
    desktopFrames = frameMetrics(frames);
    assert.ok(
      desktopFrames.medianFps >= 55,
      `desktop median frame budget: ${JSON.stringify(desktopFrames)}`
    );
    assert.ok(
      desktopFrames.p95Ms <= 24,
      `desktop p95 frame budget: ${JSON.stringify(desktopFrames)}`
    );
    const screenshot = path.join(workspace, "home-3d.png");
    await page.screenshot({ path: screenshot, fullPage: true });
    assert.ok(statSync(screenshot).size > 20_000, "visual screenshot has content");

    await page.goto(`${origin}/configurator`, { waitUntil: "networkidle" });
    await page.selectOption("#variant", "ember");
    await page.locator("#light").fill("72");
    await page.locator("#light").dispatchEvent("input");
    await page.locator("#orientation").fill("18");
    await page.locator("#orientation").dispatchEvent("input");
    await page.selectOption("#accessory", "braided-cable");
    const configured = await page.evaluate(() => ({
      app: window.__NOCTURNE__.config,
      scene: window.__NOCTURNE__.sceneConfig
    }));
    assert.deepEqual(configured.app, configured.scene);
    await page.reload({ waitUntil: "networkidle" });
    assert.deepEqual(
      await page.evaluate(() => window.__NOCTURNE__.config),
      configured.app
    );

    await page.goto(`${origin}/technology`, { waitUntil: "networkidle" });
    await page.selectOption("#part-selector", "glass_core");
    assert.equal(
      await page.evaluate(() => window.__NOCTURNE__.selectedPart),
      "glass_core"
    );

    const reduced = await browser.newContext({ reducedMotion: "reduce" });
    const reducedPage = await reduced.newPage();
    await reducedPage.goto(`${origin}/`, { waitUntil: "networkidle" });
    assert.deepEqual(
      await reducedPage.evaluate(() => ({
        reduced: window.__NOCTURNE__.reducedMotion,
        animation: window.__NOCTURNE__.animationEnabled
      })),
      { reduced: true, animation: false }
    );
    await reduced.close();

    const fallback = await browser.newContext();
    await fallback.addInitScript(() => {
      const prototype = HTMLCanvasElement.prototype as unknown as {
        getContext: (
          this: HTMLCanvasElement,
          type: string,
          ...args: unknown[]
        ) => RenderingContext | null;
      };
      const original = prototype.getContext;
      prototype.getContext = function (
        this: HTMLCanvasElement,
        type: string,
        ...args: unknown[]
      ) {
        if (type.startsWith("webgl")) return null;
        return original.call(this, type, ...args);
      };
    });
    const fallbackPage = await fallback.newPage();
    await fallbackPage.goto(`${origin}/`, { waitUntil: "networkidle" });
    await fallbackPage.locator("#enter-3d").click();
    await fallbackPage.waitForFunction(
      () => window.__NOCTURNE__.state === "3d_unavailable"
    );
    assert.equal(await fallbackPage.locator("main").count(), 1);
    await fallback.close();

    const mobile = await browser.newContext({
      viewport: { width: 390, height: 844 },
      isMobile: true,
      hasTouch: true,
      deviceScaleFactor: 2,
      colorScheme: "dark"
    });
    const mobilePage = await mobile.newPage();
    await mobilePage.goto(`${origin}/`, { waitUntil: "networkidle" });
    await mobilePage.locator("#enter-3d").tap();
    await mobilePage.waitForFunction(
      () => window.__NOCTURNE__.state === "3d_ready",
      undefined,
      { timeout: 20_000 }
    );
    const mobileSamples = await mobilePage.evaluate(() =>
      window.__NOCTURNE__.sampleFrames(120)
    );
    mobileFrames = frameMetrics(mobileSamples);
    assert.ok(
      mobileFrames.medianFps >= 40,
      `mobile median frame budget: ${JSON.stringify(mobileFrames)}`
    );
    assert.ok(
      mobileFrames.p95Ms <= 35,
      `mobile p95 frame budget: ${JSON.stringify(mobileFrames)}`
    );
    assert.equal(
      await mobilePage.evaluate(
        () =>
          document.documentElement.scrollWidth >
          document.documentElement.clientWidth + 1
      ),
      false,
      "mobile horizontal overflow"
    );
    await mobile.close();

    await page.goto(`${origin}/reserve`, { waitUntil: "networkidle" });
    await page.locator("#reserve-email").fill("invalid");
    await page.locator("#reserve-submit").click();
    await page.waitForFunction(
      () => window.__NOCTURNE__.state === "api_validation_error"
    );
    await page.evaluate(() =>
      window.__NOCTURNE__.setTestCondition("api_transient_error")
    );
    await page.locator("#reserve-email").fill("browser@example.invalid");
    await page.locator("#reserve-submit").click();
    await page.waitForFunction(
      () => window.__NOCTURNE__.state === "api_transient_error"
    );
    await page.evaluate(() => window.__NOCTURNE__.setTestCondition(null));
    await page.locator("#reserve-submit").click();
    await page.waitForFunction(
      () => window.__NOCTURNE__.state === "successful_reservation"
    );
    await page.goto(`${origin}/receipt`, { waitUntil: "networkidle" });
    await page.waitForFunction(
      () => window.__NOCTURNE__.state === "successful_reservation"
    );
    assert.match(
      await page.locator("#reservation-status").innerText(),
      /confirmed/i
    );

    if (requestedMemorySeconds > 0) {
      await page.goto(`${origin}/`, { waitUntil: "networkidle" });
      await page.locator("#enter-3d").click();
      await page.waitForFunction(
        () => window.__NOCTURNE__.state === "3d_ready",
        undefined,
        { timeout: 20_000 }
      );
      const samples: number[] = [];
      const started = Date.now();
      while (Date.now() - started < requestedMemorySeconds * 1000) {
        samples.push(
          await page.evaluate(() => {
            const next = (window.__NOCTURNE__.config.orientation + 1) % 45;
            window.__NOCTURNE__.setConfiguration({ orientation: next });
            (
              globalThis as typeof globalThis & {
                gc?: () => void;
              }
            ).gc?.();
            return (
              performance as Performance & {
                memory?: { usedJSHeapSize: number };
              }
            ).memory?.usedJSHeapSize ?? 0;
          })
        );
        await page.waitForTimeout(5_000);
      }
      memorySampleCount = samples.length;
      memoryGrowthBytes =
        samples.length >= 2 ? Math.max(0, samples.at(-1)! - samples[0]!) : 0;
      assert.ok(
        memoryGrowthBytes <= 16_777_216,
        `five-minute memory budget: ${memoryGrowthBytes} bytes`
      );
    }

    const html = readFileSync(path.join(process.cwd(), "dist", "index.html"), "utf8");
    assert.match(html, /NOCTURNE\/ONE/);
    await context.close();
  } finally {
    await browser.close();
  }
  console.log(
    JSON.stringify({
      passed: true,
      routes: 5,
      renderer: "3d_ready",
      accessibility_findings: 0,
      visual_screenshot_bytes: statSync(path.join(workspace, "home-3d.png")).size,
      desktop_frames: desktopFrames,
      mobile_frames: mobileFrames,
      memory_duration_seconds: requestedMemorySeconds,
      memory_growth_bytes: memoryGrowthBytes,
      memory_sample_count: memorySampleCount,
      no_webgl: "3d_unavailable",
      reduced_motion: true,
      reservation: "confirmed"
    })
  );
} catch (error) {
  console.error(serverLog);
  throw error;
} finally {
  server.kill("SIGTERM");
  await new Promise((resolve) => setTimeout(resolve, 150));
  rmSync(workspace, { recursive: true, force: true });
}
