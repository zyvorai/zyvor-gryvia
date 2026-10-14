// SPDX-License-Identifier: BUSL-1.1
// Copyright © 2026 Zyvor AI Labs Private Limited
// Capture marketing screenshots from the dashboard's demo mode (simulated fleet) into site/img/.
//   node scripts/shots.mjs            (needs `npm install` for playwright)
import { chromium } from 'playwright';
import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { extname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = join(fileURLToPath(import.meta.url), '..', '..');
const types = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' };
const server = createServer(async (req, res) => {
  const path = req.url.split('?')[0].replace(/^\/$/, '/index.html');
  try { const data = await readFile(join(root, 'web', path)); res.writeHead(200, { 'content-type': types[extname(path)] || 'text/plain' }); res.end(data); }
  catch { res.writeHead(404); res.end('not found'); }
}).listen(0);
await new Promise(r => server.once('listening', r));
const base = `http://127.0.0.1:${server.address().port}/`;

const browser = await chromium.launch();
const out = join(root, 'site', 'img');
for (const theme of ['light', 'dark']) {
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, deviceScaleFactor: 1, colorScheme: theme, reducedMotion: 'reduce' });
  await ctx.addInitScript(t => { try { localStorage.setItem('gryvia-preferences', JSON.stringify({ theme: t, window: 'macos27', accent: 'blue', interval: 60000, welcomeHidden: true, templates: [] })); } catch {} }, theme);
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', e => errors.push(e.message));
  await page.goto(base + '#live');
  await page.click('#demo-start');
  await page.waitForSelector('#gpu-cards .gpu-card');
  await page.waitForTimeout(1500);                       // let the simulated utilisation settle
  const shot = async name => { await page.waitForTimeout(250); await page.screenshot({ path: join(out, `${name}-${theme}.jpg`), type: 'jpeg', quality: 86 }); };
  await shot('live');
  await page.click('[data-view="models"]'); await page.waitForSelector('#model-list [data-model]'); await shot('models');
  await page.click('[data-view="fleet"]'); await page.waitForSelector('#nodes .node-card'); await shot('fleet');
  await page.click('[data-view="training"]');
  await page.fill('#train-form [name="base"]', 'mlx-community/Llama-3.2-1B-Instruct-4bit');
  await page.click('#train-form button.primary');
  await page.waitForSelector('#train-chart svg'); await shot('training');
  if (errors.length) throw new Error(errors.join('\n'));
  await ctx.close();
}
await browser.close();
server.close();
console.log('wrote screenshots to site/img');
