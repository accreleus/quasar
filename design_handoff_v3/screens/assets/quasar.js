/* Quasar — shared UI helpers (density toggle, sparkline/line-chart renderers) */
(function () {
  // ----- density -----
  const KEY = 'quasar-density';
  function applyDensity(d) {
    document.body.setAttribute('data-density', d);
    document.querySelectorAll('[data-density-btn]').forEach(b => {
      b.setAttribute('aria-selected', b.dataset.densityBtn === d);
    });
  }
  window.setDensity = function (d) { localStorage.setItem(KEY, d); applyDensity(d); };

  // ----- theme -----
  const TKEY = 'quasar-theme';
  function applyTheme(t) {
    document.documentElement.setAttribute('data-theme', t);
    document.querySelectorAll('[data-theme-label]').forEach(el => el.textContent = t === 'light' ? 'Light' : 'Dark');
    document.querySelectorAll('.up-seg [data-theme-btn]').forEach(b => b.setAttribute('aria-selected', b.dataset.themeBtn === t));
  }
  window.setTheme = function (t) { localStorage.setItem(TKEY, t); applyTheme(t); };
  window.toggleTheme = function (e) { if (e) e.stopPropagation(); const cur = document.documentElement.getAttribute('data-theme') || 'dark'; window.setTheme(cur === 'light' ? 'dark' : 'light'); };

  // ----- user menu -----
  window.toggleUserMenu = function (e) {
    e.stopPropagation();
    const btn = e.currentTarget;
    const pop = btn.parentElement.querySelector('.user-pop');
    const open = pop.classList.toggle('open');
    btn.setAttribute('aria-expanded', open);
  };
  document.addEventListener('click', function () {
    document.querySelectorAll('.user-pop.open').forEach(p => { p.classList.remove('open'); const b = p.parentElement.querySelector('.user-btn'); if (b) b.setAttribute('aria-expanded', 'false'); });
  });

  document.addEventListener('DOMContentLoaded', function () {
    applyDensity(localStorage.getItem(KEY) || 'comfortable');
    const lock = document.documentElement.getAttribute('data-theme-lock');
    applyTheme(lock || localStorage.getItem(TKEY) || 'dark');
    document.querySelectorAll('[data-density-btn]').forEach(b => {
      b.addEventListener('click', () => window.setDensity(b.dataset.densityBtn));
    });
    renderAllCharts();
  });

  // ----- charts (inline SVG, no deps) -----
  function path(points, w, h, pad) {
    const xs = points.map((_, i) => pad + (i / (points.length - 1)) * (w - pad * 2));
    const min = Math.min(...points), max = Math.max(...points);
    const range = (max - min) || 1;
    const ys = points.map(v => h - pad - ((v - min) / range) * (h - pad * 2));
    return xs.map((x, i) => (i ? 'L' : 'M') + x.toFixed(1) + ' ' + ys[i].toFixed(1)).join(' ');
  }
  window.sparkline = function (el) {
    const data = JSON.parse(el.dataset.points);
    const w = el.clientWidth || 120, h = el.clientHeight || 34, pad = 3;
    const color = el.dataset.color || 'var(--accent)';
    const d = path(data, w, h, pad);
    const area = d + ` L ${w - pad} ${h - pad} L ${pad} ${h - pad} Z`;
    el.innerHTML = `<svg viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" style="width:100%;height:100%">
      <defs><linearGradient id="sg${Math.random().toString(36).slice(2,7)}" x1="0" y1="0" x2="0" y2="1">
      <stop offset="0" stop-color="${color}" stop-opacity=".28"/><stop offset="1" stop-color="${color}" stop-opacity="0"/></linearGradient></defs>
      <path d="${area}" fill="${color}" opacity=".12"/>
      <path d="${d}" fill="none" stroke="${color}" stroke-width="1.6" stroke-linejoin="round" stroke-linecap="round"/>
    </svg>`;
  };
  // multi-series line chart with axis labels
  window.lineChart = function (el) {
    const series = JSON.parse(el.dataset.series); // [{color,points}]
    const w = el.clientWidth || 320, h = el.clientHeight || 150, pad = 8, padB = 4;
    const all = series.flatMap(s => s.points);
    const min = 0, max = Math.max(...all) * 1.1 || 1;
    const X = (i, n) => pad + (i / (n - 1)) * (w - pad * 2);
    const Y = v => h - padB - ((v - min) / (max - min)) * (h - padB - pad);
    let grid = '';
    for (let g = 0; g <= 3; g++) { const y = pad + (g / 3) * (h - padB - pad); grid += `<line x1="${pad}" y1="${y}" x2="${w - pad}" y2="${y}" stroke="rgba(255,255,255,.06)" stroke-width="1"/>`; }
    const paths = series.map(s => {
      const d = s.points.map((v, i) => (i ? 'L' : 'M') + X(i, s.points.length).toFixed(1) + ' ' + Y(v).toFixed(1)).join(' ');
      return `<path d="${d}" fill="none" stroke="${s.color}" stroke-width="1.8" stroke-linejoin="round" stroke-linecap="round"/>`;
    }).join('');
    el.innerHTML = `<svg viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" style="width:100%;height:100%">${grid}${paths}</svg>`;
  };
  function renderAllCharts() {
    document.querySelectorAll('[data-spark]').forEach(window.sparkline);
    document.querySelectorAll('[data-linechart]').forEach(window.lineChart);
  }
  window.addEventListener('resize', () => { clearTimeout(window.__crt); window.__crt = setTimeout(renderAllCharts, 150); });
})();
