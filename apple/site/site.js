// SPDX-License-Identifier: BUSL-1.1
// Copyright © 2026 Zyvor AI Labs Private Limited
(function () {
  // reveal on scroll
  var io = 'IntersectionObserver' in window ? new IntersectionObserver(function (es) { es.forEach(function (e) { if (e.isIntersecting) { e.target.classList.add('in'); io.unobserve(e.target); } }); }, { threshold: 0.12 }) : null;
  document.querySelectorAll('.reveal').forEach(function (el) { io ? io.observe(el) : el.classList.add('in'); });

  // copy buttons
  document.querySelectorAll('pre[data-copy]').forEach(function (pre) {
    var b = document.createElement('button'); b.className = 'copy'; b.type = 'button'; b.textContent = 'Copy';
    b.addEventListener('click', function () {
      navigator.clipboard.writeText(pre.dataset.copy).then(function () { b.textContent = 'Copied'; setTimeout(function () { b.textContent = 'Copy'; }, 1500); }, function () { b.textContent = 'Select + ⌘C'; });
    });
    pre.appendChild(b);
  });

  // screenshot tabs (light / dark)
  document.querySelectorAll('[data-shot-tabs]').forEach(function (root) {
    root.querySelectorAll('[role=tab]').forEach(function (tab) {
      tab.addEventListener('click', function () {
        root.querySelectorAll('[role=tab]').forEach(function (t) { t.setAttribute('aria-selected', String(t === tab)); });
        root.querySelectorAll('[data-variant]').forEach(function (el) { el.hidden = el.dataset.variant !== tab.dataset.variant; });
      });
    });
  });

  // GPU core grid: light a changing share of 80 cores, like the Live view
  var grid = document.getElementById('cores-demo');
  if (grid) {
    var n = 80, cells = [];
    for (var i = 0; i < n; i++) { var c = document.createElement('i'); grid.appendChild(c); cells.push(c); }
    var reduce = matchMedia('(prefers-reduced-motion: reduce)').matches, label = document.getElementById('cores-pct');
    function paint(pct) { var lit = Math.round(pct / 100 * n); cells.forEach(function (c, i) { c.classList.toggle('on', i < lit); }); if (label) label.textContent = Math.round(pct) + '%'; }
    paint(62);
    if (!reduce) { var t = 0; setInterval(function () { t += 0.4; paint(55 + 30 * Math.sin(t) + 8 * Math.sin(t * 2.7)); }, 900); }
  }

  // hero: a wave of GPU cores (40 x 6)
  var hg = document.getElementById('hero-grid');
  if (hg) { var f = document.createDocumentFragment(); for (var y = 0; y < 6; y++) for (var x = 0; x < 40; x++) { var c = document.createElement('i'); c.style.setProperty('--x', x); c.style.setProperty('--y', y); f.appendChild(c); } hg.appendChild(f); }

  // scroll story: highlight the step nearest the middle and show its screenshot
  var steps = document.querySelectorAll('.story-steps .step'), shots = document.querySelectorAll('.stage-imgs .shot');
  if ('IntersectionObserver' in window && steps.length) {
    var stepObserver = new IntersectionObserver(function (es) {
      es.forEach(function (e) {
        if (!e.isIntersecting) return;
        var n = e.target.dataset.step;
        steps.forEach(function (s) { s.classList.toggle('active', s.dataset.step === n); });
        shots.forEach(function (s) { s.classList.toggle('on', s.dataset.step === n); });
      });
    }, { rootMargin: '-45% 0px -45% 0px' });
    steps.forEach(function (s) { stepObserver.observe(s); });
    steps[0].classList.add('active');
  }

  // active nav link
  var links = document.querySelectorAll('nav.top a.link[href^="#"]');
  if ('IntersectionObserver' in window && links.length) {
    var sections = Array.prototype.map.call(links, function (a) { return document.querySelector(a.getAttribute('href')); }).filter(Boolean);
    var so = new IntersectionObserver(function (es) { es.forEach(function (e) { if (e.isIntersecting) links.forEach(function (a) { a.setAttribute('aria-current', String(a.getAttribute('href') === '#' + e.target.id)); }); }); }, { rootMargin: '-45% 0px -50% 0px' });
    sections.forEach(function (s) { so.observe(s); });
  }
})();
