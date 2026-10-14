// SPDX-License-Identifier: BUSL-1.1
// Copyright © 2026 Zyvor AI Labs Private Limited
// Applies the saved theme before first paint (loaded in <head>), then wires the toggle.
(function () {
  document.documentElement.classList.add('js');   // reveal-on-scroll only hides content when JavaScript runs
  try { var t = localStorage.getItem('gryvia-site-theme'); if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t; } catch (e) {}
  document.addEventListener('DOMContentLoaded', function () {
    var b = document.getElementById('theme');
    if (!b) return;
    b.addEventListener('click', function () {
      var dark = document.documentElement.dataset.theme ? document.documentElement.dataset.theme === 'dark' : matchMedia('(prefers-color-scheme: dark)').matches;
      var next = dark ? 'light' : 'dark';
      document.documentElement.dataset.theme = next;
      try { localStorage.setItem('gryvia-site-theme', next); } catch (e) {}
    });
  });
})();
