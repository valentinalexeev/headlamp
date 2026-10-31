// headlamp console shell: theme control, sidebar counts, the timed strip and data freshness.
// Needs hl.js first. Everything that came from the network goes in through textContent only.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var KEY = 'hl-theme';
  var THEMES = ['system', 'light', 'dark'];
  var ATTENTION_URL = '/_attention.json';
  var POLL_MS = 15000;
  var COUNTED = { critical: 1, waiting: 1, timed: 1 };

  // ---- theme ----
  function applyTheme(choice) {
    var root = document.documentElement;
    if (choice === 'light' || choice === 'dark') { root.setAttribute('data-hl-theme', choice); }
    else { root.removeAttribute('data-hl-theme'); }
  }

  function currentTheme() {
    var v = document.documentElement.getAttribute('data-hl-theme');
    return v === 'light' || v === 'dark' ? v : 'system';
  }

  // pickTheme applies the choice first, then tries to remember it; a missing or throwing
  // localStorage costs only the memory.
  function pickTheme(choice) {
    if (THEMES.indexOf(choice) < 0) { choice = 'system'; }
    applyTheme(choice);
    try {
      if (choice === 'system') { window.localStorage.removeItem(KEY); }
      else { window.localStorage.setItem(KEY, choice); }
    } catch (e) { /* the choice still applies to this page */ }
    return choice;
  }

  function syncThemeButtons(buttons) {
    var cur = currentTheme();
    buttons.forEach(function (b) {
      var on = b.getAttribute('data-theme') === cur;
      b.className = on ? 'on' : '';
      b.setAttribute('aria-pressed', on ? 'true' : 'false');
    });
  }

  // ---- attention data ----
  function timedItems(data) {
    var items = data && Array.isArray(data.items) ? data.items : [];
    return items.filter(function (it) { return it && it.level === 'timed'; });
  }

  // countsBySource: { source: { n, crit } } for the items that need a person.
  function countsBySource(data) {
    var out = Object.create(null);
    var items = data && Array.isArray(data.items) ? data.items : [];
    items.forEach(function (it) {
      if (!it || !it.source || !COUNTED[it.level]) { return; }
      var c = out[it.source] || (out[it.source] = { n: 0, crit: false });
      c.n += 1;
      if (it.level === 'critical') { c.crit = true; }
    });
    return out;
  }

  function applyCounts(entries, data) {
    var counts = countsBySource(data);
    entries.forEach(function (e) {
      var c = counts[e.source] || null;
      if (!c) { e.el.hidden = true; e.el.textContent = ''; e.el.className = 'navcount'; return; }
      e.el.hidden = false;
      e.el.textContent = String(c.n);
      e.el.className = c.crit ? 'navcount crit' : 'navcount';
      e.el.setAttribute('title', c.n + (c.crit ? ' need attention, some at risk' : ' need attention'));
    });
  }

  // needsReload: the page was rendered before this change was applied, so it cannot prove the
  // operator can still reach the console through the new rules. Seconds on both sides.
  // Unknown times also count as needing a reload.
  function needsReload(item, loadedSec) {
    var t = Date.parse(item && item.applied_at);
    // Fail closed: without both times the page cannot prove it is newer than the change.
    if (isNaN(t) || typeof loadedSec !== 'number' || !isFinite(loadedSec)) { return true; }
    // Whole seconds on both sides: the same second cannot prove the page is newer.
    return loadedSec <= Math.floor(t / 1000);
  }

  // loadedSeconds: when the server rendered this page, in unix seconds (NaN when unknown). It is the
  // other half of needsReload; pages with their own Confirm button read it from here too.
  function loadedSeconds(strip) {
    strip = strip || document.querySelector('[role="status"][data-hl-loaded]');
    var v = strip && strip.getAttribute('data-hl-loaded');
    return v ? Number(v) : NaN;
  }

  // onPage: the page already shows this local path (the query is ignored), so a link to it is noise.
  function onPage(path) {
    var here = typeof window !== 'undefined' && window.location ? window.location.pathname : '';
    return typeof path === 'string' && !!here && path.split('?')[0] === here;
  }

  function countdown(deadlineIso, nowMs) {
    var t = Date.parse(deadlineIso);
    if (isNaN(t)) { return ''; }
    var s = Math.max(0, Math.round((t - nowMs) / 1000));
    var m = Math.floor(s / 60);
    var r = s % 60;
    return (m < 10 ? '0' : '') + m + ':' + (r < 10 ? '0' : '') + r;
  }

  function localPath(p) {
    return typeof p === 'string' && p.charAt(0) === '/' && p.charAt(1) !== '/' && p.indexOf('\\') < 0;
  }

  function clear(node) {
    while (node.firstChild) { node.removeChild(node.firstChild); }
  }

  function button(cls, label, iconName) {
    var b = hl.el('button', cls);
    b.setAttribute('type', 'button');
    var ic = iconName ? hl.icon(iconName) : null;
    if (ic) { b.appendChild(ic); }
    b.appendChild(hl.el('span', null, label));
    return b;
  }

  // controller owns what the shell shows. parts: { strip, fresh, top, lamp, counts: [{source, el}] }.
  // opts: { api, now, reload } (all optional; the defaults are the real ones).
  function controller(parts, opts) {
    opts = opts || {};
    var api = opts.api || hl.api;
    var now = opts.now || function () { return Date.now(); };
    var reload = opts.reload || function () { window.location.reload(); };
    var st = { sig: null, data: null, lastOk: 0, fails: 0, error: '', busy: false, countEls: [], poller: null };

    function loaded() { return parts.strip ? loadedSeconds(parts.strip) : NaN; }

    function act(method, url, body) {
      st.busy = true;
      st.error = '';
      renderStrip();
      return api(method, url, body).then(function () {
        st.busy = false;
        return ctl.poll();
      }, function (err) {
        st.busy = false;
        st.error = hl.plain(err && err.message ? err.message : 'request failed', 200);
        renderStrip();
      });
    }

    function renderStrip() {
      var strip = parts.strip;
      if (!strip) { return; }
      var items = timedItems(st.data);
      var loadedSec = loaded();
      // Rebuild only when what is shown changes, so focus and the live region survive a poll.
      // The deadline is left out of the signature: the countdown is updated in place.
      var sig = JSON.stringify([items.map(function (it) {
        return [it.id, it.title, it.change_id, it.action_label, it.action_path, !!it.deadline, needsReload(it, loadedSec)];
      }), st.busy, st.error]);
      if (sig === st.sig) {
        var withDeadline = items.filter(function (it) { return it.deadline; });
        st.countEls.forEach(function (c, i) {
          c.deadline = withDeadline[i].deadline;
          c.el.textContent = countdown(c.deadline, now());
        });
        return;
      }
      st.sig = sig;
      clear(strip);
      st.countEls = [];
      if (!items.length) { strip.hidden = true; return; }
      strip.hidden = false;
      items.forEach(function (it) {
        var g = hl.el('span', 'g warn');
        g.setAttribute('aria-hidden', 'true');
        strip.appendChild(g);
        var text = hl.el('span', null, hl.plain(it.title, 200));
        strip.appendChild(text);
        if (it.deadline) {
          var lead = hl.el('span', 't2', 'Reverts in');
          var c = hl.el('span', 'count', countdown(it.deadline, now()));
          strip.appendChild(lead);
          strip.appendChild(c);
          st.countEls.push({ el: c, deadline: it.deadline });
        }
        if (it.change_id) {
          var id = it.change_id;
          var primary;
          if (needsReload(it, loadedSec)) {
            primary = button('btn primary sm', 'Reload to confirm', null);
            primary.addEventListener('click', function () { reload(); });
          } else {
            primary = button('btn primary sm', 'Confirm', 'check');
            primary.addEventListener('click', function () { act('POST', '/policy/confirm', { change_id: id }); });
          }
          var revert = button('btn sm', 'Revert now', 'undo');
          revert.addEventListener('click', function () { act('POST', '/policy/revert-now', { change_id: id }); });
          primary.disabled = st.busy;
          revert.disabled = st.busy;
          strip.appendChild(primary);
          strip.appendChild(revert);
        } else if (it.action_label && localPath(it.action_path) && !onPage(it.action_path)) {
          var a = hl.el('a', 'btn primary sm', hl.plain(it.action_label, 60));
          a.setAttribute('href', it.action_path);
          strip.appendChild(a);
        }
      });
      if (st.error) { strip.appendChild(hl.el('span', 'small t2', st.error)); }
    }

    function renderFresh() {
      var f = parts.fresh;
      if (!f) { return; }
      clear(f);
      var text = '';
      if (st.fails >= 2) { f.appendChild(hl.status('warn', 'Data may be stale')); }
      else if (st.lastOk) {
        text = 'Updated ' + hl.relTime(new Date(st.lastOk).toISOString(), now());
        f.appendChild(hl.el('span', null, text));
      }
      if (parts.top && f.firstChild) { parts.top.hidden = false; }
    }

    function render() {
      if (parts.counts) { applyCounts(parts.counts, st.data); }
      if (parts.lamp) {
        var any = st.data && Array.isArray(st.data.items) && st.data.items.some(function (it) { return it && COUNTED[it.level]; });
        parts.lamp.className = any ? 'lamp' : 'lamp clear';
      }
      renderStrip();
      renderFresh();
    }

    var ctl = {
      state: st,
      render: render,
      // poll reads the attention data. A 404 means the endpoint is not there (yet): stay quiet.
      // Any other failure counts; two in a row mark the data stale and keep the last numbers.
      poll: function () {
        return api('GET', ATTENTION_URL).then(function (data) {
          st.data = data || {};
          st.lastOk = now();
          st.fails = 0;
          render();
        }, function (err) {
          if (err && err.status === 404) { return; }
          st.fails += 1;
          renderFresh();
        });
      },
      // tick refreshes the clocks (freshness text, countdowns) without a request.
      tick: function () {
        st.countEls.forEach(function (c) { c.el.textContent = countdown(c.deadline, now()); });
        renderFresh();
      },
    };
    return ctl;
  }

  function list(sel) {
    return Array.prototype.slice.call(document.querySelectorAll(sel));
  }

  function init() {
    var nav = document.querySelector('[data-nc-nav]');
    if (!nav) { return; }

    var buttons = list('[data-theme]');
    syncThemeButtons(buttons);
    buttons.forEach(function (b) {
      b.addEventListener('click', function () {
        pickTheme(b.getAttribute('data-theme'));
        syncThemeButtons(buttons);
      });
    });
    window.addEventListener('storage', function (e) {
      if (e && (e.key === KEY || e.key === null)) {
        applyTheme(e.newValue === 'light' || e.newValue === 'dark' ? e.newValue : 'system');
        syncThemeButtons(buttons);
      }
    });

    var toggle = document.querySelector('[data-nav-toggle]');
    var side = document.querySelector('nav[data-nc-nav]');
    if (toggle && side) {
      var base = side.className;
      toggle.addEventListener('click', function () {
        var open = toggle.getAttribute('aria-expanded') !== 'true';
        toggle.setAttribute('aria-expanded', open ? 'true' : 'false');
        side.className = open ? base + ' open' : base;
      });
    }

    var counts = list('[data-source]').map(function (a) {
      return { source: a.getAttribute('data-source'), el: a.querySelector('.navcount') };
    }).filter(function (e) { return e.el; });
    var ctl = controller({
      strip: document.querySelector('[role="status"][data-hl-loaded]'),
      fresh: document.querySelector('[data-fresh]'),
      top: document.querySelector('[data-top]'),
      lamp: document.querySelector('.lamp'),
      counts: counts,
    });
    var poller = hl.poll(function () { return ctl.poll(); }, POLL_MS);
    ctl.state.poller = poller;
    setInterval(function () { if (!document.hidden) { ctl.tick(); } }, 1000);
  }

  hl.shell = {
    pickTheme: pickTheme, applyTheme: applyTheme, currentTheme: currentTheme,
    syncThemeButtons: syncThemeButtons, timedItems: timedItems, countsBySource: countsBySource,
    applyCounts: applyCounts, needsReload: needsReload, loadedSeconds: loadedSeconds, countdown: countdown, controller: controller,
    init: init,
  };
  init();
})();
