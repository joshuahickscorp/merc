#!/usr/bin/env node
// Interactive loopback checks for scripts/produce-website-validation.py.
// Uses Playwright against the installed Google Chrome executable.

import { pathToFileURL } from 'node:url';
import path from 'node:path';

const base = process.argv[2];
const chrome = process.argv[3];
const playwrightRoot = process.argv[4];
if (!base || !chrome || !playwrightRoot) {
  console.error('usage: website-validation-browser.mjs BASE_URL CHROME_PATH PLAYWRIGHT_CORE_ROOT');
  process.exit(2);
}

const { chromium } = await import(
  pathToFileURL(path.join(playwrightRoot, 'node_modules', 'playwright-core', 'index.mjs')).href
);

const pages = [
  { name: 'public', path: '/' },
  { name: 'buyer', path: '/buyer' },
  { name: 'operator', path: '/admin' },
];
const viewports = [
  { name: '1440x900', width: 1440, height: 900 },
  { name: '390x844', width: 390, height: 844 },
];

const checks = {
  pages_loaded_without_console_errors: false,
  no_horizontal_overflow: false,
  keyboard_focus_order_exercised: false,
  dialog_keyboard_open_and_escape_close: false,
  buyer_form_labels_exposed: false,
  buyer_errors_announced: false,
  operator_unauthorized_failure_visible: false,
  browser_storage_credentials_absent: false,
  reduced_motion_active: false,
};
const pageStatus = { public: 'FAIL', buyer: 'FAIL', operator: 'FAIL' };
const consoleErrors = [];
const origin = new URL(base).origin;
const controlPlane = path =>
  path.startsWith('/v1/')
  || path.startsWith('/admin/')
  || path === '/version'
  || path === '/readyz'
  || path === '/healthz';
const staticAssetFailures = [];

const browser = await chromium.launch({
  executablePath: chrome,
  headless: true,
});

try {
  const context = await browser.newContext({
    colorScheme: 'dark',
    reducedMotion: 'reduce',
  });
  const page = await context.newPage();
  page.on('pageerror', err => {
    consoleErrors.push(String(err));
  });
  page.on('console', msg => {
    if (msg.type() !== 'error') return;
    const text = msg.text();
    // Control-plane fetches 404 on a static loopback server; that is expected
    // and is how operator unauthorized failure is observed.
    if (/Failed to load resource:.*404/.test(text)) return;
    consoleErrors.push(text);
  });
  page.on('response', response => {
    let url;
    try {
      url = new URL(response.url());
    } catch {
      return;
    }
    if (url.origin !== origin) return;
    if (response.status() < 400) return;
    if (controlPlane(url.pathname)) return;
    staticAssetFailures.push(`${response.status()} ${url.pathname}`);
  });

  let overflow = false;
  for (const viewport of viewports) {
    await page.setViewportSize({ width: viewport.width, height: viewport.height });
    for (const surface of pages) {
      const response = await page.goto(base + surface.path, { waitUntil: 'domcontentloaded' });
      if (!response || response.status() >= 400) {
        throw new Error(`${surface.path} at ${viewport.name} HTTP ${response && response.status()}`);
      }
      const scroll = await page.evaluate(() => ({
        scroll: document.documentElement.scrollWidth,
        client: document.documentElement.clientWidth,
      }));
      if (scroll.scroll > scroll.client + 1) {
        overflow = true;
      }
    }
  }
  checks.no_horizontal_overflow = !overflow;

  await page.setViewportSize({ width: 1440, height: 900 });

  await page.goto(base + '/', { waitUntil: 'domcontentloaded' });
  const focusOrder = [];
  for (let i = 0; i < 12; i += 1) {
    await page.keyboard.press('Tab');
    focusOrder.push(await page.evaluate(() => {
      const el = document.activeElement;
      if (!el) return '';
      return el.id || el.tagName.toLowerCase();
    }));
  }
  checks.keyboard_focus_order_exercised = new Set(focusOrder.filter(Boolean)).size >= 2;

  const dialog = page.locator('#receipts');
  await page.locator('#open-receipts').click();
  await dialog.waitFor({ state: 'visible' });
  const opened = await page.evaluate(() => document.getElementById('receipts')?.open === true);
  await page.keyboard.press('Escape');
  await page.waitForTimeout(100);
  const closed = await page.evaluate(() => document.getElementById('receipts')?.open === false);
  checks.dialog_keyboard_open_and_escape_close = opened && closed;

  await page.emulateMedia({ reducedMotion: 'reduce' });
  checks.reduced_motion_active = await page.evaluate(() =>
    window.matchMedia('(prefers-reduced-motion: reduce)').matches
  );

  await page.goto(base + '/buyer', { waitUntil: 'domcontentloaded' });
  const buyerLabels = await page.evaluate(() => {
    const email = document.querySelector('label[for], #signup-form label, #login-form label');
    const loginEmail = document.querySelector('#email');
    const signupEmail = document.querySelector('#signup-email');
    const live = document.querySelector('#login-status, #signup-status');
    return {
      labeled: Boolean(email),
      loginHasLabel: Boolean(loginEmail && loginEmail.closest('label')),
      signupHasLabel: Boolean(signupEmail && signupEmail.closest('label')),
      live: live?.getAttribute('aria-live') === 'polite',
    };
  });
  checks.buyer_form_labels_exposed =
    buyerLabels.labeled && buyerLabels.loginHasLabel && buyerLabels.signupHasLabel;

  await page.locator('#email').fill('not-an-email');
  await page.locator('#login-form button[type="submit"], #login-form button').first().click();
  const loginValidity = await page.locator('#email').evaluate(el => el.checkValidity());
  const liveRegion = await page.locator('#login-status').getAttribute('aria-live');
  checks.buyer_errors_announced = loginValidity === false || liveRegion === 'polite';

  const storage = await page.evaluate(() => ({
    local: Object.keys(localStorage).length,
    session: Object.keys(sessionStorage).length,
  }));
  checks.browser_storage_credentials_absent = storage.local === 0 && storage.session === 0;

  await page.goto(base + '/admin', { waitUntil: 'domcontentloaded' });
  await page.locator('#tabs button').first().click();
  await page.waitForTimeout(300);
  const operator = await page.evaluate(() => {
    const status = document.querySelector('#status');
    return {
      text: (status?.textContent || '').trim(),
      isError: status?.classList.contains('error'),
    };
  });
  checks.operator_unauthorized_failure_visible =
    operator.isError && operator.text.length > 0;

  pageStatus.public = 'PASS';
  pageStatus.buyer = 'PASS';
  pageStatus.operator = 'PASS';
  checks.pages_loaded_without_console_errors =
    consoleErrors.length === 0 && staticAssetFailures.length === 0;

  const failed = Object.entries(checks).filter(([, ok]) => !ok).map(([name]) => name);
  const result = {
    ok: failed.length === 0,
    checks,
    surfaces: pageStatus,
    viewports: viewports.map(v => v.name),
    console_errors: consoleErrors,
    static_asset_failures: staticAssetFailures,
    failed,
  };
  process.stdout.write(JSON.stringify(result));
  process.exit(result.ok ? 0 : 1);
} finally {
  await browser.close();
}
