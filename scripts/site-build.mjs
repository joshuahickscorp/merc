import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';

const root = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..');
const pageNames = ['web/index.html', 'web/admin.html'];
const pages = new Map();
for (const name of pageNames) {
  const html = fs.readFileSync(path.join(root, name), 'utf8');
  pages.set(name, html);
  if (!html.includes('<!doctype html>') || !html.includes('computexchange')) {
    throw new Error(`${name}: invalid static page`);
  }
  if (/\b\d+\s+(?:passed|checks)\b/i.test(html)) {
    throw new Error(`${name}: static proof count is forbidden`);
  }
  if (/\sstyle\s*=/i.test(html)) {
    throw new Error(`${name}: inline style attributes are forbidden by the hash-only CSP`);
  }
}

const publicHTML = pages.get('web/index.html');
for (const phrase of [
  'private test canary',
  'no real payments',
  'approved accounts only',
  'apple metal suppliers only',
  'embed and batch_infer only',
  'capacity and latency are limited',
]) {
  if (!publicHTML.toLowerCase().includes(phrase)) {
    throw new Error(`web/index.html: missing mandatory canary disclosure: ${phrase}`);
  }
}
if (!/<label\b[^>]*\bfor=["']alpha-role["'][^>]*>/i.test(publicHTML)) {
  throw new Error('web/index.html: alpha role control requires an explicit label');
}
if (!publicHTML.includes(':focus-visible')) {
  throw new Error('web/index.html: visible keyboard focus styling is required');
}
if (!publicHTML.includes('prefers-reduced-motion:reduce')) {
  throw new Error('web/index.html: reduced-motion behavior is required');
}
if (/\.site\s*\{[^}]*display\s*:\s*none/is.test(publicHTML) || !publicHTML.includes('@media (max-width:899px)')) {
  throw new Error('web/index.html: the full site must remain usable at mobile widths');
}

const adminHTML = pages.get('web/admin.html');
if (/\b(?:localStorage|sessionStorage)\b/.test(adminHTML)) {
  throw new Error('web/admin.html: operator bearer credentials must remain memory-only');
}

function cssColor(html, variable) {
  const match = html.match(new RegExp(`--${variable}:\\s*(#[0-9a-f]{6})`, 'i'));
  if (!match) throw new Error(`web/index.html: missing --${variable} color`);
  return match[1];
}

function luminance(hex) {
  const channels = [1, 3, 5].map(index => Number.parseInt(hex.slice(index, index + 2), 16) / 255);
  const linear = channels.map(value => value <= 0.03928 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4);
  return 0.2126 * linear[0] + 0.7152 * linear[1] + 0.0722 * linear[2];
}

function contrast(a, b) {
  const [bright, dark] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (bright + 0.05) / (dark + 0.05);
}

const labelContrast = contrast(cssColor(publicHTML, 'ash'), cssColor(publicHTML, 'bg'));
if (labelContrast < 4.5) {
  throw new Error(`web/index.html: label contrast ${labelContrast.toFixed(2)} is below WCAG AA 4.5:1`);
}

const caddy = fs.readFileSync(path.join(root, 'Caddyfile'), 'utf8');
for (const header of [
  'Strict-Transport-Security',
  'Content-Security-Policy',
  'Permissions-Policy',
  'Cross-Origin-Opener-Policy',
  'X-Content-Type-Options',
  'X-Frame-Options',
  'Referrer-Policy',
]) {
  if (!caddy.includes(header)) throw new Error(`Caddyfile: missing required security header ${header}`);
}
if (!/@metrics\s+path\s+\/metrics[\s\S]*?respond\s+@metrics\s+404/.test(caddy)) {
  throw new Error('Caddyfile: public /metrics endpoint must fail closed');
}
if (!/^\s*-Server\s*$/m.test(caddy)) {
  throw new Error('Caddyfile: reverse-proxy server identity must be removed');
}
if (!/header\s+@html\s+Cache-Control\s+"no-store"/.test(caddy)) {
  throw new Error('Caddyfile: public and operator HTML must not be stored');
}
if (/['"]unsafe-(?:inline|eval)['"]/.test(caddy)) {
  throw new Error('Caddyfile: CSP must not allow unsafe inline or evaluated code');
}
const cspMatch = caddy.match(/Content-Security-Policy\s+"([^"]+)"/);
if (!cspMatch) throw new Error('Caddyfile: Content-Security-Policy value is missing');
const csp = new Map(cspMatch[1].split(';').map(part => {
  const tokens = part.trim().split(/\s+/);
  return [tokens.shift(), tokens];
}));
for (const [directive, source] of [
  ['default-src', "'self'"],
  ['base-uri', "'none'"],
  ['object-src', "'none'"],
  ['frame-ancestors', "'none'"],
  ['form-action', "'self'"],
]) {
  if (!csp.get(directive)?.includes(source)) {
    throw new Error(`Caddyfile: CSP requires ${directive} ${source}`);
  }
}

const inlineHashes = [];
for (const [name, html] of pages) {
  for (const tag of ['style', 'script']) {
    const blocks = html.matchAll(new RegExp(`<${tag}(?:\\s[^>]*)?>([\\s\\S]*?)<\\/${tag}>`, 'gi'));
    for (const block of blocks) {
      const digest = crypto.createHash('sha256').update(block[1]).digest('base64');
      inlineHashes.push({name, tag, value: `sha256-${digest}`});
    }
  }
}
for (const hash of inlineHashes) {
  const directive = `${hash.tag}-src`;
  if (!csp.get(directive)?.includes(`'${hash.value}'`)) {
    throw new Error(`Caddyfile: ${directive} is stale for ${hash.name}: ${hash.value}`);
  }
}

console.log(`site-build: public/operator pages, AA contrast (${labelContrast.toFixed(2)}:1), and hash-bound security headers validated`);
await import('./validate-observability.mjs');
