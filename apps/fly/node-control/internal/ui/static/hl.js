// headlamp console: the one shared script. It defines window.hl and nothing else global.
// Text that came from the network goes in through textContent only; markup is never built from strings.
(function () {
  'use strict';

  var SVGNS = 'http://www.w3.org/2000/svg';

  // The same outlines as icons.go (a Go test keeps the two in step).
  var ICONS = {
    overview: 'M3 3h7v9H3zM14 3h7v5h-7zM14 12h7v9h-7zM3 16h7v5H3z',
    nodes: 'M4 3h16a1 1 0 0 1 1 1v5a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1zM4 14h16a1 1 0 0 1 1 1v5a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1v-5a1 1 0 0 1 1-1zM7 6.5h.01M7 17.5h.01',
    devices: 'M7 2h10a2 2 0 0 1 2 2v16a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2zM12 18h.01',
    funnel: 'M2 12a10 10 0 1 0 20 0a10 10 0 1 0 -20 0M2 12h20M12 2a15 15 0 0 1 0 20M12 2a15 15 0 0 0 0 20',
    tasks: 'm3 17 2 2 4-4M3 7l2 2 4-4M13 6h8M13 12h8M13 18h8',
    sessions: 'M3 4h18a1 1 0 0 1 1 1v12a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1zM8 22h8M12 18v4M10 8l5 3-5 3z',
    policy: 'M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z',
    key: 'M2 15.5a5.5 5.5 0 1 0 11 0a5.5 5.5 0 1 0 -11 0M21 2l-9.6 9.6M15.5 7.5l3 3L22 7l-3-3',
    activity: 'M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8M3 3v5h5M12 7v5l4 2',
    search: 'M3 11a8 8 0 1 0 16 0a8 8 0 1 0 -16 0M21 21l-4.3-4.3',
    plus: 'M5 12h14M12 5v14',
    system: 'M3 3h18a1 1 0 0 1 1 1v12a1 1 0 0 1-1 1H3a1 1 0 0 1-1-1V4a1 1 0 0 1 1-1zM8 21h8M12 17v4',
    sun: 'M8 12a4 4 0 1 0 8 0a4 4 0 1 0 -8 0M12 2v2M12 20v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M2 12h2M20 12h2M6.34 17.66l-1.41 1.41M19.07 4.93l-1.41 1.41',
    moon: 'M12 3a6 6 0 0 0 9 9 9 9 0 1 1-9-9z',
    play: 'M6 3l14 9-14 9z',
    stop: 'M6 6h12v12H6z',
    ban: 'M2 12a10 10 0 1 0 20 0a10 10 0 1 0 -20 0M4.93 4.93l14.14 14.14',
    trash: 'M3 6h18M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2',
    extend: 'M8 2v4M16 2v4M21 11V6a2 2 0 0 0-2-2H5a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h7M3 10h18M16 19h6M19 16v6',
    link: 'M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71',
    copy: 'M10 8h10a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H10a2 2 0 0 1-2-2V10a2 2 0 0 1 2-2zM4 16a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h10a2 2 0 0 1 2 2',
    close: 'M18 6 6 18M6 6l12 12',
    external: 'M15 3h6v6M10 14 21 3M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6',
    menu: 'M4 6h16M4 12h16M4 18h16',
    check: 'M20 6 9 17l-5-5',
    undo: 'M3 7v6h6M21 17a9 9 0 0 0-9-9 9 9 0 0 0-6 2.3L3 13',
    edit: 'M12 20h9M16.5 3.5a2.12 2.12 0 0 1 3 3L7 19l-4 1 1-4z'
  };

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) { e.className = cls; }
    if (text !== undefined && text !== null) { e.textContent = text; }
    return e;
  }

  // icon returns an SVG element, or null for a name that is not in ICONS. Every caller tests the result and
  // simply leaves the icon out, so an unknown name never breaks a page.
  function icon(name) {
    var d = Object.prototype.hasOwnProperty.call(ICONS, name) ? ICONS[name] : null;
    if (d === null) { return null; }
    var s = document.createElementNS(SVGNS, 'svg');
    var attrs = { 'class': 'ico', width: '16', height: '16', viewBox: '0 0 24 24', fill: 'none',
      stroke: 'currentColor', 'stroke-width': '1.75', 'stroke-linecap': 'round',
      'stroke-linejoin': 'round', 'aria-hidden': 'true' };
    Object.keys(attrs).forEach(function (k) { s.setAttribute(k, attrs[k]); });
    var p = document.createElementNS(SVGNS, 'path');
    p.setAttribute('d', d);
    s.appendChild(p);
    return s;
  }

  function relTime(iso, nowMs) {
    if (!iso) { return ''; }
    var t = Date.parse(iso);
    if (isNaN(t)) { return ''; }
    var diff = Math.round((t - nowMs) / 1000);
    var future = diff > 0;
    var s = Math.abs(diff);
    var text;
    if (s < 5) { return 'just now'; }
    if (s < 60) { text = s + ' s'; }
    else if (s < 3600) { text = Math.floor(s / 60) + ' min'; }
    else if (s < 86400) { text = Math.floor(s / 3600) + ' h ' + Math.floor((s % 3600) / 60) + ' min'; }
    else { text = Math.floor(s / 86400) + ' d ' + Math.floor((s % 86400) / 3600) + ' h'; }
    return future ? 'in ' + text : text + ' ago';
  }

  function fmtBytes(n) {
    if (typeof n !== 'number' || !isFinite(n) || n < 0) { return ''; }
    if (n < 1024) { return Math.round(n) + ' B'; }
    var units = ['KB', 'MB', 'GB', 'TB'];
    var v = n / 1024;
    var i = 0;
    // 1048575 B rounds to "1024.0 KB": show it as 1.0 MB instead.
    while (i < units.length - 1 && v.toFixed(1) === '1024.0') { v = v / 1024; i++; }
    while (i < units.length - 1 && v >= 1024) { v = v / 1024; i++; }
    return v.toFixed(1) + ' ' + units[i];
  }

  // plain: a label from outside, safe to show. Control, bidi-override, zero-width and line
  // separator characters are dropped, then the text is cut to n characters.
  var UNSAFE = /[\u0000-\u001f\u007f-\u009f\u061c\u200b-\u200f\u2028-\u202e\u2060-\u206f\ufeff]/g;
  function plain(s, n) {
    if (s === undefined || s === null) { return ''; }
    var chars = Array.from(String(s).replace(UNSAFE, ''));
    return (n >= 0 ? chars.slice(0, n) : chars).join('');
  }

  function errorMessage(status, body) {
    if (body && typeof body.error === 'string' && body.error) { return plain(body.error, 200); }
    if (body && typeof body.detail === 'string' && body.detail) { return plain(body.detail, 200); }
    return 'request failed (HTTP ' + status + ')';
  }

  // API_TIMEOUT_MS is past the server's own 60 s write limit, so only a request the server can no longer answer ends here.
  var API_TIMEOUT_MS = 70000;

  // api resolves with the parsed JSON body (null when empty) and rejects with an Error that
  // carries .status and a readable message, or .timeout when the request never ended. Cookies of this
  // origin only; no secrets in the URL. timeoutMs is for tests.
  function api(method, url, body, timeoutMs) {
    var opts = { method: method, credentials: 'same-origin', headers: { Accept: 'application/json' } };
    var abort = typeof AbortController === 'function' ? new AbortController() : null;
    var timedOut = false;
    var timer = null;
    if (abort) {
      opts.signal = abort.signal;
      timer = setTimeout(function () { timedOut = true; abort.abort(); }, timeoutMs || API_TIMEOUT_MS);
    }
    if (body !== undefined && body !== null) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body);
    }
    return fetch(url, opts).then(function (res) {
      return res.text().then(function (txt) {
        var data = null;
        if (txt) { try { data = JSON.parse(txt); } catch (e) { data = null; } }
        if (!res.ok) {
          var err = new Error(errorMessage(res.status, data));
          err.status = res.status;
          err.data = data;
          throw err;
        }
        return data;
      });
    }).then(function (data) {
      clearTimeout(timer);
      return data;
    }, function (e) {
      clearTimeout(timer);
      if (!timedOut) { throw e; }
      var err = new Error('The request timed out. It may still have gone through, so check before trying again.');
      err.timeout = true;
      throw err;
    });
  }

  // poll runs fn now and again ms after each run finishes (never overlapping), and pauses while
  // the tab is hidden. It returns { stop, now }; now() runs fn again at once.
  function poll(fn, ms) {
    var timer = null, stopped = false, running = false, again = false;
    function schedule() {
      if (stopped) { return; }
      clearTimeout(timer);
      timer = setTimeout(tick, ms);
    }
    function tick() {
      if (stopped) { return; }
      if (typeof document !== 'undefined' && document.hidden) { schedule(); return; }
      if (running) { again = true; return; }
      running = true;
      var done = function () {
        running = false;
        if (again) { again = false; tick(); return; }
        schedule();
      };
      var r;
      try { r = fn(); } catch (e) { done(); return; }
      if (r && typeof r.then === 'function') { r.then(done, done); } else { done(); }
    }
    tick();
    return {
      stop: function () { stopped = true; clearTimeout(timer); },
      now: function () { clearTimeout(timer); tick(); },
    };
  }

  var LEVELS = { crit: 1, warn: 1, ok: 1, off: 1, good: 1 };
  // status: shape and word together, never colour alone.
  function status(level, word) {
    var l = Object.prototype.hasOwnProperty.call(LEVELS, level) ? level : 'ok';
    var s = el('span', 'st ' + l);
    var g = el('span', 'g ' + l);
    g.setAttribute('aria-hidden', 'true');
    s.appendChild(g);
    s.appendChild(el('span', null, word));
    return s;
  }

  function queryParam(k) {
    try { return new URLSearchParams(window.location.search).get(k); } catch (e) { return null; }
  }

  function setQueryParam(k, v) {
    try {
      var u = new URL(window.location.href);
      if (v === null || v === undefined || v === '') { u.searchParams.delete(k); }
      else { u.searchParams.set(k, v); }
      window.history.replaceState(null, '', u.pathname + u.search + u.hash);
    } catch (e) { /* no history: the page still works */ }
  }

  function arr(v) { return Array.isArray(v) ? v : []; }
  function obj(v) { return v && typeof v === 'object' && !Array.isArray(v) ? v : {}; }
  function clear(node) { while (node.firstChild) { node.removeChild(node.firstChild); } }

  // button: a plain button with an optional icon and click handler.
  function button(text, iconName, cls, onClick) {
    var b = el('button', 'btn' + (cls ? ' ' + cls : ''));
    b.setAttribute('type', 'button');
    var ic = iconName ? icon(iconName) : null;
    if (ic) { b.appendChild(ic); }
    b.appendChild(el('span', null, text));
    if (onClick) { b.addEventListener('click', onClick); }
    return b;
  }

  // focusKey: the data-fk of the focused control when it sits inside box, else null. A page redraws box from
  // scratch and calls refocus with this key afterwards, so keyboard focus survives the redraw.
  function focusKey(box) {
    var ae = typeof document !== 'undefined' ? document.activeElement : null;
    for (var n = ae; n; n = n.parentNode) {
      if (n === box) { return ae.getAttribute ? ae.getAttribute('data-fk') : null; }
    }
    return null;
  }

  // refocus puts focus on the control under box that carries key. A control that is disabled now (a button that
  // was just pressed) cannot take focus; fallback, when given, names the one that gets it instead.
  function refocus(box, key, fallback) {
    if (!key) { return; }
    var stack = [box];
    while (stack.length) {
      var n = stack.pop();
      if (n.getAttribute && n.getAttribute('data-fk') === key && !n.disabled) { n.focus(); return; }
      (n.children ? Array.prototype.slice.call(n.children) : []).forEach(function (c) { stack.push(c); });
    }
    if (fallback) { refocus(box, fallback); }
  }

  // confirmPanel builds an inline confirmation that names the object and the consequence. It
  // replaces confirm() and prompt(). opts: title, object, consequence, confirmLabel,
  // cancelLabel, icon, into (optional parent), onConfirm, onCancel. Focus starts on Cancel and, when the panel is
  // cancelled or dismissed with Esc, goes back to the control that was focused when it opened (if that is still on the page).
  function confirmPanel(opts) {
    var opener = typeof document !== 'undefined' ? document.activeElement : null;
    var panel = el('div', 'note crit confirm');
    panel.setAttribute('role', 'group');
    panel.setAttribute('aria-label', plain(opts.title || 'Confirm', 120));
    if (opts.title) { panel.appendChild(el('h2', null, opts.title)); }
    if (opts.object) {
      var line = el('p');
      line.appendChild(el('strong', null, opts.object));
      panel.appendChild(line);
    }
    if (opts.consequence) { panel.appendChild(el('p', 't2', opts.consequence)); }

    var buttons = el('div', 'rowbtns');
    var cancel = el('button', 'btn', opts.cancelLabel || 'Cancel');
    cancel.setAttribute('type', 'button');
    var ok = el('button', 'btn danger');
    ok.setAttribute('type', 'button');
    var ic = opts.icon ? icon(opts.icon) : null;
    if (ic) { ok.appendChild(ic); }
    ok.appendChild(el('span', null, opts.confirmLabel || 'Confirm'));
    buttons.appendChild(cancel);
    buttons.appendChild(ok);
    panel.appendChild(buttons);

    function close(cb) {
      panel.remove();
      if (typeof cb === 'function') { cb(); }
    }
    function dismiss() {
      // First, so a caller that redraws and places focus itself in onCancel has the last word.
      if (opener && opener.isConnected && typeof opener.focus === 'function') { opener.focus(); }
      close(opts.onCancel);
    }
    cancel.addEventListener('click', dismiss);
    ok.addEventListener('click', function () { close(opts.onConfirm); });
    panel.addEventListener('keydown', function (ev) {
      if (ev && ev.key === 'Escape') { dismiss(); }
    });

    if (opts.into) { opts.into.appendChild(panel); }
    cancel.focus();
    if (!opts.into && typeof setTimeout === 'function') {
      // The caller usually attaches the panel just after this returns.
      setTimeout(function () { if (panel.isConnected) { cancel.focus(); } }, 0);
    }
    return panel;
  }

  window.hl = {
    el: el, icon: icon, relTime: relTime, fmtBytes: fmtBytes, plain: plain, api: api, poll: poll,
    status: status, queryParam: queryParam, setQueryParam: setQueryParam, confirmPanel: confirmPanel,
    arr: arr, obj: obj, clear: clear, button: button, focusKey: focusKey, refocus: refocus,
  };
})();
