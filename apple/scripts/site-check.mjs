// SPDX-License-Identifier: BUSL-1.1
// Copyright © 2026 Zyvor AI Labs Private Limited
// Serve _site/ and check it: loads, no console errors, no horizontal scroll at phone width, light and dark, demo boots.
//   node scripts/site-check.mjs [--shots DIR]
import { chromium } from 'playwright';
import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { extname, join, normalize } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = join(fileURLToPath(import.meta.url), '..', '..', '_site');
const types = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png', '.jpg': 'image/jpeg', '.xml': 'application/xml', '.txt': 'text/plain' };
const server = createServer(async (req, res) => {
  let p = normalize(req.url.split('?')[0]); if (p.endsWith('/')) p += 'index.html';
  try { const d = await readFile(join(root, p)); res.writeHead(200, { 'content-type': types[extname(p)] || 'application/octet-stream' }); res.end(d); }
  catch { res.writeHead(404); res.end('nf'); }
}).listen(0);
await new Promise(r => server.once('listening', r));
const base = `http://127.0.0.1:${server.address().port}/`;
const shotsDir = process.argv.includes('--shots') ? process.argv[process.argv.indexOf('--shots') + 1] : null;
const browser = await chromium.launch();
const problems = [];
for (const [name, size, scheme] of [['desktop-light', { width: 1440, height: 900 }, 'light'], ['desktop-dark', { width: 1440, height: 900 }, 'dark'], ['phone', { width: 390, height: 844 }, 'light']]) {
  const ctx = await browser.newContext({ viewport: size, colorScheme: scheme });
  const page = await ctx.newPage();
  const errs = [];
  page.on('pageerror', e => errs.push(e.message));
  page.on('console', m => { if (m.type() === 'error') errs.push(m.text()); });
  page.on('requestfailed', r => errs.push('request failed: ' + r.url()));
  const resp = await page.goto(base, { waitUntil: 'load' });
  if (!resp || resp.status() !== 200) problems.push(`${name}: status ${resp && resp.status()}`);
  const h1 = await page.textContent('h1');
  if (!/scheduled/i.test(h1)) problems.push(`${name}: hero headline missing`);
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1);
  if (overflow) problems.push(`${name}: horizontal scroll`);
  const imgs = await page.evaluate(() => [...document.images].filter(i => i.complete && i.naturalWidth === 0).map(i => i.src));
  if (imgs.length) problems.push(`${name}: broken images ${imgs.join(', ')}`);
  if (name === 'desktop-light') {
    await page.click('[role=tab][data-variant=dark]');
    if (await page.locator('[data-variant=dark]:not([role=tab])').first().isHidden()) problems.push('screenshot tabs do not switch');
    const hasOg = await page.getAttribute('meta[property="og:image"]', 'content');
    if (!hasOg) problems.push('og:image missing');
  }
  await page.evaluate(async () => { for (let y = 0; y < document.body.scrollHeight; y += 500) { window.scrollTo(0, y); await new Promise(r => setTimeout(r, 60)); } window.scrollTo(0, 0); });
  if (shotsDir) await page.screenshot({ path: join(shotsDir, `site-${name}.png`), fullPage: true });
  if (errs.length) problems.push(`${name}: ${errs.join(' | ')}`);
  await ctx.close();
}
// the demo boots and shows the simulated-fleet banner
{
  const page = await browser.newPage();
  const errs = []; page.on('pageerror', e => errs.push(e.message));
  await page.goto(base + 'demo/');
  await page.click('#demo-start');
  await page.waitForSelector('#gpu-cards .gpu-card');
  if (await page.locator('#demo-banner').isHidden()) problems.push('demo banner (simulated fleet) not shown');
  if (errs.length) problems.push('demo: ' + errs.join(' | '));
}
await browser.close(); server.close();
if (problems.length) { console.error('site-check FAILED:\n  ' + problems.join('\n  ')); process.exit(1); }
console.log('site-check ok');
