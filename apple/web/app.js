// SPDX-License-Identifier: BUSL-1.1
// Copyright © 2026 Zyvor AI Labs Private Limited
'use strict';

/* ---------- helpers ---------- */
const $ = s => document.querySelector(s);
const $$ = s => [...document.querySelectorAll(s)];
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[c]));
const reEsc = s => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
const ACCENTS = {blue: '#0088ff', purple: '#a550e0', pink: '#ff2d78', red: '#ff383c', orange: '#ff8d28', yellow: '#f5b800', green: '#30d158', graphite: '#8c8c93'};
const VIEWS = ['live', 'overview', 'fleet', 'jobs', 'timeline', 'models', 'playground', 'datasets', 'training', 'workflows', 'deployments', 'tenants', 'alerts', 'audit', 'settings', 'guide'];
const TITLES = {live: 'Live', overview: 'Overview', fleet: 'Mac fleet', jobs: 'Workloads', timeline: 'Timeline', models: 'Model hub', playground: 'Playground', datasets: 'Datasets', training: 'Training', workflows: 'Workflows', deployments: 'Deployments', tenants: 'Tenants & quotas', alerts: 'Alerts', audit: 'Activity', settings: 'Settings', guide: 'Get started'};
const ACTIVE = ['running', 'cancelling'];
const DEMO_JOB = {runtime: 'demo', model: 'Hello Gryvia', memoryGiB: 1, priority: 50, mode: 'exclusive', retries: 0, timeoutSeconds: 60, args: ['--seconds', '3'], labels: ['demo']};

/* ---------- state ---------- */
let idleHist = {}, runMetrics = {}, chartJob = null, results = [], suites = {}, compareSet = new Set(), activeModel = null, token = '', state = null, view = 'live', telemetry = {}, streaming = false, streamAbort = null, flowSig = '', view0 = 0, busy = false, paused = false, lastOk = 0, failures = 0;
let demo = null, timer = null, logJob = null, logTimer = null, logLines = [], samples = [], selected = new Set();
try { token = sessionStorage.getItem('gryvia.token') || ''; } catch {}

/* ---------- preferences (never contains the token) ---------- */
const PREF_KEY = 'gryvia-preferences';
let prefs = {welcomeHidden: false, theme: 'auto', window: 'macos27', accent: 'blue', interval: 5000, reduce: false, templates: []};
try { Object.assign(prefs, JSON.parse(localStorage.getItem(PREF_KEY) || '{}')); } catch {}
function savePrefs() { try { localStorage.setItem(PREF_KEY, JSON.stringify(prefs)); } catch {} applyPrefs(); }
function applyPrefs() {
  const r = document.documentElement;
  if (prefs.theme === 'auto') r.removeAttribute('data-theme'); else r.dataset.theme = prefs.theme;
  r.dataset.window = prefs.window;
  r.dataset.accent = prefs.accent;
  r.dataset.reduceTransparency = String(!!prefs.reduce);
  $('#set-theme').value = prefs.theme; $('#set-window').value = prefs.window;
  $('#set-interval').value = String(prefs.interval); $('#set-reduce').checked = !!prefs.reduce;
  $$('.swatch').forEach(b => b.setAttribute('aria-checked', String(b.dataset.accent === prefs.accent)));
  restartTimer();
}

/* ---------- API ---------- */
async function api(path, body) {
  if (demo) return demoApi(path, body);
  const r = await fetch('/api/' + path, {method: body ? 'POST' : 'GET', headers: {Authorization: 'Bearer ' + token, ...(body ? {'Content-Type': 'application/json'} : {})}, body: body ? JSON.stringify(body) : undefined});
  const data = await r.json();
  if (!r.ok) throw Error(data.error || 'Request failed');
  return data;
}
function hint(msg) {
  if (/Failed to fetch|NetworkError|Load failed/i.test(msg)) return 'Cannot reach the control plane. Is the server running and reachable from this browser?';
  if (/authentication required|unauthorized|401/i.test(msg)) return 'The server rejected this token. Use an admin or tenant token from credentials.env.';
  return msg;
}
function notice(s) { $('#notice').textContent = s; }
function toast(msg, kind) {
  const t = document.createElement('div');
  t.className = 'toast' + (kind === 'error' ? ' error' : ''); t.textContent = msg;
  $('#toasts').append(t); setTimeout(() => t.remove(), 3600);
}
const fail = e => { notice(hint(e.message)); };

/* ---------- demo mode: clearly simulated ---------- */
function startDemo() {
  const now = Date.now() / 1000;
  const spec = (model, runtime, mem, prio, labels = []) => ({model, runtime, memoryGiB: mem, priority: prio, retries: 0, timeoutSeconds: 3600, args: [], mode: 'exclusive', labels});
  const mk = (n, tenant, st, sp, node, age, started) => ({id: ('demo' + n + 'c0ffee').padEnd(32, '0'), tenant, state: st, created: now - age, started: started ? now - started : null, node, attempt: st === 'queued' ? 0 : 1, reason: st === 'queued' ? 'node memory reservation budget' : '', spec: sp, result: {}});
  demo = {
    nodes: [
      {id: 'studio-01', seen: now, drain: false, online: true, budgetGiB: 48, slots: 1, runtimes: ['mlx', 'llama', 'demo'], thermal: 'nominal', gpu: 'Apple M3 Ultra', host: 'studio-01.local', presence: {idleSeconds: 40, locked: false, onBattery: false, batteryPct: 100}, policy: {mode: 'idle', idleMinutes: 5}, gate: {allowed: false, reason: 'owner is active'}, gpuModel: 'Apple M3 Ultra', gpuCores: 80, totalGiB: 96, gpuUtilPct: 60, gpuInUseMiB: 21000},
      {id: 'mini-01', seen: now, drain: false, online: true, budgetGiB: 12, slots: 1, runtimes: ['mlx', 'coreml', 'demo'], thermal: 'fair', gpu: 'Apple M4 Pro', host: 'mini-01.local', gpuModel: 'Apple M4 Pro', gpuCores: 20, totalGiB: 24, gpuUtilPct: 40, gpuInUseMiB: 3500, presence: {idleSeconds: 1800, locked: false, onBattery: false, batteryPct: 100}, policy: {mode: 'idle', idleMinutes: 5}, gate: {allowed: true, reason: ''}},
      {id: 'air-01', seen: now - 200, drain: false, online: false, budgetGiB: 6, slots: 1, runtimes: ['coreml'], thermal: 'nominal', gpu: 'Apple M2', host: 'air-01.local', gpuModel: 'Apple M2', gpuCores: 10, totalGiB: 8, gpuUtilPct: 0, gpuInUseMiB: 0}],
    telemetry: {}, decisions: [{seq: 1, at: now - 90, job: 'demo3c0ffee', node: null, kind: 'wait', detail: 'node memory reservation budget'}, {seq: 2, at: now - 600, job: 'demo1c0ffee', node: 'studio-01', kind: 'admit', detail: 'admitted on studio-01'}],
    jobs: [
      mk(1, 'research', 'running', spec('Llama 3.1 8B eval', 'mlx', 24, 70, ['eval']), 'studio-01', 900, 600),
      mk(2, 'default', 'running', spec('Embeddings batch', 'coreml', 4, 50, ['nightly']), 'mini-01', 400, 240),
      mk(3, 'research', 'queued', spec('Fine-tune LoRA', 'mlx', 40, 60, ['train']), null, 320, 0),
      mk(4, 'default', 'succeeded', spec('CPU smoke test', 'demo', 1, 50), 'mini-01', 3000, 2900),
      mk(5, 'research', 'failed', spec('Whisper transcribe', 'mlx', 8, 40), 'studio-01', 2600, 2500)],
    tenants: [{id: 'default', quota: 32, weight: 1}, {id: 'research', quota: 64, weight: 2}],
    models: [{id: 'llama-3.2-1b-q4', runtime: 'llama', memoryGiB: 1.8, status: 'ready', quant: 'Q4_K_M', paramsB: 1.2, sizeBytes: 808e6, source: 'bartowski/Llama-3.2-1B-Instruct-GGUF/Llama-3.2-1B-Instruct-Q4_K_M.gguf', revision: 'main', node: 'mini-01', path: '/demo/models/Llama-3.2-1B-Q4_K_M.gguf', sha256: 'demo'.padEnd(64, '0'), endpoints: ['http://demo.invalid'], provenance: {verified: true, chainVerified: true, baseUnattested: false, chain: [{job: 'demodownload', kind: 'download', verified: true, signer: 'mini-01', why: '', at: now - 3000}]}}],
    results: [{at: now - 400, job: 'd1', model: 'llama-3.2-1b-q4', kind: 'benchmark', node: 'mini-01', body: {success: true, promptTokensPerSecond: 910.2, generationTokensPerSecond: 72.4}},
              {at: now - 300, job: 'd2', model: 'llama-3.2-1b-q4', kind: 'evaluate', node: 'mini-01', body: {success: true, passed: 4, total: 5, passRate: 0.8, cases: [{passed: true, prompt: 'What is 2+2?', output: '4'}, {passed: false, prompt: 'Capital of Peru?', output: 'Lima is'}]}}],
    suites: {}, datasets: [{id: 'support-chats', status: 'ready', rows: 4200, bytes: 6.2e6, node: 'studio-01', source: 'datasets/acme/support/train.jsonl', path: '/demo/support.jsonl'}], workflows: [],
    events: [{at: now - 60, kind: 'job.claimed', subject: 'demo1', detail: 'studio-01'}, {at: now - 600, kind: 'tenant.updated', subject: 'research', detail: ''}],
    usage: [{tenant: 'default', workerSeconds: 3120, reservedGiBSeconds: 12480}, {tenant: 'research', workerSeconds: 9100, reservedGiBSeconds: 120000}]};
  $('#demo-banner').hidden = false; $('#connect').hidden = true;
  samples = []; refresh();
}
function stopDemo() { demo = null; $('#demo-banner').hidden = true; $('#connect').hidden = false; state = null; render(); tick(); }
function demoApi(path, body) {
  const now = Date.now() / 1000, d = demo;
  // advance the simulation: queued jobs start when a node has room; running jobs finish after ~25 s
  d.jobs.forEach(j => { if (j.state === 'running' && j.started && now - j.started > 1500 && j.id.startsWith('demo9')) j.state = 'succeeded'; });
  d.jobs.filter(j => j.state === 'queued').forEach(j => {
    const n = d.nodes.find(n => n.online && !n.drain && n.runtimes.includes(j.spec.runtime) && d.jobs.filter(x => x.node === n.id && ACTIVE.includes(x.state)).reduce((s, x) => s + x.spec.memoryGiB, 0) + j.spec.memoryGiB <= n.budgetGiB);
    if (n) { Object.assign(j, {state: 'running', node: n.id, started: now, attempt: 1, reason: ''}); d.decisions.push({seq: Date.now() + Math.random(), at: now, job: j.id, node: n.id, kind: 'admit', detail: 'admitted ' + (j.spec.model || j.spec.runtime) + ' on ' + n.id}); }
  });
  d.nodes.forEach(n => {
    const busy = n.online && d.jobs.some(j => j.node === n.id && ACTIVE.includes(j.state));
    const target = !n.online ? 0 : busy ? 62 + 30 * Math.sin(now / 5 + n.id.length) : 4 + 5 * Math.random();
    n.gpuUtilPct = Math.max(0, Math.min(100, (n.gpuUtilPct ?? 0) * 0.6 + target * 0.4 + (n.online ? Math.random() * 6 - 3 : 0)));
    n.gpuInUseMiB = busy ? reservedOn(n.id, d.jobs) * 1024 * 0.8 + 900 : n.online ? 700 : 0;
    const arr = d.telemetry[n.id] = d.telemetry[n.id] || [];
    arr.push({at: now, util: n.gpuUtilPct, inUseGiB: n.gpuInUseMiB / 1024, reservedGiB: reservedOn(n.id, d.jobs)});
    if (arr.length > 90) arr.shift();
  });
  if (path === 'telemetry') return Promise.resolve(JSON.parse(JSON.stringify(d.telemetry)));
  if (path === 'state') return Promise.resolve(JSON.parse(JSON.stringify({jobs: d.jobs, nodes: d.nodes, tenants: d.tenants, models: d.models, events: d.events, usage: d.usage, decisions: d.decisions, datasets: d.datasets, workflows: d.workflows, time: now})));
  if (path === 'history') return Promise.resolve([]);
  if (path.startsWith('runs/')) {
    const steps = Array.from({length: 20}, (_, i) => (i + 1) * 5);
    return Promise.resolve({trainLoss: steps.map(s => [s, 2.6 * Math.exp(-s / 70) + 0.9 + Math.sin(s) * 0.03]), valLoss: steps.filter((_, i) => i % 4 === 3).map(s => [s, 2.7 * Math.exp(-s / 80) + 1.0]), tokensPerSec: [[100, 840]], peakGB: [[100, 3.4]]});
  }
  if (path === 'datasets/onboard') { d.datasets.push({id: body.id, status: 'ready', rows: 1000, bytes: 2e6, node: 'mini-01', source: 'datasets/' + body.source.repo}); return Promise.resolve({job: 'demo-ds'}); }
  if (path === 'datasets') { d.datasets.push({status: 'ready', ...body}); return Promise.resolve({ok: true}); }
  if (path === 'workflows') { d.workflows.unshift({id: 'demo-wf' + d.workflows.length, name: body.name, steps: body.steps.map(s => ({name: s.name, job: 'dj-' + s.name})), tenant: 'default'}); return Promise.resolve({id: 'demo-wf'}); }
  if (path === 'results') return Promise.resolve(JSON.parse(JSON.stringify(d.results)));
  if (path === 'suites') return Promise.resolve(JSON.parse(JSON.stringify(d.suites)));
  if (path === 'models/onboard') {
    d.models.push({id: body.id, runtime: body.runtime, memoryGiB: body.paramsB ? estimateGiB(body.paramsB, body.quant) : 8, status: 'onboarding', quant: body.quant, source: body.source.repo || body.source.url});
    setTimeout(() => { const m = d.models.find(x => x.id === body.id); if (m) Object.assign(m, {status: 'ready', path: '/demo/models/' + body.id + '.gguf', sizeBytes: 7e8, node: 'mini-01'}); }, 4000);
    return Promise.resolve({job: 'demo-download'});
  }
  if (path.startsWith('logs/')) return Promise.resolve([0, 1, 2].map(i => ({seq: i, at: now - 30 + i * 10, text: ['demo runtime started', 'warming model cache (simulated)', 'finished step ' + (i + 1)][i]})));
  if (path === 'jobs' && body.modelId && ['benchmark', 'evaluate'].includes(body.kind)) {
    setTimeout(() => d.results.unshift({at: Date.now() / 1000, job: 'dx', model: body.modelId, kind: body.kind, node: 'mini-01', body: body.kind === 'benchmark' ? {success: true, promptTokensPerSecond: 880 + Math.random() * 60, generationTokensPerSecond: 70 + Math.random() * 6} : {success: true, passed: 2, total: 2, passRate: 1, cases: []}}), 3000);
  }
  if (path === 'jobs') { d.decisions.push({seq: Date.now(), at: now, job: 'new', node: null, kind: 'queue', detail: 'queued ' + (body.model || body.runtime)}); d.jobs.unshift({id: ('demo9' + Math.random().toString(16).slice(2)).padEnd(32, '0').slice(0, 32), tenant: body.tenant || 'default', state: 'queued', created: now, started: null, node: null, attempt: 0, reason: '', spec: body, result: {}}); return Promise.resolve({id: d.jobs[0].id}); }
  if (path === 'cancel') { const j = d.jobs.find(j => j.id === body.id); if (j) j.state = j.state === 'queued' ? 'cancelled' : 'cancelling'; if (j && j.state === 'cancelling') setTimeout(() => { j.state = 'cancelled'; }, 1500); }
  if (path === 'drain') { const n = d.nodes.find(n => n.id === body.id); if (n) n.drain = body.drain; }
  if (path === 'priority') { const j = d.jobs.find(j => j.id === body.id); if (j) j.spec.priority = body.priority; }
  if (path === 'nodes/policy') { const n = d.nodes.find(n => n.id === body.id); if (n) { n.policy = {...body}; delete n.policy.id; n.gate = body.mode === 'paused' ? {allowed: false, reason: 'paused by the Mac owner'} : body.mode === 'idle' && n.presence && n.presence.idleSeconds < body.idleMinutes * 60 ? {allowed: false, reason: 'owner is active'} : {allowed: true, reason: ''}; } return Promise.resolve({policy: body}); }
  if (path === 'tenants') { const t = d.tenants.find(t => t.id === body.id); if (t) Object.assign(t, body); else d.tenants.push(body); }
  if (path === 'models') d.models.push(body);
  if (path === 'suites') d.suites[body.id] = body.cases;
  return Promise.resolve({ok: true});
}

/* ---------- rendering ---------- */
const badge = s => `<span class="badge ${esc(s)}">${esc(s)}</span>`;
const sum = (jobs, f) => jobs.reduce((s, j) => s + f(j), 0);
const reservedOn = (id, jobs) => sum(jobs.filter(j => j.node === id && ACTIVE.includes(j.state)), j => j.spec.memoryGiB);
const ago = ms => { const s = Math.max(0, Math.round((Date.now() - ms) / 1000)); return s < 2 ? 'just now' : s < 60 ? s + 's ago' : s < 3600 ? Math.round(s / 60) + 'm ago' : Math.round(s / 3600) + 'h ago'; };

function jobRow(j, withSelect) {
  const labels = (j.spec.labels || []).map(l => `<span class="label">${esc(l)}</span>`).join('');
  const act = [];
  act.push(`<button data-logs="${esc(j.id)}">Logs</button>`);
  if (ACTIVE.includes(j.state) || j.state === 'queued') act.push(`<button data-cancel="${esc(j.id)}">Cancel</button>`);
  if (j.state === 'queued') act.push(`<button data-bump="${esc(j.id)}" title="Raise priority by 10">Prio +10</button>`);
  act.push(`<button data-clone="${esc(j.id)}">Clone</button>`);
  if (['failed', 'cancelled', 'succeeded'].includes(j.state)) act.push(`<button data-retry="${esc(j.id)}">Retry</button>`);
  return `<tr><td>${withSelect ? `<input type="checkbox" data-select="${esc(j.id)}" ${selected.has(j.id) ? 'checked' : ''} aria-label="Select ${esc(j.id.slice(0, 8))}">` : ''}</td>
    <td><strong>${esc(j.spec.model || j.spec.runtime)}</strong> ${labels}<br><small>${esc(j.id.slice(0, 10))} · attempt ${j.attempt} · prio ${j.spec.priority}</small></td>
    <td>${esc(j.tenant)}</td><td>${badge(j.state)}</td><td>${j.spec.memoryGiB} GiB</td><td>${esc(j.node || j.reason || '—')}</td><td>${act.join('')}</td></tr>`;
}
function table(jobs, withSelect) {
  if (!jobs.length) return emptyArt('queue', 'No workloads match', 'Submit a native job, or press <strong>Run demo job</strong>.');
  return `<div class="table-wrap"><table><thead><tr><th></th><th>Workload</th><th>Tenant</th><th>State</th><th>Reservation</th><th>Placement</th><th>Actions</th></tr></thead><tbody>${jobs.map(j => jobRow(j, withSelect)).join('')}</tbody></table></div>`;
}
function filtered() {
  const q = $('#filter').value.toLowerCase(), st = $('#state-filter').value;
  return state.jobs.filter(j => (!st || j.state === st) && JSON.stringify([j.spec.model, j.spec.runtime, j.tenant, j.state, j.id, j.spec.labels]).toLowerCase().includes(q));
}
function spark(vals, cls, label, fmt) {
  if (vals.length < 2) return `<div class="spark ${cls}"><small>${label}</small><div class="muted">Collecting samples…</div></div>`;
  const max = Math.max(...vals, 1), w = 200, h = 56;
  const pts = vals.map((v, i) => `${(i / (vals.length - 1) * w).toFixed(1)},${(h - 4 - v / max * (h - 8)).toFixed(1)}`).join(' ');
  return `<div class="spark ${cls}"><small>${label} · now ${fmt(vals[vals.length - 1])}</small><svg viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" role="img" aria-label="${esc(label)} trend"><polyline points="${pts}"/></svg></div>`;
}
function alerts() {
  const out = [], {nodes, jobs, tenants, time} = state;
  nodes.forEach(n => {
    if (!n.online) out.push({level: 'error', title: `${n.id} is offline`, detail: 'No heartbeat in the last 30 seconds.'});
    if (['serious', 'critical'].includes(n.thermal)) out.push({level: 'error', title: `${n.id} thermal state ${n.thermal}`, detail: 'The scheduler will not place new work here.'});
    if (n.drain) out.push({level: 'info', title: `${n.id} is draining`, detail: 'No new work is admitted until you resume it.'});
  });
  jobs.filter(j => j.state === 'queued' && time - j.created > 300).forEach(j => out.push({level: 'warn', title: `${j.spec.model || j.spec.runtime} waiting ${Math.round((time - j.created) / 60)} min`, detail: j.reason || 'queued'}));
  jobs.filter(j => j.state === 'failed' && time - j.created < 86400).slice(0, 5).forEach(j => out.push({level: 'error', title: `${j.spec.model || j.spec.runtime} failed`, detail: `${j.id.slice(0, 10)} on ${j.node || 'unknown node'}`}));
  tenants.forEach(t => { const used = sum(jobs.filter(j => j.tenant === t.id && ACTIVE.includes(j.state)), j => j.spec.memoryGiB); if (used / t.quota >= 0.9) out.push({level: 'warn', title: `${t.id} at ${(used / t.quota * 100).toFixed(0)}% of quota`, detail: `${used} / ${t.quota} GiB reserved`}); });
  tenants.forEach(t => { const used = ((state.usage || []).find(u => u.tenant === t.id)?.reservedGiBSeconds || 0) / 3600; if (t.budget && used / t.budget >= 0.8) out.push({level: used >= t.budget ? 'error' : 'warn', title: `${t.id} used ${(used / t.budget * 100).toFixed(0)}% of its budget`, detail: `${used.toFixed(1)} of ${t.budget} reserved GiB-hours (estimate, not an invoice)`}); });
  return out;
}
function render() {
  $('#title').textContent = TITLES[view];
  if (!state) return;
  const {nodes, jobs, tenants, models, events} = state;
  const count = s => jobs.filter(j => j.state === s).length;
  $('#metrics').innerHTML = [['Macs online', nodes.filter(n => n.online).length, `of ${nodes.length} registered`], ['Running', count('running'), 'Active reservations'], ['Queued', count('queued'), 'Awaiting admission'], ['Completed', count('succeeded'), 'Successful workloads']]
    .map(([l, v, d]) => `<div class="metric"><span>${l}</span><strong>${v}</strong><small>${d}</small></div>`).join('');
  $('#capacity').innerHTML = nodes.map(n => { const u = reservedOn(n.id, jobs); return `<div class="capacity-row"><strong>${esc(n.id)}</strong><span>${u.toFixed(1)} / ${n.budgetGiB.toFixed(1)} GiB reserved</span></div><progress max="${n.budgetGiB}" value="${u}"></progress>`; }).join('') || '<div class="empty">Connect a Mac agent to see fleet capacity.</div>';
  $('#queue-health').innerHTML = ['queued', 'running', 'failed', 'cancelled'].map(s => `<div class="queue-line"><span>${s}</span><strong>${count(s)}</strong></div>`).join('');
  $('#trend').innerHTML = spark(samples.map(s => s.reservedGiB), 'reserved', 'Reserved GiB', v => v.toFixed(1)) + spark(samples.map(s => s.running), 'running', 'Running jobs', v => v) + spark(samples.map(s => s.queued), 'queued', 'Queued jobs', v => v);
  $('#trend-note').textContent = samples.length > 1 ? `· last ${samples.length} samples` : '';
  $('#recent').innerHTML = table(jobs.slice(0, 5), false);
  $('#job-table').innerHTML = table(filtered(), true);
  $('#bulk').hidden = selected.size === 0; $('#bulk-count').textContent = `${selected.size} selected`;
  nodes.forEach(n => { const h = idleHist[n.id] = idleHist[n.id] || []; const v = n.presence?.idleSeconds; if (typeof v === 'number' && (!h.length || h[h.length - 1].t !== state.time)) { h.push({t: state.time, v: n.presence?.locked ? 3600 : v}); if (h.length > 60) h.shift(); } });
  $('#nodes').innerHTML = nodes.map(n => {
    const pol = n.policy || {mode: 'always', idleMinutes: 5}, gate = n.gate || {allowed: true, reason: ''}, pr = n.presence || {};
    const presence = [pr.idleSeconds !== undefined ? (pr.locked ? 'screen locked' : 'idle ' + (pr.idleSeconds < 90 ? Math.round(pr.idleSeconds) + ' s' : Math.round(pr.idleSeconds / 60) + ' min')) : null,
      pr.onBattery ? `battery ${Math.round(pr.batteryPct ?? 0)}%` : (pr.batteryPct !== undefined ? 'plugged in' : null), pr.lowPowerMode ? 'Low Power Mode' : null, pr.micInUse ? 'mic in use' : null].filter(Boolean).join(' · ');
    return `<article class="node-card"><div class="section-head"><h3>${esc(n.id)}</h3>${badge(n.online ? 'online' : 'offline')}</div>
    <p>${esc(n.gpu || n.host || 'Native Mac worker')}</p><p>${n.budgetGiB} GiB budget · ${esc(n.thermal || 'unknown')} thermal · ${reservedOn(n.id, jobs).toFixed(1)} GiB reserved</p><p>${esc(n.runtimes.join(' · '))}</p>
    <p>${!n.online ? '<span class="badge">offline</span>' : gate.allowed ? '<span class="badge succeeded">accepting work</span>' : `<span class="badge queued">not accepting: ${esc(gate.reason)}</span>`} <span class="muted">${esc(presence)}</span></p>
    ${idleSpark(idleHist[n.id] || [])}
    <div class="policy"><small>When may this Mac run fleet work?</small>
      <div class="seg" role="group" aria-label="Work policy for ${esc(n.id)}" style="margin-top:6px">${[['always', 'Always'], ['idle', `When I'm away`], ['paused', 'Paused']].map(([m, l]) => `<button type="button" data-policy-set="${esc(n.id)}" data-mode="${m}" aria-pressed="${pol.mode === m}">${l}</button>`).join('')}</div></div>
    <div class="actions"><button data-node="${esc(n.id)}">Details</button><button data-drain="${esc(n.id)}" data-value="${!n.drain}">${n.drain ? 'Resume admission' : 'Drain node'}</button></div></article>`;
  }).join('') || emptyArt('fleet', 'No Macs registered', 'Install the agent on a Mac and it appears here.');
  renderGantt();
  $('#tenant-list').innerHTML = `<table><tr><th>Tenant</th><th>Quota</th><th>Reserved now</th><th>Weight</th><th>Budget used</th></tr>${tenants.map(t => { const used = ((state.usage || []).find(u => u.tenant === t.id)?.reservedGiBSeconds || 0) / 3600; return `<tr><td>${esc(t.id)}</td><td>${t.quota} GiB</td><td>${sum(jobs.filter(j => j.tenant === t.id && ACTIVE.includes(j.state)), j => j.spec.memoryGiB)} GiB</td><td>${t.weight}</td><td>${t.budget ? `${used.toFixed(1)} / ${t.budget} GiB-h` : 'no budget'}</td></tr>`; }).join('')}</table>`;
  $('#usage-list').innerHTML = (state.usage || []).map(u => `<div class="queue-line"><span>${esc(u.tenant)}</span><strong>${u.workerSeconds.toFixed(1)} worker-seconds · ${u.reservedGiBSeconds.toFixed(1)} GiB-seconds${u.kWh ? ` · ≈${(u.kWh * 1000).toFixed(1)} Wh (estimate)` : ''}</strong></div>`).join('') || '<div class="empty">Metered attempts appear after completion or lease expiry.</div>';
  renderModels();
  $('#events').innerHTML = events.length ? `<table><tr><th>Time</th><th>Event</th><th>Subject</th><th>Detail</th></tr>${events.map(e => `<tr><td>${esc(new Date(e.at * 1000).toLocaleString())}</td><td>${esc(e.kind)}</td><td>${esc(e.subject)}</td><td>${esc(e.detail)}</td></tr>`).join('')}</table>` : '<div class="empty">No administrator activity visible.</div>';
  const al = alerts(), badgeEl = $('#alert-count');
  badgeEl.hidden = al.length === 0; badgeEl.textContent = al.length;
  $('#alert-list').innerHTML = al.length ? al.map(a => `<div class="queue-line"><span>${badge(a.level === 'error' ? 'failed' : a.level === 'warn' ? 'queued' : 'running')} <strong>${esc(a.title)}</strong></span><span class="muted">${esc(a.detail)}</span></div>`).join('') : emptyArt('chart', 'All clear', 'No offline Macs, thermal limits or long waits.');
  renderTemplates();
  renderWelcome();
  renderLive();
  renderBuild();
  renderHero();
}
function renderWelcome() {
  const hasNode = state.nodes.some(n => n.online), hasJob = state.jobs.length > 0, done = state.jobs.some(j => j.state === 'succeeded');
  [['#step-node', hasNode], ['#step-job', hasJob], ['#step-done', done]].forEach(([s, ok]) => document.querySelector(s).classList.toggle('done', ok));
  $('#welcome').hidden = prefs.welcomeHidden || (hasNode && hasJob && done);
}
async function runDemoJob() {
  try {
    await api('jobs', {...DEMO_JOB});
    await refresh(); toast('Demo job submitted. Watch it in Workloads.');
  } catch (e) { fail(e); }
}

/* ---------- graphics: rings, area charts, heat strip (SVG/CSS only; every graphic is driven by state) ---------- */
const motionOK = () => !matchMedia('(prefers-reduced-motion: reduce)').matches;
const JOB_VARS = 8;
const jobIdx = id => { let h = 0; for (const c of String(id)) h = (h * 31 + c.charCodeAt(0)) >>> 0; return h % JOB_VARS; };
const jobColor = id => `var(--job-${jobIdx(id)})`;
let gradSeq = 0;
function ring({pct, size = 168, stroke = 14, label, sub = ''}) {
  const r = (size - stroke) / 2, c = 2 * Math.PI * r, p = Math.max(0, Math.min(100, pct ?? 0));
  const gid = 'rg' + (++gradSeq);
  return `<svg class="ring" viewBox="0 0 ${size} ${size}" role="img" aria-label="${esc(label ?? Math.round(p) + '%')} ${esc(sub)}">
    <defs><linearGradient id="${gid}" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="var(--accent-2)"/><stop offset="1" stop-color="var(--accent-color)"/></linearGradient></defs>
    <circle class="track" cx="${size / 2}" cy="${size / 2}" r="${r}" stroke-width="${stroke}"/>
    <circle class="bar glow" cx="${size / 2}" cy="${size / 2}" r="${r}" stroke-width="${stroke}" stroke="url(#${gid})" stroke-dasharray="${c.toFixed(1)}" stroke-dashoffset="${(c * (1 - p / 100)).toFixed(1)}"/>
    <text x="${size / 2}" y="${size / 2 + (sub ? 4 : 11)}" font-size="${Math.round(size * 0.24)}">${esc(label ?? Math.round(p) + '%')}</text>
    ${sub ? `<text class="sub" x="${size / 2}" y="${size / 2 + size * 0.17}">${esc(sub)}</text>` : ''}</svg>`;
}
// Catmull-Rom spline through points, as cubic Béziers
function smooth(pts) {
  if (pts.length < 2) return '';
  let d = `M${pts[0][0].toFixed(1)},${pts[0][1].toFixed(1)}`;
  for (let i = 0; i < pts.length - 1; i++) {
    const p0 = pts[i - 1] || pts[i], p1 = pts[i], p2 = pts[i + 1], p3 = pts[i + 2] || p2;
    const c1 = [p1[0] + (p2[0] - p0[0]) / 6, p1[1] + (p2[1] - p0[1]) / 6], c2 = [p2[0] - (p3[0] - p1[0]) / 6, p2[1] - (p3[1] - p1[1]) / 6];
    d += ` C${c1[0].toFixed(1)},${c1[1].toFixed(1)} ${c2[0].toFixed(1)},${c2[1].toFixed(1)} ${p2[0].toFixed(1)},${p2[1].toFixed(1)}`;
  }
  return d;
}
// values: [number], times: [epoch seconds] (optional); max defaults to 100 (percent)
function areaChart(values, times = [], {max = 100, unit = '%', label = 'trend'} = {}) {
  if (values.length < 2) return '<div class="muted" style="margin-top:8px;font-size:12px">Collecting samples…</div>';
  const W = 300, H = 72, pad = 4, gid = 'ag' + (++gradSeq);
  const pts = values.map((v, i) => [i / (values.length - 1) * W, H - pad - Math.min(1, v / max) * (H - pad * 2)]);
  const line = smooth(pts);
  return `<div class="areachart" data-v='${JSON.stringify(values.map(v => Math.round(v * 10) / 10))}' data-t='${JSON.stringify(times)}' data-u="${esc(unit)}">
    <svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none" role="img" aria-label="${esc(label)}">
      <defs><linearGradient id="${gid}" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="var(--accent-color)" stop-opacity=".32"/><stop offset="1" stop-color="var(--accent-color)" stop-opacity="0"/></linearGradient></defs>
      <path d="${line} L${W},${H} L0,${H} Z" fill="url(#${gid})"/><path class="line" d="${line}" vector-effect="non-scaling-stroke"/>
      <line class="cross" x1="0" y1="0" x2="0" y2="${H}" vector-effect="non-scaling-stroke"/><circle class="pt" r="4" cx="0" cy="0"/>
    </svg><span class="tip"></span></div>`;
}
document.addEventListener('pointermove', e => {
  const box = e.target.closest && e.target.closest('.areachart');
  $$('.areachart.hover').forEach(c => { if (c !== box) c.classList.remove('hover'); });
  if (!box) return;
  const v = JSON.parse(box.dataset.v), t = JSON.parse(box.dataset.t || '[]'), r = box.getBoundingClientRect();
  const f = Math.max(0, Math.min(1, (e.clientX - r.left) / r.width)), i = Math.round(f * (v.length - 1));
  const svg = box.querySelector('svg'), W = 300, H = 72, pad = 4, max = 100;
  const x = i / (v.length - 1) * W, y = H - pad - Math.min(1, v[i] / max) * (H - pad * 2);
  box.querySelector('.cross').setAttribute('x1', x); box.querySelector('.cross').setAttribute('x2', x);
  const pt = box.querySelector('.pt'); pt.setAttribute('cx', x); pt.setAttribute('cy', y);
  const tip = box.querySelector('.tip');
  tip.textContent = `${v[i]}${box.dataset.u}` + (t[i] ? ' · ' + new Date(t[i] * 1000).toLocaleTimeString() : '');
  tip.style.left = (f * 100) + '%';
  box.classList.add('hover');
});
document.addEventListener('pointerleave', () => $$('.areachart.hover').forEach(c => c.classList.remove('hover')), true);
function countUp(el, to, {digits = 0, ms = 700} = {}) {
  const from = Number(el.dataset.v ?? 0);
  el.dataset.v = String(to);
  if (!motionOK() || from === to) { el.textContent = to.toFixed(digits); return; }
  const t0 = performance.now();
  const step = now => { const k = Math.min(1, (now - t0) / ms), e = 1 - Math.pow(1 - k, 3); el.textContent = (from + (to - from) * e).toFixed(digits); if (k < 1) requestAnimationFrame(step); };
  requestAnimationFrame(step);
}
const idleSpark = h => {
  if (h.length < 2) return '';
  const max = Math.max(600, ...h.map(p => p.v)), W = 200, H = 28;
  const pts = h.map((p, i) => [i / (h.length - 1) * W, H - 2 - p.v / max * (H - 4)]);
  return `<div class="idle-spark" role="img" aria-label="Owner idle time, last ${h.length} samples since this page opened"><small>Owner idle time · since this page opened</small><svg viewBox="0 0 ${W} ${H}" preserveAspectRatio="none"><path class="fill" d="M0,${H} L${pts.map(p => p.map(n => n.toFixed(1)).join(',')).join(' L')} L${W},${H}Z"/><path d="M${pts.map(p => p.map(n => n.toFixed(1)).join(',')).join(' L')}"/></svg></div>`;
};
const emptyArt = (kind, title, body) => {
  const art = {
    queue: '<path d="M10 20h60M10 40h44M10 60h28"/><rect x="82" y="14" width="30" height="52" rx="8"/>',
    model: '<path d="M60 10l40 22-40 22-40-22z"/><path d="M20 50l40 22 40-22"/>',
    fleet: '<rect x="14" y="18" width="46" height="32" rx="6"/><path d="M26 62h22M37 50v12"/><rect x="72" y="30" width="38" height="26" rx="6"/>',
    chart: '<path d="M12 70V14M12 70h100"/><path d="M20 56l22-18 18 10 26-28 20 12"/>',
  }[kind];
  return `<div class="empty-art"><svg viewBox="0 0 124 80" aria-hidden="true">${art}</svg><strong style="color:var(--text);font-weight:600">${esc(title)}</strong><span>${body}</span></div>`;
};

/* ---------- hero: the one number that matters on each view ---------- */
function heroData() {
  if (!state) return null;
  const {nodes, jobs} = state, online = nodes.filter(n => n.online);
  const cores = sum(online, n => n.gpuCores || 0), reserved = sum(online, n => reservedOn(n.id, jobs)), budget = sum(online, n => n.budgetGiB);
  const utils = online.filter(n => typeof n.gpuUtilPct === 'number'), avg = utils.length ? sum(utils, n => n.gpuUtilPct) / utils.length : null;
  const running = jobs.filter(j => j.state === 'running').length, queued = jobs.filter(j => j.state === 'queued').length;
  const best = state.models.map(m => latest(m.id, 'benchmark')?.body.generationTokensPerSecond).filter(Number.isFinite);
  const accepting = nodes.filter(n => n.online && (!n.gate || n.gate.allowed)).length;
  switch (view) {
    case 'live': return {kicker: 'Live', big: cores || online.length, unit: cores ? 'GPU cores' : (online.length === 1 ? 'GPU' : 'GPUs'),
      text: `across ${online.length} online Mac${online.length === 1 ? '' : 's'}. One GPU per Mac, measured as macOS reports it.`,
      stats: [[avg === null ? '—' : Math.round(avg) + '%', 'GPU busy'], [`${reserved.toFixed(0)} of ${budget.toFixed(0)} GiB`, 'reserved'], [running, 'running'], [queued, 'queued']],
      art: ring({pct: avg ?? 0, label: avg === null ? '—' : Math.round(avg) + '%', sub: 'GPU busy'})};
    case 'overview': return {kicker: 'Overview', big: running, unit: running === 1 ? 'job running' : 'jobs running', text: `${queued} waiting for admission across ${online.length} Mac${online.length === 1 ? '' : 's'}.`,
      stats: [[jobs.filter(j => j.state === 'succeeded').length, 'completed'], [jobs.filter(j => j.state === 'failed').length, 'failed']],
      art: ring({pct: budget ? reserved / budget * 100 : 0, label: budget ? Math.round(reserved / budget * 100) + '%' : '—', sub: 'memory reserved'})};
    case 'fleet': return {kicker: 'Mac fleet', big: accepting, unit: `of ${nodes.length} accepting work`, text: 'Owner policy decides when each Mac may run fleet work.',
      stats: [[online.length, 'online'], [nodes.filter(n => n.drain).length, 'draining']], art: ring({pct: nodes.length ? accepting / nodes.length * 100 : 0, label: `${accepting}/${nodes.length}`, sub: 'accepting'})};
    case 'models': return {kicker: 'Model hub', big: best.length ? Math.max(...best).toFixed(0) : state.models.length, unit: best.length ? 'tok/s best generation' : 'models',
      text: best.length ? 'Fastest measured generation across your benchmarked models.' : 'Onboard a model, then benchmark and evaluate it on any Mac.',
      stats: [[state.models.filter(m => (m.status || 'ready') === 'ready').length, 'ready'], [state.models.filter(m => m.status === 'onboarding').length, 'onboarding'], [state.models.filter(m => (m.endpoints || []).length).length, 'deployed']]};
    case 'training': {
      const vals = state.models.map(m => m.finalValLoss).filter(Number.isFinite);
      return {kicker: 'Training', big: vals.length ? Math.min(...vals).toFixed(3) : jobs.filter(j => j.spec.kind === 'train').length, unit: vals.length ? 'best validation loss' : 'training runs',
        text: 'LoRA fine-tuning on one Mac with MLX. Loss updates as MLX reports it.', stats: [[jobs.filter(j => j.spec.kind === 'train' && j.state === 'running').length, 'running']]};
    }
    case 'jobs': return {kicker: 'Workloads', big: jobs.length, unit: 'workloads', text: 'Every job, with the scheduler\'s reason when it is waiting.',
      stats: [[running, 'running'], [queued, 'queued'], [jobs.filter(j => j.state === 'failed').length, 'failed']]};
    case 'deployments': return {kicker: 'Deployments', big: state.models.filter(m => (m.endpoints || []).length).length, unit: 'serving', text: 'Models served by llama-server or mlx_lm.server on a Mac, over loopback.', stats: []};
    case 'datasets': return {kicker: 'Datasets', big: (state.datasets || []).length, unit: 'datasets', text: 'Data you train and evaluate with, recorded with row counts and SHA-256.', stats: []};
    default: return null;
  }
}
function renderHero() {
  const el = $('#hero'), d = heroData();
  if (!d || (!token && !demo)) { el.hidden = true; document.body.classList.remove('has-hero'); return; }
  const big = typeof d.big === 'number' ? '<span class="num" id="hero-num" data-v="' + (el.dataset.view === view ? (el.dataset.last ?? 0) : 0) + '">0</span>' : esc(d.big);
  el.innerHTML = `<div><div class="kicker">${esc(d.kicker)}</div><h1><span id="hero-big">${big}</span><span class="unit">${esc(d.unit)}</span></h1><p>${esc(d.text)}</p>
    ${d.stats.length ? `<div class="stats">${d.stats.map(([v, l]) => `<span><b>${esc(v)}</b> ${esc(l)}</span>`).join('')}</div>` : ''}</div>${d.art ? `<div class="hero-art">${d.art}</div>` : ''}`;
  const n = document.getElementById('hero-num');
  if (n && typeof d.big === 'number') { countUp(n, d.big, {digits: Number.isInteger(d.big) ? 0 : 3}); el.dataset.last = String(d.big); } else if (n) n.textContent = d.big;
  el.dataset.view = view; el.hidden = false;
  document.body.classList.add('has-hero');
}

/* ---------- Live: GPUs + real-time scheduling ---------- */
const chip = (j, extra = '') => `<div class="job-chip ${extra}" data-chip="${esc(j.id)}" style="--hue-c:${jobColor(j.id)}"><span>${esc(j.spec.model || j.spec.runtime)}</span><small>${j.spec.memoryGiB} GiB · prio ${j.spec.priority}</small></div>`;
function renderLive() {
  if (!state) return;
  const {nodes, jobs} = state, online = nodes.filter(n => n.online);
  const cores = sum(online, n => n.gpuCores || 0), reserved = sum(online, n => reservedOn(n.id, jobs)), budget = sum(online, n => n.budgetGiB);
  const utils = online.filter(n => typeof n.gpuUtilPct === 'number'), avg = utils.length ? sum(utils, n => n.gpuUtilPct) / utils.length : null;
  $('#live-summary').innerHTML = nodes.length ? `<div class="heat-labelled" aria-label="Per-Mac GPU busy">${nodes.map(n => { const v = Math.round(n.online ? (n.gpuUtilPct ?? 0) : 0); return `<div class="heat-cell"><i style="--v:${v}"></i><span>${esc(n.id)}</span><b>${n.online ? v + '%' : 'offline'}</b></div>`; }).join('')}</div>` : '';

  $('#gpu-cards').innerHTML = nodes.map(n => {
    const util = typeof n.gpuUtilPct === 'number' ? n.gpuUtilPct : null, nc = n.gpuCores || 0;
    const lit = util === null ? 0 : util / 100 * nc, full = Math.floor(lit), part = lit - full > 0.15 ? 1 : 0;
    const coreCells = nc ? Array.from({length: nc}, (_, i) => `<div class="core ${i < full ? 'on' : i < full + part ? 'part' : ''}" style="--i:${i}"></div>`).join('') : '';
    const mine = jobs.filter(j => j.node === n.id && ACTIVE.includes(j.state));
    const total = n.totalGiB || n.budgetGiB;
    const segs = mine.map(j => `<div class="memseg" title="${esc(j.spec.model || j.spec.runtime)} · ${j.spec.memoryGiB} GiB reserved" style="width:${Math.min(100, j.spec.memoryGiB / total * 100)}%;background:${jobColor(j.id)}"></div>`).join('');
    const mark = n.gpuInUseMiB ? `<div class="mem-mark" title="GPU in use: ${(n.gpuInUseMiB / 1024).toFixed(1)} GiB" style="left:${Math.min(100, n.gpuInUseMiB / 1024 / total * 100)}%"></div>` : '';
    const t = (telemetry[n.id] || []).slice(-60);
    const spark = areaChart(t.map(s => s.util), t.map(s => s.at), {label: n.id + ' GPU busy over time'});
    return `<article class="gpu-card" data-gpu="${esc(n.id)}" style="--busy:${Math.round(util ?? 0)}">
      <header><h3>${esc(n.id)}</h3>${badge(n.online ? 'online' : 'offline')}</header>
      <p>${esc([n.gpuModel || n.gpu || 'Apple GPU', nc ? nc + ' GPU cores' : 'cores unknown', n.totalGiB ? n.totalGiB.toFixed(0) + ' GiB unified memory' : ''].filter(Boolean).join(' · '))}</p>
      <div class="gpu-util">${util === null ? '—' : util.toFixed(0) + '%'} <small>GPU busy</small></div>
      ${nc ? `<div class="cores" aria-label="${nc} GPU cores, ${util === null ? 'unknown' : util.toFixed(0) + '% busy'}">${coreCells}</div>` : ''}
      ${spark}
      <div class="membar" aria-label="Memory">${segs}${mark}</div>
      <div class="legend"><span>${reservedOn(n.id, jobs).toFixed(1)} GiB reserved by ${mine.length} job${mine.length === 1 ? '' : 's'}</span>${n.gpuInUseMiB ? `<span>▏ ${(n.gpuInUseMiB / 1024).toFixed(1)} GiB in use</span>` : ''}<span>${esc(n.thermal || 'thermal unknown')}</span></div>
    </article>`;
  }).join('') || emptyArt('fleet', 'No Macs registered yet', 'Start an agent: <code>make run-agent</code>');

  // scheduling flow with FLIP animation so chips visibly move queue → Mac
  const queued = jobs.filter(j => j.state === 'queued').sort((a, b) => b.spec.priority - a.spec.priority || a.created - b.created);
  const sig = JSON.stringify([queued.map(j => j.id + j.reason), nodes.map(n => [n.id, n.online, n.drain]), jobs.filter(j => ACTIVE.includes(j.state)).map(j => j.id + j.node)]);
  const feed = state.decisions || [];
  const last = feed[feed.length - 1];
  const gateText = queued.length ? `<strong>${queued.length} waiting</strong><span class="reason">${esc(queued[0].reason || 'evaluating…')}</span>` : '<strong>Queue empty</strong>nothing waiting';
  if (sig !== flowSig) {
    flowSig = sig;
    const before = new Map($$('#flow [data-chip]').map(e => [e.dataset.chip, e.getBoundingClientRect()]));
    $('#flow').innerHTML = `<div class="flow-col"><h4>Queue</h4>${queued.map(j => chip(j, 'waiting')).join('') || '<small class="muted">empty</small>'}</div>
      <div class="flow-gate" id="gate">${gateText}<small>admission: priority → fairness → fit</small></div>
      <div class="flow-col"><h4>Macs</h4>${nodes.map(n => `<div class="lane-row"><small><strong>${esc(n.id)}</strong> · ${n.online ? (n.drain ? 'draining' : 'online') : 'offline'}</small>${jobs.filter(j => j.node === n.id && ACTIVE.includes(j.state)).map(j => chip(j)).join('') || '<small class="muted">idle</small>'}</div>`).join('')}</div>`;
    if (!matchMedia('(prefers-reduced-motion: reduce)').matches) {
      const gate = document.getElementById('gate').getBoundingClientRect();
      $$('#flow [data-chip]').forEach(e => {
        const prev = before.get(e.dataset.chip), now = e.getBoundingClientRect();
        const from = prev || {left: gate.left, top: gate.top};
        const dx = from.left - now.left, dy = from.top - now.top;
        if (!dx && !dy) return;
        e.style.transition = 'none'; e.style.transform = `translate(${dx}px,${dy}px)${prev ? '' : ' scale(.6)'}`; e.style.opacity = prev ? '1' : '0';
        requestAnimationFrame(() => requestAnimationFrame(() => { e.style.transition = 'transform .6s cubic-bezier(.2,.8,.2,1), opacity .6s'; e.style.transform = ''; e.style.opacity = '1'; }));
      });
    }
  } else {
    const g = document.getElementById('gate'); if (g) g.innerHTML = gateText + '<small>admission: priority → fairness → fit</small>';
  }
  drawFlowFx(queued, nodes, jobs);
  const names = new Map(jobs.map(j => [j.id, j.spec.model || j.spec.runtime]));
  $('#decision-feed').innerHTML = feed.slice(-25).reverse().map(d => `<div><time>${new Date(d.at * 1000).toLocaleTimeString()}</time><span class="k-${esc(d.kind)}"><strong>${esc(d.kind)}</strong> ${esc(names.get(d.job) || String(d.job || '').slice(0, 8))}${d.node ? ' · ' + esc(d.node) : ''} — ${esc(d.detail)}</span></div>`).join('') || '<div class="muted">No scheduling decisions yet.</div>';
}

// curved lanes + travelling particles over the flow columns (decorative; the chips carry the information)
function drawFlowFx(queued, nodes, jobs) {
  const flow = $('#flow'); if (!flow) return;
  flow.querySelector('.fx')?.remove();
  if (innerWidth < 1181) return;
  const box = flow.getBoundingClientRect(), rel = el => { const r = el.getBoundingClientRect(); return {l: r.left - box.left, r: r.right - box.left, y: r.top - box.top + r.height / 2}; };
  const cols = flow.querySelectorAll('.flow-col'), gate = document.getElementById('gate'); if (cols.length < 2 || !gate) return;
  const q = rel(cols[0]), g = rel(gate), lanes = [...flow.querySelectorAll('.lane-row')];
  const curve = (x1, y1, x2, y2) => `M${x1},${y1} C${x1 + (x2 - x1) * .55},${y1} ${x1 + (x2 - x1) * .45},${y2} ${x2},${y2}`;
  const paths = [{d: curve(q.r, q.y, g.l, g.y), color: 'var(--status-queued)', dots: queued.length ? Math.min(3, queued.length) : 0}];
  lanes.forEach((lane, i) => {
    const r = rel(lane), running = jobs.filter(j => j.node === nodes[i]?.id && ACTIVE.includes(j.state));
    paths.push({d: curve(g.r, g.y, r.l, r.y), color: running[0] ? jobColor(running[0].id) : 'var(--line)', dots: running.length ? 2 : 0});
  });
  const svg = `<svg class="fx-lines" width="${box.width}" height="${box.height}" aria-hidden="true">${paths.map(p => `<path d="${p.d}"/>`).join('')}</svg>`;
  const dots = motionOK() ? paths.flatMap(p => Array.from({length: p.dots}, (_, k) => `<i class="fx-dot" style="offset-path:path('${p.d}');--c:${p.color};--delay:${(k * -1.1).toFixed(1)}s"></i>`)).join('') : '';
  flow.insertAdjacentHTML('afterbegin', `<div class="fx">${svg}${dots}</div>`);
}
function renderGantt() {
  const {nodes, jobs, time} = state, span = 1800, t0 = time - span;
  const axis = [0, 1, 2, 3].map(i => `<span style="left:${(i * 100 / 3).toFixed(1)}%">${i === 3 ? 'now' : '−' + Math.round(span / 60 * (3 - i) / 3) + ' min'}</span>`).join('');
  const rows = nodes.map(n => {
    const bars = jobs.filter(j => j.node === n.id && j.started && ACTIVE.includes(j.state)).map(j => {
      const left = Math.max(0, (j.started - t0) / span * 100), width = Math.max(1.5, 100 - left), hue = jobColor(j.id), label = `${j.spec.model || j.spec.runtime} · ${j.spec.memoryGiB} GiB`;
      return `<div class="bar ${esc(j.state)}" style="left:${left.toFixed(2)}%;width:${width.toFixed(2)}%;--bar:${hue}" tabindex="0" title="${esc(label)} · started ${Math.max(0, Math.round((time - j.started) / 60))} min ago">${esc(label)}</div>`;
    }).join('');
    return `<div class="gantt-row"><div><strong>${esc(n.id)}</strong><br><small>${reservedOn(n.id, jobs).toFixed(1)} / ${n.budgetGiB} GiB</small></div><div class="lane">${bars || '<small class="lane-idle">idle</small>'}<i class="now-line" aria-hidden="true"></i></div></div>`;
  }).join('');
  const queued = jobs.filter(j => j.state === 'queued');
  $('#gantt').innerHTML = rows ? `<div class="gantt-axis" aria-hidden="true"><span></span><div>${axis}</div></div>` + rows + (queued.length ? `<p class="gap">${queued.length} queued: ${queued.slice(0, 6).map(j => esc(j.spec.model || j.spec.runtime) + ' (' + esc(j.reason || 'waiting') + ')').join(', ')}</p>` : '') : emptyArt('fleet', 'No Macs registered', 'Install the agent on a Mac and it appears here.');
}
function renderTemplates() {
  $('#templates').innerHTML = prefs.templates.map((t, i) => `<span class="chip"><button data-template="${i}" title="Apply template">${esc(t.name)}</button><button data-template-del="${i}" aria-label="Delete template">×</button></span>`).join('');
}
function formBody(form) {
  const b = Object.fromEntries(new FormData(form));
  for (const k of ['memoryGiB', 'priority', 'retries', 'timeoutSeconds']) b[k] = Number(b[k]);
  try { b.args = JSON.parse(b.args); } catch { throw Error('Arguments must be a JSON array, e.g. ["--prompt","hello"]'); }
  if (!Array.isArray(b.args)) throw Error('Arguments must be a JSON array of strings.');
  b.labels = (b.labels || '').split(',').map(s => s.trim()).filter(Boolean);
  if (!b.node) delete b.node;
  return b;
}
function fillForm(spec, tenant) {
  const f = $('#submit');
  for (const k of ['model', 'runtime', 'memoryGiB', 'priority', 'mode', 'retries', 'timeoutSeconds', 'node']) if (f.elements[k]) f.elements[k].value = spec[k] ?? f.elements[k].defaultValue ?? '';
  if (tenant) f.elements.tenant.value = tenant;
  f.elements.args.value = JSON.stringify(spec.args || []);
  f.elements.labels.value = (spec.labels || []).join(', ');
}


/* ---------- Model hub: onboarding, benchmark, evaluate, quantise, compare ---------- */
const GB = n => n ? (n / 1024 ** 3).toFixed(2) + ' GiB' : '—';
const latest = (model, kind) => results.find(r => r.model === model && r.kind === kind && r.body.success);
function estimateGiB(paramsB, quant) {
  const q = (quant || '').toLowerCase();
  const table = [['q2', 2.8], ['q3', 3.6], ['q4', 4.8], ['q5', 5.7], ['q6', 6.6], ['q8', 8.5], ['int4', 4.5], ['4bit', 4.5], ['8bit', 8.5], ['f16', 16], ['bf16', 16], ['fp16', 16], ['f32', 32]];
  const bits = (table.find(([k]) => q.includes(k)) || [0, 16])[1];
  return Math.round((paramsB * 1e9 * bits / 8 * 1.15 / 1024 ** 3 + 0.75) * 10) / 10;
}
function provBadge(m) {
  const p = m.provenance;
  if (!p) return m.path ? '<span class="label" title="No signed attestation: registered manually or downloaded by an agent without a signing key">unattested</span>' : '';
  if (p.chainVerified) return '<span class="badge succeeded" title="Signed by the node that produced it, and every attested parent is signed too">✓ verified chain</span> ';
  if (p.verified) return '<span class="badge running" title="This step is signed by its node; the base model was not attested on this fleet">✓ signed · base unattested</span> ';
  return '<span class="badge queued" title="An attestation was missing or failed verification">unverified</span> ';
}
function renderModels() {
  const models = state.models;
  const tps = models.map(m => latest(m.id, 'benchmark')?.body.generationTokensPerSecond).filter(Number.isFinite), top = Math.max(1, ...tps);
  $('#model-list').innerHTML = models.map(m => {
    const b = latest(m.id, 'benchmark')?.body, e = latest(m.id, 'evaluate')?.body, st = m.status || 'ready';
    const gen = b?.generationTokensPerSecond;
    return `<article class="node-card spec"><div class="section-head"><h3>${esc(m.id)}</h3>${badge(st === 'ready' ? 'succeeded' : st === 'failed' ? 'failed' : st === 'onboarding' ? 'queued' : 'running').replace(/>[^<]*</, '>' + esc(st) + '<')}</div>
      ${Number.isFinite(gen) ? `<div class="big">${gen.toFixed(0)} <small>tok/s generation</small></div><div class="rel" title="${(gen / top * 100).toFixed(0)}% of the fastest model"><i style="width:${(gen / top * 100).toFixed(0)}%"></i></div>`
        : `<div class="big" style="color:var(--muted)">—<small> not benchmarked</small></div><div class="rel"><i style="width:0"></i></div>`}
      <p>${esc(m.runtime)}${m.quant ? ' · ' + esc(m.quant) : ''}${m.paramsB ? ' · ' + m.paramsB + 'B params' : ''} · ~${m.memoryGiB} GiB memory${m.sizeBytes ? ' · ' + GB(m.sizeBytes) : ''}</p>
      <p>${provBadge(m)}${b ? `<span class="label">${(b.promptTokensPerSecond || 0).toFixed(0)} tok/s prompt</span>` : ''}${e ? `<span class="label">eval ${e.passed}/${e.total}</span>` : ''}</p>
      <p class="muted" style="font-size:12px">${esc(m.source || 'No pinned source')}</p>
      <div class="actions"><label class="remember"><input type="checkbox" data-compare="${esc(m.id)}" ${compareSet.has(m.id) ? 'checked' : ''}> Compare</label><button data-model="${esc(m.id)}">Open</button></div></article>`;
  }).join('') || emptyArt('model', 'No models yet', 'Press <strong>Onboard a model</strong> to download one onto a Mac.');
  $('#compare').disabled = compareSet.size < 2;
  const withEndpoints = models.filter(m => (m.endpoints || []).length);
  $('#pg-model').innerHTML = (withEndpoints.length ? withEndpoints : models).map(m => `<option>${esc(m.id)}</option>`).join('');
}
function openModel(id) {
  const m = state.models.find(x => x.id === id); if (!m) return;
  activeModel = id; $('#modeldlg-title').textContent = id;
  const rs = results.filter(r => r.model === id).slice(0, 20);
  const meta = [['Status', m.status || 'ready'], ['Runtime', m.runtime], ['Quantisation', m.quant || '—'], ['Source', m.source || '—'], ['Revision', m.revision || '—'], ['On Mac', m.node || '—'], ['Path', m.path || '—'], ['SHA-256', m.sha256 || '—'], ['Size', GB(m.sizeBytes)], ['Memory estimate', `${m.memoryGiB} GiB`], ['Parent', m.parent || '—']];
  const row = r => {
    const b = r.body;
    const detail = r.kind === 'benchmark' ? `${(b.generationTokensPerSecond || 0).toFixed(1)} tok/s generation · ${(b.promptTokensPerSecond || 0).toFixed(0)} tok/s prompt`
      : r.kind === 'evaluate' ? `${b.passed}/${b.total} passed (${((b.passRate || 0) * 100).toFixed(0)}%)`
      : r.kind === 'download' ? `${GB(b.bytes)} · ${esc((b.sha256 || '').slice(0, 12))}…` : r.kind === 'quantize' ? `${esc(b.quant || '')} · ${GB(b.bytes)}` : '';
    const cases = r.kind === 'evaluate' && b.cases ? `<details><summary>cases</summary>${b.cases.map(c => `<div class="queue-line"><span>${c.passed ? '✔' : '✘'} ${esc(c.prompt)}</span><span class="muted">${esc(c.output.slice(0, 80))}</span></div>`).join('')}</details>` : '';
    return `<tr><td>${new Date(r.at * 1000).toLocaleString()}</td><td>${esc(r.kind)}</td><td>${b.success ? badge('succeeded') : badge('failed')}</td><td>${detail}${cases}</td><td>${esc(r.node)}</td></tr>`;
  };
  const chain = (m.provenance && m.provenance.chain) || [];
  const provHtml = m.provenance ? `<h3 class="gap">Provenance</h3><table><tr><th>Step</th><th>Signed</th><th>Signer</th><th>When</th><th>Note</th></tr>${chain.map(c => `<tr><td>${esc(c.kind)} <small>${esc(String(c.job).slice(0, 8))}</small></td><td>${c.verified ? badge('succeeded').replace('>succeeded<', '>verified<') : badge('queued').replace('>queued<', '>unverified<')}</td><td>${esc(c.signer || '—')}</td><td>${new Date(c.at * 1000).toLocaleString()}</td><td>${esc(c.why || '')}</td></tr>`).join('')}</table>${m.provenance.baseUnattested ? '<p class="muted">The base model came from outside this fleet (for example pulled by mlx-lm from Hugging Face) and has no attestation.</p>' : ''}<p class="muted">Signatures prove which Mac produced an artifact and that its result record was not altered; they are not a global supply-chain guarantee.</p>` : '';
  $('#modeldlg-body').innerHTML = `<table>${meta.map(([k, v]) => `<tr><td>${esc(k)}</td><td style="word-break:break-all">${esc(v)}</td></tr>`).join('')}</table>${provHtml}
    <h3 class="gap">History</h3>${rs.length ? `<table><tr><th>When</th><th>Kind</th><th>Result</th><th>Detail</th><th>Mac</th></tr>${rs.map(row).join('')}</table>` : '<div class="empty">No benchmarks or evaluations yet.</div>'}`;
  const ready = (m.status || 'ready') === 'ready' && !!m.path;
  const gguf = ready && m.runtime === 'llama';
  $('#m-bench').disabled = !gguf; $('#m-quant').disabled = !gguf;
  $('#m-eval').disabled = !(gguf || (ready && m.runtime === 'mlx'));
  $('#m-fuse').hidden = m.kind !== 'lora-adapter'; $('#m-fuse').disabled = !ready;
  $('#m-bench').title = $('#m-quant').title = gguf ? '' : 'Needs a downloaded GGUF model';
  $('#modeldlg').showModal();
}
function modelJob(m, kind, runtime, args, label = kind) {
  return {runtime, kind, modelId: m.id, model: `${label} ${m.id}`, memoryGiB: Math.max(1, m.memoryGiB), priority: 55, mode: 'exclusive', retries: 0, timeoutSeconds: 3600, args, labels: [kind], ...(m.node ? {node: m.node} : {})};
}
async function submitModelJob(spec, msg) {
  try { await api('jobs', spec); await refresh(); toast(msg); $('#modeldlg').close(); $('#evaldlg').close(); select('jobs'); } catch (e) { fail(e); }
}
function openCompare() {
  const ids = [...compareSet], rows = [['Status', m => m.status || 'ready'], ['Runtime', m => m.runtime], ['Quantisation', m => m.quant || '—'], ['Memory (GiB)', m => m.memoryGiB], ['Disk', m => GB(m.sizeBytes)],
    ['Generation tok/s', m => (latest(m.id, 'benchmark')?.body.generationTokensPerSecond ?? '—')], ['Prompt tok/s', m => (latest(m.id, 'benchmark')?.body.promptTokensPerSecond ?? '—')],
    ['Eval pass rate', m => { const e = latest(m.id, 'evaluate')?.body; return e ? `${e.passed}/${e.total}` : '—'; }]];
  const ms = ids.map(id => state.models.find(m => m.id === id)).filter(Boolean);
  $('#modeldlg-title').textContent = 'Compare models';
  $('#modeldlg-body').innerHTML = `<table><tr><th></th>${ms.map(m => `<th>${esc(m.id)}</th>`).join('')}</tr>${rows.map(([l, f]) => `<tr><td>${l}</td>${ms.map(m => `<td>${esc(f(m))}</td>`).join('')}</tr>`).join('')}</table>`;
  ['#m-bench', '#m-eval', '#m-quant'].forEach(s => { $(s).disabled = true; });
  $('#modeldlg').showModal();
}
function updateOnboardForm() {
  const f = $('#onboard-form'), kind = f.elements.kind.value;
  $$('#onboard-form [data-src]').forEach(e => e.classList.toggle('hidden', e.dataset.src !== kind));
  const p = Number(f.elements.paramsB.value);
  $('#onboard-estimate').value = p > 0 ? `~${estimateGiB(p, f.elements.quant.value)} GiB resident (estimate, not measured)` : 'Enter parameters and quantisation';
}
function openOnboard() {
  const capable = state.nodes.filter(n => n.runtimes.includes('download'));
  $('#onboard-node').innerHTML = capable.length ? capable.map(n => `<option value="${esc(n.id)}">${esc(n.id)}${n.online ? '' : ' (offline)'}</option>`).join('') : '<option value="">No Mac can download yet: redeploy the agent</option>';
  updateOnboardForm(); $('#onboard').showModal();
}
$('#onboard-open').onclick = openOnboard; $('#onboard-close').onclick = () => $('#onboard').close();
$('#onboard-form').addEventListener('input', updateOnboardForm);
$('#onboard-form').onsubmit = async e => {
  e.preventDefault();
  const f = Object.fromEntries(new FormData(e.target));
  const source = f.kind === 'hf' ? {kind: 'hf', repo: f.repo, file: f.file, revision: f.revision || 'main', maxGiB: Number(f.maxGiB)} : {kind: 'url', url: f.url, maxGiB: Number(f.maxGiB)};
  const body = {id: f.id, source, runtime: f.runtime, quant: f.quant};
  if (f.paramsB) body.paramsB = Number(f.paramsB);
  if (f.node) body.node = f.node;
  try { const r = await api('models/onboard', body); $('#onboard').close(); await refresh(); toast('Download started. Follow it in Workloads.'); openLogs(r.job); } catch (err) { fail(err); }
};
$('#modeldlg-close').onclick = () => $('#modeldlg').close();
$('#m-bench').onclick = () => { const m = state.models.find(x => x.id === activeModel); submitModelJob(modelJob(m, 'benchmark', 'bench', ['--model', m.path]), 'Benchmark queued'); };
$('#m-fuse').onclick = () => {
  const m = state.models.find(x => x.id === activeModel), base = state.models.find(x => x.id === m.parent) || {};
  submitModelJob(modelJob(m, 'fuse', 'fuse', ['--model', base.source || m.parent, '--adapter', m.path, '--name', m.id], 'fuse'), 'Fusing adapter; a "-fused" model appears when done');
};
$('#m-quant').onclick = () => {
  const m = state.models.find(x => x.id === activeModel), t = 'Q4_K_M';
  submitModelJob(modelJob(m, 'quantize', 'quantize', ['--input', m.path, '--type', t], 'quantize'), `Quantising to ${t}; a new model appears when it finishes`);
};
$('#m-eval').onclick = () => {
  $('#eval-suite').innerHTML = '<option value="">Custom (below)</option>' + Object.keys(suites).map(s => `<option>${esc(s)}</option>`).join('');
  $('#evaldlg').showModal();
};
$('#eval-suite').onchange = e => { if (suites[e.target.value]) $('#eval-cases').value = JSON.stringify(suites[e.target.value], null, 1); };
$('#evaldlg-close').onclick = () => $('#evaldlg').close();
function evalCases() {
  let cases;
  try { cases = JSON.parse($('#eval-cases').value); } catch { throw Error('Cases must be valid JSON'); }
  if (!Array.isArray(cases) || !cases.length) throw Error('Provide at least one case');
  return cases;
}
$('#eval-run').onclick = () => {
  try {
    const m = state.models.find(x => x.id === activeModel), cases = evalCases(), inline = JSON.stringify(cases);
    if (inline.length > 3900) throw Error('Suite is over 4 KB; trim cases (inline limit).');
    let args = ['--model', m.path, '--cases', inline];
    if (m.kind === 'lora-adapter') { const base = state.models.find(x => x.id === m.parent) || {}; args = ['--engine', 'mlx', '--model', base.source || m.parent, '--adapter', m.path, '--cases', inline]; }
    else if (m.runtime === 'mlx') args = ['--engine', 'mlx', '--model', m.path, '--cases', inline];
    submitModelJob(modelJob(m, 'evaluate', 'evaluate', args, 'evaluate'), 'Evaluation queued');
  } catch (e) { fail(e); }
};
$('#eval-save').onclick = async () => {
  try { const id = 'suite-' + new Date().toISOString().slice(0, 16).replace(/[:T]/g, '-'); await api('suites', {id, cases: evalCases()}); suites[id] = evalCases(); toast('Saved suite ' + id); } catch (e) { fail(e); }
};
$('#compare').onclick = openCompare;
let pgMessages = [], pgAbort = null;
function pgRender(partial = '') {
  const lines = pgMessages.map(m => (m.role === 'user' ? 'You: ' : 'Model: ') + m.content);
  if (partial !== null) lines.push('Model: ' + partial);
  $('#pg-output').textContent = lines.length ? lines.join('\n\n') : 'No reply yet.';
  $('#pg-output').scrollTop = $('#pg-output').scrollHeight;
}
$('#pg-clear').onclick = () => { pgMessages = []; $('#pg-meta').textContent = ''; pgRender(null); };
$('#pg-stop').onclick = () => { if (pgAbort) pgAbort.abort(); };
$('#pg-send').onclick = async () => {
  const prompt = $('#pg-prompt').value.trim();
  if (!prompt) return;
  pgMessages.push({role: 'user', content: prompt}); $('#pg-prompt').value = '';
  pgAbort = new AbortController(); $('#pg-send').disabled = true; $('#pg-stop').hidden = false;
  let reply = '', chunks = 0; const t0 = performance.now();
  pgRender('');
  try {
    if (demo) {   // simulated replica: stream a canned answer token by token
      for (const w of ('This is a simulated reply from the demo fleet. In a real deployment tokens stream from llama-server or mlx_lm.server running on a Mac.').split(' ')) {
        if (pgAbort.signal.aborted) break;
        reply += (reply ? ' ' : '') + w; chunks++; pgRender(reply); await new Promise(r => setTimeout(r, 60));
      }
    } else {
      const r = await fetch('/v1/chat/completions', {method: 'POST', signal: pgAbort.signal, headers: {'Content-Type': 'application/json', Authorization: 'Bearer ' + token},
        body: JSON.stringify({model: $('#pg-model').value, messages: pgMessages, max_tokens: 512, stream: true})});
      if (!r.ok) { const d = await r.json(); throw Error(d.error || 'Request failed'); }
      const reader = r.body.getReader(), dec = new TextDecoder(); let buf = '';
      for (;;) {
        const {value, done} = await reader.read();
        if (done) break;
        buf += dec.decode(value, {stream: true});
        let i;
        while ((i = buf.indexOf('\n')) >= 0) {
          const line = buf.slice(0, i).trim(); buf = buf.slice(i + 1);
          if (!line.startsWith('data:')) continue;
          const payload = line.slice(5).trim();
          if (payload === '[DONE]') continue;
          try {
            const delta = JSON.parse(payload).choices?.[0]?.delta?.content;
            if (delta) { reply += delta; chunks++; pgRender(reply); }
          } catch { /* ignore keep-alives */ }
        }
      }
    }
  } catch (e) {
    if (e.name !== 'AbortError') {
      pgMessages.pop(); $('#pg-prompt').value = prompt;
      pgRender(null); $('#pg-meta').textContent = 'Error: ' + hint(e.message) + ' (does this model have a deployed replica?)';
      $('#pg-send').disabled = false; $('#pg-stop').hidden = true; pgAbort = null;
      return;
    }
  }
  if (reply) pgMessages.push({role: 'assistant', content: reply}); else pgMessages.pop();
  const secs = (performance.now() - t0) / 1000;
  $('#pg-meta').textContent = reply ? `${chunks} chunks in ${secs.toFixed(1)} s${pgAbort && pgAbort.signal.aborted ? ' · stopped' : ''}` : '';
  pgRender(null);
  $('#pg-send').disabled = false; $('#pg-stop').hidden = true; pgAbort = null;
};


/* ---------- Build / Run: datasets, training, workflows, deployments ---------- */
const nodeOptions = (cap) => state.nodes.filter(n => !cap || n.runtimes.includes(cap)).map(n => `<option value="${esc(n.id)}">${esc(n.id)}${n.online ? '' : ' (offline)'}</option>`).join('') || '<option value="">No Mac has this runtime yet</option>';
function renderBuild() {
  const ds = state.datasets || [];
  $('#dataset-list').innerHTML = ds.length ? `<table><tr><th>Dataset</th><th>Status</th><th>Rows</th><th>Size</th><th>Mac</th><th>Source</th></tr>${ds.map(d => `<tr><td><strong>${esc(d.id)}</strong></td><td>${badge(d.status === 'ready' ? 'succeeded' : d.status === 'failed' ? 'failed' : 'queued').replace(/>[^<]*</, '>' + esc(d.status) + '<')}</td><td>${d.rows ?? '—'}</td><td>${GB(d.bytes)}</td><td>${esc(d.node || '—')}</td><td>${esc(d.source || d.path || '')}</td></tr>`).join('')}</table>` : emptyArt('queue', 'No datasets yet', 'Add one from Hugging Face below, or register a file already on a Mac.');
  $('#dataset-node').innerHTML = nodeOptions('download'); $('#dataset-local-node').innerHTML = nodeOptions();
  $('#workflow-node').innerHTML = nodeOptions('download');
  $('#train-dataset').innerHTML = ds.filter(d => d.status === 'ready').map(d => `<option value="${esc(d.id)}">${esc(d.id)} (${d.rows ?? '?'} rows)</option>`).join('') || '<option value="">Add a dataset first</option>';
  $('#mlx-models').innerHTML = state.models.filter(m => m.runtime === 'mlx').map(m => `<option value="${esc(m.source || m.id)}">`).join('');
  // training runs
  const runs = state.jobs.filter(j => j.spec.kind === 'train');
  const adapters = state.models.filter(m => m.kind === 'lora-adapter');
  const best = Math.min(...adapters.map(a => a.finalValLoss).filter(v => typeof v === 'number'), Infinity);
  $('#train-runs').innerHTML = runs.length ? `<table><tr><th>Run</th><th>State</th><th>Dataset</th><th>Iters</th><th>Result</th><th></th></tr>${runs.map(j => {
    const a = adapters.find(x => x.trainJob === j.id);
    const iters = (j.spec.args[j.spec.args.indexOf('--iters') + 1]) || '—';
    return `<tr><td><strong>${esc(j.spec.modelId || '')}</strong><br><small>${esc(j.id.slice(0, 10))}</small></td><td>${badge(j.state)}</td><td>${esc(j.spec.datasetId || '—')}</td><td>${esc(iters)}</td>
      <td>${a ? `val loss ${a.finalValLoss?.toFixed(3) ?? '—'}${a.finalValLoss === best ? ' <span class="label">best</span>' : ''}<br><small>${esc(a.path)}</small>` : (j.state === 'running' ? 'training…' : '—')}</td>
      <td><button data-chart="${esc(j.id)}">Loss chart</button> <button data-logs="${esc(j.id)}">Logs</button></td></tr>`;
  }).join('')}</table>` : emptyArt('chart', 'No training runs yet', 'Add a dataset, then start a fine-tune.');
  // workflows
  const wfs = state.workflows || [], byId = new Map(state.jobs.map(j => [j.id, j]));
  $('#workflow-list').innerHTML = wfs.length ? wfs.map(w => `<div class="queue-line"><span><strong>${esc(w.name)}</strong> ${w.steps.map(s => { const j = byId.get(s.job); return `<span class="label" title="${esc(s.name)}">${esc(s.name)} · ${esc(j ? j.state : '?')}</span>`; }).join('→')}</span><button data-wf-cancel="${esc(w.id)}">Cancel</button></div>`).join('') : emptyArt('queue', 'No pipelines yet', 'Chain onboard, benchmark and deploy into one workflow.');
  // deployments
  const ready = state.models.filter(m => (m.runtime === 'llama' || (m.runtime === 'mlx' && m.kind === 'fused')) && m.path);
  $('#deploy-list').innerHTML = ready.map(m => {
    const live = (m.endpoints || []).length > 0;
    return `<article class="node-card"><div class="section-head"><h3>${esc(m.id)}</h3>${live ? badge('running').replace('>running<', '>serving<') : badge('offline').replace('>offline<', '>stopped<')}</div>
      <p>${esc(m.quant || '')} · ~${m.memoryGiB} GiB · ${esc(m.node || 'Mac unknown')}</p>${live ? `<p><code>${esc(m.endpoints[m.endpoints.length - 1])}</code></p>` : ''}
      <div class="actions">${live ? `<button data-undeploy="${esc(m.deployJob || '')}">Stop</button><button data-view-go="playground">Open Playground</button>` : `<button class="primary" data-deploy="${esc(m.id)}">Deploy</button>`}</div></article>`;
  }).join('') || emptyArt('model', 'No downloaded models yet', 'Onboard a GGUF model in the Model hub.');
}
function drawChart(m) {
  const series = [['trainLoss', 'Train loss', 'var(--accent-color)'], ['valLoss', 'Validation loss', 'var(--status-queued)']].filter(([k]) => (m[k] || []).length);
  if (!series.length) { $('#train-chart').innerHTML = emptyArt('chart', 'No loss reported yet', 'MLX prints the first line after the model loads.'); return; }
  const pts = series.flatMap(([k]) => m[k]), maxS = Math.max(...pts.map(p => p[0]), 1), maxV = Math.max(...pts.map(p => p[1])) * 1.05, minV = Math.min(0, Math.min(...pts.map(p => p[1])));
  const W = 720, H = 260, L = 44, R = 16, T = 14, B = 30, X = s => L + s / maxS * (W - L - R), Y = v => T + (1 - (v - minV) / Math.max(maxV - minV, 1e-6)) * (H - T - B);
  const ticks = Array.from({length: 5}, (_, i) => minV + (maxV - minV) * i / 4);
  const gid = 'lg' + (++gradSeq);
  const train = (m.trainLoss || []).map(p => [X(p[0]), Y(p[1])]), val = (m.valLoss || []).map(p => [X(p[0]), Y(p[1])]);
  const bestVal = (m.valLoss || []).reduce((b, p) => (!b || p[1] < b[1] ? p : b), null);
  const area = train.length > 1 ? `<path d="${smooth(train)} L${train[train.length - 1][0]},${H - B} L${train[0][0]},${H - B} Z" fill="url(#${gid})"/>` : '';
  $('#train-chart').innerHTML = `<div class="areachart losschart" style="margin:0" data-loss='${JSON.stringify({t: m.trainLoss || [], v: m.valLoss || []})}'>
    <svg class="lossplot" viewBox="0 0 ${W} ${H}" role="img" aria-label="Training loss over steps">
      <defs><linearGradient id="${gid}" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="var(--accent-color)" stop-opacity=".28"/><stop offset="1" stop-color="var(--accent-color)" stop-opacity="0"/></linearGradient></defs>
      <g class="grid">${ticks.map(v => `<line x1="${L}" x2="${W - R}" y1="${Y(v).toFixed(1)}" y2="${Y(v).toFixed(1)}"/><text x="${L - 8}" y="${(Y(v) + 4).toFixed(1)}" text-anchor="end">${v.toFixed(2)}</text>`).join('')}</g>
      <text x="${L}" y="${H - 8}">step 0</text><text x="${W - R}" y="${H - 8}" text-anchor="end">step ${maxS}</text>
      ${area}
      ${train.length > 1 ? `<path d="${smooth(train)}" fill="none" stroke="var(--accent-color)" stroke-width="2.6" stroke-linecap="round" stroke-linejoin="round"/>` : ''}
      ${val.length ? `<path d="${val.length > 1 ? smooth(val) : ''}" fill="none" stroke="var(--status-queued)" stroke-width="2.4" stroke-dasharray="6 5" stroke-linecap="round"/>${val.map(p => `<circle cx="${p[0].toFixed(1)}" cy="${p[1].toFixed(1)}" r="3.5" fill="var(--status-queued)" stroke="var(--surface)" stroke-width="2"/>`).join('')}` : ''}
      ${bestVal ? `<g><circle cx="${X(bestVal[0]).toFixed(1)}" cy="${Y(bestVal[1]).toFixed(1)}" r="7" fill="none" stroke="var(--status-succeeded)" stroke-width="2.5"/><text x="${(X(bestVal[0]) + (X(bestVal[0]) > W - 170 ? -12 : 12)).toFixed(1)}" y="${(Y(bestVal[1]) - 10).toFixed(1)}" text-anchor="${X(bestVal[0]) > W - 170 ? 'end' : 'start'}" style="fill:var(--status-succeeded);font-weight:600">best ${bestVal[1].toFixed(3)} @ ${bestVal[0]}</text></g>` : ''}
    </svg></div>
    <div class="legend" style="display:flex;gap:18px;flex-wrap:wrap;margin-top:6px">${series.map(([k, l, c]) => `<span><i style="display:inline-block;width:10px;height:10px;border-radius:50%;background:${c};margin-right:6px"></i>${l}${m[k].length ? ' (last ' + m[k][m[k].length - 1][1].toFixed(3) + ')' : ''}</span>`).join('')}${m.tokensPerSec ? `<span class="muted">${m.tokensPerSec[m.tokensPerSec.length - 1][1].toFixed(0)} tok/s</span>` : ''}${m.peakGB ? `<span class="muted">peak ${m.peakGB[m.peakGB.length - 1][1].toFixed(1)} GB</span>` : ''}</div>`;
}
async function showChart(job) {
  chartJob = job; $('#train-chart-panel').hidden = false;
  $('#train-chart-title').textContent = 'Loss · ' + job.slice(0, 10);
  try { runMetrics = await api('runs/' + job + '/metrics'); drawChart(runMetrics); } catch (e) { fail(e); }
}
setInterval(() => { if (chartJob && view === 'training' && !paused) showChart(chartJob); }, 3000);

$('#dataset-form').onsubmit = async e => {
  e.preventDefault(); const f = Object.fromEntries(new FormData(e.target));
  try { await api('datasets/onboard', {id: f.id, node: f.node || undefined, source: {repo: f.repo, file: f.file, revision: f.revision || 'main', maxGiB: Number(f.maxGiB)}}); await refresh(); toast('Dataset download started'); } catch (err) { fail(err); }
};
$('#dataset-local-form').onsubmit = async e => {
  e.preventDefault(); const f = Object.fromEntries(new FormData(e.target));
  try { await api('datasets', {id: f.id, path: f.path, rows: f.rows ? Number(f.rows) : undefined, node: f.node || undefined, source: 'local file'}); await refresh(); toast('Dataset registered'); } catch (err) { fail(err); }
};
$('#train-form').onsubmit = async e => {
  e.preventDefault(); const f = Object.fromEntries(new FormData(e.target));
  const ds = (state.datasets || []).find(d => d.id === f.dataset);
  if (!ds) return fail(Error('Pick a ready dataset first (Datasets).'));
  const id = f.base.replace(/[^A-Za-z0-9._-]+/g, '__');
  try {
    if (!state.models.some(m => m.id === id)) await api('models', {id, runtime: 'mlx', memoryGiB: Number(f.memoryGiB), source: f.base, status: 'ready', notes: 'base model for fine-tuning'});
    const spec = {runtime: 'train', kind: 'train', modelId: id, datasetId: f.dataset, model: `train ${id}`, memoryGiB: Number(f.memoryGiB), priority: 60, mode: 'exclusive', retries: 0, timeoutSeconds: 21600, labels: ['train'],
      args: ['--model', f.base, '--data', '{{dataset.path}}', '--iters', String(f.iters), '--batch-size', String(f.batch), '--num-layers', String(f.layers), '--learning-rate', String(f.lr)]};
    if (ds.node) spec.node = ds.node;
    const r = await api('jobs', spec); await refresh(); toast('Training queued'); showChart(r.id);
  } catch (err) { fail(err); }
};
$('#workflow-form').onsubmit = async e => {
  e.preventDefault(); const f = Object.fromEntries(new FormData(e.target)), mem = Number(f.memoryGiB);
  const common = {memoryGiB: mem, priority: 55, retries: 0, timeoutSeconds: 3600, mode: 'exclusive', ...(f.node ? {node: f.node} : {})};
  const bench = {name: 'benchmark', spec: {...common, runtime: 'bench', kind: 'benchmark', modelId: f.id, model: 'benchmark ' + f.id, args: ['--model', '{{model.path}}']}};
  const evalStep = {name: 'evaluate', spec: {...common, runtime: 'evaluate', kind: 'evaluate', modelId: f.id, model: 'evaluate ' + f.id, args: ['--model', '{{model.path}}', '--cases', JSON.stringify([{prompt: 'Once upon a time', regex: '.+'}])]}};
  try {
    let steps;
    if (f.template === 'onboard') {
      await api('models', {id: f.id, runtime: 'llama', memoryGiB: mem, status: 'onboarding', source: `${f.repo}/${f.file}`});
      steps = [{name: 'download', spec: {...common, runtime: 'download', kind: 'download', mode: 'shared', memoryGiB: 0.5, modelId: f.id, model: 'download ' + f.id, args: ['--repo', f.repo, '--file', f.file]}}, {...bench, dependsOn: ['download']}, {...evalStep, dependsOn: ['benchmark']}];
    } else {
      steps = [{name: 'quantize', spec: {...common, runtime: 'quantize', kind: 'quantize', modelId: f.id, model: 'quantize ' + f.id, args: ['--input', '{{model.path}}', '--type', 'Q4_K_M']}}, {...bench, dependsOn: ['quantize']}];
    }
    await api('workflows', {name: f.template + ' ' + f.id, steps}); await refresh(); toast('Pipeline started');
  } catch (err) { fail(err); }
};

/* ---------- navigation ---------- */
function select(name) {
  if (!VIEWS.includes(name)) name = 'overview';
  const apply = () => {
    view = name;
    $$('.view').forEach(e => e.classList.toggle('hidden', e.id !== name));
    $$('nav button').forEach(e => e.classList.toggle('selected', e.dataset.view === name));
    $('#title').textContent = TITLES[name];
    $('#metrics').hidden = name !== 'overview';
    if (state) { renderHero(); renderLive(); }
    try { history.replaceState(null, '', '#' + name); } catch {}
  };
  if (document.startViewTransition && motionOK() && view !== name && !document.hidden) {
    // Progressive enhancement: never let an aborted transition surface as an error or skip the update.
    let ran = false;
    try {
      const t = document.startViewTransition(() => { ran = true; apply(); });
      t.ready.catch(() => {}); t.finished.catch(() => {}); t.updateCallbackDone.catch(() => {});
    } catch { /* fall through */ }
    setTimeout(() => { if (!ran) { ran = true; apply(); } }, 400);   // safety net if the browser never runs the callback
  } else apply();
}
addEventListener('scroll', () => $('#bar').classList.toggle('scrolled', scrollY > 72), {passive: true});
addEventListener('resize', () => { if (state) drawFlowFx(state.jobs.filter(j => j.state === 'queued'), state.nodes, state.jobs); });

/* ---------- refresh loop ---------- */
function tick() {
  const dot = $('#live-dot'), label = $('#updated');
  if (demo) { label.textContent = 'Demo · simulated'; dot.className = 'dot live'; return; }
  if (!token) { label.textContent = 'Not connected'; dot.className = 'dot'; return; }
  if (failures > 0) { label.textContent = 'Disconnected · last update ' + (lastOk ? ago(lastOk) : 'never'); dot.className = 'dot error'; return; }
  label.textContent = (paused ? 'Paused · ' : streaming ? 'Streaming · ' : 'Live · ') + 'updated ' + (lastOk ? ago(lastOk) : '—');
  dot.className = 'dot' + (paused ? '' : ' live');
}
async function refresh() {
  if ((!token && !demo) || busy) return;
  busy = true;
  try {
    state = await api('state');
    try { telemetry = await api('telemetry'); } catch { telemetry = {}; }
    try { results = await api('results'); suites = await api('suites'); } catch { results = []; suites = {}; }
    try {
      const h = await api('history');
      if (h.length) samples = h.slice(-120);
      else throw 0;
    } catch {
      const jobs = state.jobs;
      samples.push({reservedGiB: sum(jobs.filter(j => ACTIVE.includes(j.state)), j => j.spec.memoryGiB), running: jobs.filter(j => j.state === 'running').length, queued: jobs.filter(j => j.state === 'queued').length});
      samples = samples.slice(-120);
    }
    render(); notice(''); failures = 0; lastOk = Date.now();
    $('#connect').hidden = true;
    startStream();
  } catch (e) {
    failures++; notice(hint(e.message)); $('#connect').hidden = !!demo;
  } finally { busy = false; tick(); }
}

/* ---------- real-time stream (SSE over fetch; polling is the fallback) ---------- */
async function startStream() {
  if (streaming || demo || !token) return;
  streamAbort = new AbortController(); streaming = true;
  try {
    const r = await fetch('/api/stream', {headers: {Authorization: 'Bearer ' + token}, signal: streamAbort.signal});
    if (!r.ok || !r.body) throw Error('stream unavailable');
    const reader = r.body.getReader(), dec = new TextDecoder(); let buf = '';
    for (;;) {
      const {value, done} = await reader.read();
      if (done) break;
      buf += dec.decode(value, {stream: true});
      let i;
      while ((i = buf.indexOf('\n\n')) >= 0) {
        const frame = buf.slice(0, i); buf = buf.slice(i + 2);
        if (!frame.startsWith('data: ') || paused) continue;
        const snap = JSON.parse(frame.slice(6));
        telemetry = snap.telemetry || telemetry; delete snap.telemetry;
        state = snap; failures = 0; lastOk = Date.now(); notice(''); $('#connect').hidden = true;
        render(); tick();
      }
    }
  } catch { /* fall back to polling below */ }
  streaming = false;
  if (token && !demo) setTimeout(startStream, 3000);
}
function restartTimer() { clearInterval(timer); timer = setInterval(() => { if (!paused && !streaming) refresh(); }, Number(prefs.interval) || 5000); }

/* ---------- logs ---------- */
function drawLogs() {
  const q = $('#log-search').value, needle = q ? new RegExp(reEsc(esc(q)), 'gi') : null;
  const html = logLines.filter(l => !q || l.text.toLowerCase().includes(q.toLowerCase())).map(l => {
    let t = esc(`${new Date(l.at * 1000).toLocaleTimeString()}  ${l.text}`);
    if (needle) t = t.replace(needle, m => `<mark>${m}</mark>`);
    const cls = /error|fail|fatal|traceback/i.test(l.text) ? 'log-err' : /warn/i.test(l.text) ? 'log-warn' : '';
    return cls ? `<span class="${cls}">${t}</span>` : t;
  }).join('\n');
  const pre = $('#log-text');
  pre.innerHTML = html || 'No logs received.';
  pre.classList.toggle('nowrap', !$('#log-wrap').checked);
  if ($('#log-follow').checked) pre.scrollTop = pre.scrollHeight;
}
async function pullLogs() { if (!logJob) return; try { logLines = await api('logs/' + encodeURIComponent(logJob)); drawLogs(); } catch (e) { fail(e); } }
async function openLogs(id) {
  logJob = id; $('#log-title').textContent = 'Worker logs · ' + id.slice(0, 10);
  await pullLogs();
  if (!$('#logs').open) $('#logs').showModal();
  clearInterval(logTimer); logTimer = setInterval(() => { if ($('#log-follow').checked) pullLogs(); }, 2000);
}
function download(name, text, mime) {
  const a = document.createElement('a');
  a.href = URL.createObjectURL(new Blob([text], {type: mime})); a.download = name; a.click();
  setTimeout(() => URL.revokeObjectURL(a.href), 1000);
}
function csvCell(v) { const s = String(v ?? ''); return /[",\n]/.test(s) ? '"' + s.replace(/"/g, '""') + '"' : s; }
function exportJobs(kind) {
  const rows = selected.size ? state.jobs.filter(j => selected.has(j.id)) : filtered();
  if (kind === 'json') return download('gryvia-jobs.json', JSON.stringify(rows, null, 2), 'application/json');
  const head = ['id', 'tenant', 'state', 'runtime', 'model', 'memoryGiB', 'priority', 'node', 'attempt', 'created', 'labels'];
  const body = rows.map(j => [j.id, j.tenant, j.state, j.spec.runtime, j.spec.model || '', j.spec.memoryGiB, j.spec.priority, j.node || '', j.attempt, new Date(j.created * 1000).toISOString(), (j.spec.labels || []).join('|')].map(csvCell).join(','));
  download('gryvia-jobs.csv', [head.join(','), ...body].join('\n') + '\n', 'text/csv');
}

/* ---------- node drawer ---------- */
function openNode(id) {
  const n = state.nodes.find(n => n.id === id); if (!n) return;
  const mine = state.jobs.filter(j => j.node === id && ACTIVE.includes(j.state));
  const rows = [['Hardware', n.gpu || n.host || '—'], ['Budget', `${n.budgetGiB} GiB (${reservedOn(id, state.jobs).toFixed(1)} reserved)`], ['Slots', n.slots ?? 1], ['Thermal', n.thermal || 'unknown'], ['Runtimes', n.runtimes.join(', ')], ['Last seen', new Date(n.seen * 1000).toLocaleTimeString()], ['Admission', n.drain ? 'Draining' : 'Open']];
  $('#drawer-title').textContent = id;
  $('#drawer-body').innerHTML = `<table>${rows.map(([k, v]) => `<tr><td>${esc(k)}</td><td>${esc(v)}</td></tr>`).join('')}</table><h3 class="gap">Active workloads</h3>${table(mine, false)}`;
  $('#drawer').showModal();
}

/* ---------- command palette ---------- */
let paletteItems = [], paletteIndex = 0;
function commands() {
  const c = VIEWS.map((v, i) => ({title: 'Go to ' + TITLES[v], hint: String(i + 1), run: () => select(v)}));
  c.push({title: 'Submit workload…', hint: 'form', run: () => { select('jobs'); setTimeout(() => $('#submit').elements.model.focus(), 50); }});
  c.push({title: 'Run a demo job', hint: 'quick', run: runDemoJob}, {title: 'Open the getting-started guide', hint: 'help', run: () => select('guide')});
  c.push({title: 'Start a fine-tune…', hint: 'train', run: () => select('training')}, {title: 'Add a dataset…', hint: 'data', run: () => select('datasets')});
  c.push({title: 'Onboard a model…', hint: 'models', run: () => { select('models'); if (state) openOnboard(); }});
  c.push({title: 'Refresh now', hint: 'r', run: refresh}, {title: paused ? 'Resume live updates' : 'Pause live updates', hint: 'p', run: togglePause});
  c.push({title: demo ? 'Exit demo mode' : 'Start demo mode (simulated fleet)', hint: 'demo', run: () => demo ? stopDemo() : startDemo()});
  c.push({title: 'Export workloads as CSV', hint: 'csv', run: () => state && exportJobs('csv')}, {title: 'Show keyboard shortcuts', hint: '?', run: () => $('#help').showModal()});
  Object.keys(ACCENTS).forEach(a => c.push({title: 'Accent: ' + a, hint: 'theme', run: () => { prefs.accent = a; savePrefs(); }}));
  ['auto', 'light', 'dark'].forEach(t => c.push({title: 'Theme: ' + t, hint: 'theme', run: () => { prefs.theme = t; savePrefs(); }}));
  if (state) {
    state.nodes.forEach(n => c.push({title: 'Node ' + n.id, hint: n.online ? 'online' : 'offline', run: () => openNode(n.id)}));
    state.jobs.slice(0, 40).forEach(j => c.push({title: 'Logs: ' + (j.spec.model || j.spec.runtime) + ' ' + j.id.slice(0, 8), hint: j.state, run: () => openLogs(j.id)}));
  }
  return c;
}
function drawPalette() {
  const q = $('#palette-input').value.toLowerCase();
  paletteItems = commands().filter(c => c.title.toLowerCase().includes(q)).slice(0, 30);
  paletteIndex = Math.min(paletteIndex, Math.max(0, paletteItems.length - 1));
  $('#palette-list').innerHTML = paletteItems.map((c, i) => `<li role="option" data-i="${i}" aria-selected="${i === paletteIndex}"><span>${esc(c.title)}</span><small>${esc(c.hint)}</small></li>`).join('') || '<li><span>No matches</span></li>';
}
function openPalette() { $('#palette-input').value = ''; paletteIndex = 0; drawPalette(); if (!$('#palette').open) $('#palette').showModal(); $('#palette-input').focus(); }
function runPalette(i) { const c = paletteItems[i]; if (!c) return; $('#palette').close(); c.run(); }
function togglePause() { paused = !paused; $('#pause').textContent = paused ? 'Resume' : 'Pause'; tick(); }

/* ---------- events ---------- */
$('#login').addEventListener('submit', async e => {
  e.preventDefault(); token = $('#token').value;
  try { if ($('#remember').checked) sessionStorage.setItem('gryvia.token', token); else sessionStorage.removeItem('gryvia.token'); } catch {}
  await refresh();
});
$('#run-demo').onclick = runDemoJob; $('#run-demo-2').onclick = runDemoJob;
$('#welcome-hide').onclick = () => { prefs.welcomeHidden = true; savePrefs(); $('#welcome').hidden = true; };
$('#open-guide').onclick = () => select('guide');
$('#demo-start').onclick = startDemo; $('#demo-exit').onclick = stopDemo;
$('#pause').onclick = togglePause; $('#refresh').onclick = refresh;
$('#palette-open').onclick = openPalette;
$('#filter').oninput = () => state && render(); $('#state-filter').onchange = () => state && render();
$$('nav button').forEach(e => e.onclick = () => select(e.dataset.view));
$('#export-csv').onclick = () => state && exportJobs('csv'); $('#export-json').onclick = () => state && exportJobs('json');
$('#bulk-clear').onclick = () => { selected.clear(); render(); };
$('#bulk-cancel').onclick = async () => {
  try { for (const id of selected) await api('cancel', {id}); toast(`Cancelled ${selected.size} workloads`); selected.clear(); await refresh(); } catch (e) { fail(e); }
};
$('#submit').onsubmit = async e => {
  e.preventDefault();
  try { await api('jobs', formBody(e.target)); await refresh(); toast('Workload submitted'); } catch (err) { fail(err); }
};
$('#save-template').onclick = () => {
  try {
    const b = formBody($('#submit')); delete b.node;
    prefs.templates.push({name: `${b.model || b.runtime} · ${b.memoryGiB} GiB`, body: b}); prefs.templates = prefs.templates.slice(-12);
    savePrefs(); renderTemplates(); toast('Template saved');
  } catch (err) { fail(err); }
};
for (const [selector, path, numbers] of [['#tenant-form', 'tenants', ['quota', 'weight', 'budgetGiBHours']], ['#model-form', 'models', ['memoryGiB']]]) {
  $(selector).onsubmit = async e => {
    e.preventDefault();
    try { const b = Object.fromEntries(new FormData(e.target)); numbers.forEach(k => { if (b[k] === '' || b[k] === undefined) delete b[k]; else b[k] = Number(b[k]); }); if (b.endpoints) b.endpoints = JSON.parse(b.endpoints); await api(path, b); await refresh(); toast('Saved'); } catch (err) { fail(err); }
  };
}
document.addEventListener('change', async e => {
  const pm = e.target.dataset && e.target.dataset.policyMode;
  if (pm) {
    try { const n = state.nodes.find(x => x.id === pm); await api('nodes/policy', {id: pm, ...(n.policy || {}), mode: e.target.value}); await refresh(); toast(`${pm}: ${e.target.value === 'idle' ? 'runs fleet work only when you are away' : e.target.value === 'paused' ? 'fleet work paused' : 'runs fleet work any time'}`); } catch (err) { fail(err); }
    return;
  }
  const id = e.target.dataset && e.target.dataset.select;
  const cm = e.target.dataset && e.target.dataset.compare;
  if (cm) { e.target.checked ? compareSet.add(cm) : compareSet.delete(cm); $('#compare').disabled = compareSet.size < 2; return; }
  if (id) { e.target.checked ? selected.add(id) : selected.delete(id); $('#bulk').hidden = selected.size === 0; $('#bulk-count').textContent = `${selected.size} selected`; }
});
document.addEventListener('click', async e => {
  const li = e.target.closest('#palette-list li[data-i]'); if (li) return runPalette(Number(li.dataset.i));
  const b = e.target.closest('button'); if (!b) return;
  const d = b.dataset, find = id => state.jobs.find(j => j.id === id);
  try {
    if (d.cancel) { await api('cancel', {id: d.cancel}); toast('Cancel requested'); await refresh(); }
    if (d.bump) { const j = find(d.bump); await api('priority', {id: d.bump, priority: Math.min(100, j.spec.priority + 10)}); await refresh(); toast('Priority raised'); }
    if (d.drain) { await api('drain', {id: d.drain, drain: d.value === 'true'}); await refresh(); }
    if (d.logs) await openLogs(d.logs);
    if (d.node) openNode(d.node);
    if (d.clone) { fillForm(find(d.clone).spec, find(d.clone).tenant); select('jobs'); $('#submit').scrollIntoView({block: 'center'}); toast('Form filled from workload'); }
    if (d.retry) { const j = find(d.retry); const body = {...j.spec, tenant: j.tenant}; await api('jobs', body); await refresh(); toast('Retry submitted'); }
    if (d.template !== undefined) { const t = prefs.templates[Number(d.template)]; fillForm(t.body); toast('Template applied'); }
    if (d.templateDel !== undefined) { prefs.templates.splice(Number(d.templateDel), 1); savePrefs(); renderTemplates(); }
    if (d.accent) { prefs.accent = d.accent; savePrefs(); }
    if (d.policySet) { const n = state.nodes.find(x => x.id === d.policySet); await api('nodes/policy', {id: d.policySet, ...(n.policy || {}), mode: d.mode}); await refresh(); toast(`${d.policySet}: ${d.mode === 'idle' ? 'runs fleet work only when you are away' : d.mode === 'paused' ? 'fleet work paused' : 'runs fleet work any time'}`); }
    if (d.model) openModel(d.model);
    if (d.chart) { select('training'); showChart(d.chart); }
    if (d.wfCancel) { await api('workflows/cancel', {id: d.wfCancel}); await refresh(); toast('Pipeline cancelled'); }
    if (d.viewGo) select(d.viewGo);
    if (d.undeploy) { await api('cancel', {id: d.undeploy}); await refresh(); toast('Stopping server'); }
    if (d.deploy) {
      const m = state.models.find(x => x.id === d.deploy);
      await api('jobs', {runtime: 'serve', kind: 'serve', modelId: m.id, model: 'serve ' + m.id, memoryGiB: Math.max(1, m.memoryGiB), priority: 70, mode: 'shared', retries: 0, timeoutSeconds: 604800, args: ['--model', m.path, ...(m.runtime === 'mlx' ? ['--engine', 'mlx'] : [])], labels: ['serve'], ...(m.node ? {node: m.node} : {})});
      await refresh(); toast('Deploying. The endpoint appears when the server is healthy.');
    }
  } catch (err) { fail(err); }
});
$('#close-logs').onclick = () => $('#logs').close();
$('#logs').addEventListener('close', () => { clearInterval(logTimer); logJob = null; });
$('#drawer-close').onclick = () => $('#drawer').close(); $('#help-close').onclick = () => $('#help').close();
$('#log-search').oninput = drawLogs; $('#log-wrap').onchange = drawLogs;
$('#log-copy').onclick = async () => { try { await navigator.clipboard.writeText(logLines.map(l => l.text).join('\n')); toast('Logs copied'); } catch { toast('Copy blocked by the browser', 'error'); } };
$('#log-download').onclick = () => download(`gryvia-${(logJob || 'job').slice(0, 10)}.log`, logLines.map(l => `${new Date(l.at * 1000).toISOString()} ${l.text}`).join('\n') + '\n', 'text/plain');
$('#palette-input').oninput = () => { paletteIndex = 0; drawPalette(); };
$('#palette-input').onkeydown = e => {
  if (e.key === 'ArrowDown') { paletteIndex = Math.min(paletteItems.length - 1, paletteIndex + 1); drawPalette(); e.preventDefault(); }
  if (e.key === 'ArrowUp') { paletteIndex = Math.max(0, paletteIndex - 1); drawPalette(); e.preventDefault(); }
  if (e.key === 'Enter') { runPalette(paletteIndex); e.preventDefault(); }
};
$('#set-theme').onchange = e => { prefs.theme = e.target.value; savePrefs(); };
$('#set-window').onchange = e => { prefs.window = e.target.value; savePrefs(); };
$('#set-interval').onchange = e => { prefs.interval = Number(e.target.value); savePrefs(); };
$('#set-reduce').onchange = e => { prefs.reduce = e.target.checked; savePrefs(); };
document.addEventListener('keydown', e => {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); return openPalette(); }
  if (e.target.closest && e.target.closest('input,textarea,select') || e.metaKey || e.ctrlKey || e.altKey) return;
  if (document.querySelector('dialog[open]')) return;
  if (e.key === '/') { e.preventDefault(); select('jobs'); $('#filter').focus(); }
  else if (e.key === 'g') select('guide');
  else if (e.key === 'r') refresh();
  else if (e.key === 'p') togglePause();
  else if (e.key === '?') $('#help').showModal();
  else if (/^[1-9]$/.test(e.key) && VIEWS[Number(e.key) - 1]) select(VIEWS[Number(e.key) - 1]);
});

/* ---------- boot ---------- */
$('#accents').innerHTML = Object.entries(ACCENTS).map(([n, c]) => `<button type="button" class="swatch" role="radio" aria-checked="false" data-accent="${n}" title="${n}" aria-label="${n}" style="background:${c}"></button>`).join('');
applyPrefs();
select((location.hash || '#live').slice(1));
setInterval(tick, 1000);
if (token) { $('#remember').checked = true; refresh(); }
if (!token && !location.hash) select('guide');
tick();
