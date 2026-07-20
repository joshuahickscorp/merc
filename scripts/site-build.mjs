import fs from 'node:fs';
import path from 'node:path';

const root = path.resolve(path.dirname(new URL(import.meta.url).pathname), '..');
for (const name of ['web/index.html', 'web/admin.html']) {
  const html = fs.readFileSync(path.join(root, name), 'utf8');
  if (!html.includes('<!doctype html>') || !html.includes('computexchange')) {
    throw new Error(`${name}: invalid static page`);
  }
  if (/\b\d+\s+(?:passed|checks)\b/i.test(html)) {
    throw new Error(`${name}: static proof count is forbidden`);
  }
}
console.log('site-build: public and operator pages validated');
