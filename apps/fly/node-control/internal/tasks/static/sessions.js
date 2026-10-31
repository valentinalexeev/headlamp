// headlamp console: the Browser sessions page. Needs /_ui/hl.js first (the shell loads it). Node names come from
// the tailnet and are never markup. A session link carries a token: it goes into the Open link's href and nowhere
// else (not the page URL, not a message, not an attribute other than that href).
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var POLL_MS = 5000;
  var COLUMNS = 4;
  var DEFAULT_TTL = 3600;
  var TTL_CHOICES = [[3600, '1 hour'], [1800, '30 minutes'], [14400, '4 hours']];
  var ORDER = { running: 0, idle: 1, unavailable: 2 };

  function arr(v) { return Array.isArray(v) ? v : []; }
  function obj(v) { return v && typeof v === 'object' && !Array.isArray(v) ? v : {}; }
  function clear(node) { while (node.firstChild) { node.removeChild(node.firstChild); } }
  function label(v, n) { return hl.plain(v, n || 100); }
  function keyed(el, key) { el.setAttribute('data-fk', key); return el; }

  // Time left until iso, in words: "1 h 5 min", "42 min", "45 s". "Ending now" once it has passed, '' when unknown.
  function timeLeft(iso, nowMs) {
    if (!iso) { return ''; }
    var t = Date.parse(iso);
    if (isNaN(t)) { return ''; }
    var s = Math.round((t - nowMs) / 1000);
    if (s <= 0) { return 'Ending now'; }
    if (s < 60) { return s + ' s'; }
    var m = Math.floor(s / 60);
    if (m < 60) { return m + ' min'; }
    return Math.floor(m / 60) + ' h ' + (m % 60) + ' min';
  }

  // Only a plain http link is ever put in an href.
  function safeUrl(u) {
    return typeof u === 'string' && /^http:\/\//.test(u) ? u : '';
  }

  // sessionRows: one display row per node, running first, then idle, then unavailable, each by name.
  function sessionRows(nodes) {
    var rows = [];
    arr(nodes).forEach(function (n) {
      n = obj(n);
      if (typeof n.id !== 'string' || !n.id) { return; }
      var s = obj(n.session);
      var kind = !n.available ? 'unavailable' : (s.state === 'running' ? 'running' : 'idle');
      var level = kind === 'running' ? 'ok' : 'off';
      var word = kind === 'running' ? 'Running' : kind === 'unavailable' ? 'Unavailable' : 'No session';
      if (kind === 'idle' && s.state === 'ended') { word = 'Ended' + (s.reason ? ' (' + label(s.reason, 40) + ')' : ''); }
      rows.push({
        id: n.id, name: label(n.id, 100), kind: kind, level: level, word: word,
        href: kind === 'running' ? safeUrl(n.url) : '', expires: kind === 'running' ? String(s.expires_at || '') : ''
      });
    });
    rows.sort(function (a, b) { return (ORDER[a.kind] - ORDER[b.kind]) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0); });
    return rows;
  }

  function button(text, iconName, cls, onClick) {
    var b = hl.el('button', 'btn' + (cls ? ' ' + cls : ''));
    b.setAttribute('type', 'button');
    var ic = iconName ? hl.icon(iconName) : null;
    if (ic) { b.appendChild(ic); }
    b.appendChild(hl.el('span', null, text));
    if (onClick) { b.addEventListener('click', onClick); }
    return b;
  }

  function focusKey(box) {
    var ae = typeof document !== 'undefined' ? document.activeElement : null;
    for (var n = ae; n; n = n.parentNode) {
      if (n === box) { return ae.getAttribute ? ae.getAttribute('data-fk') : null; }
    }
    return null;
  }
  function refocus(box, key) {
    if (!key) { return; }
    var stack = [box];
    while (stack.length) {
      var n = stack.pop();
      if (n.getAttribute && n.getAttribute('data-fk') === key && !n.disabled) { n.focus(); return; }
      (n.children ? Array.prototype.slice.call(n.children) : []).forEach(function (c) { stack.push(c); });
    }
  }

  // mount wires the page's elements to the data. deps (all optional): now() for the clock.
  function mount(els, deps) {
    deps = deps || {};
    var now = deps.now || function () { return Date.now(); };
    var st = { loaded: false, failed: '', rows: [], busy: false, open: null, ttl: {}, cells: {}, tableSig: null };
    var ctrl = {};

    function setError(text) { els.error.textContent = text; els.error.hidden = !text; }
    function setNotice(text) { els.notice.textContent = text; els.notice.hidden = !text; }

    function summary() {
      if (!st.loaded) { return st.failed ? 'Could not load the sessions. Retrying.' : ''; }
      var n = st.rows.filter(function (r) { return r.kind === 'running'; }).length;
      var text = (n === 0 ? 'None running' : n + ' running') + '. Sessions end by themselves when their time runs out.';
      if (st.failed) { text += ' The data may be out of date.'; }
      return text;
    }

    // The time left moves with the clock: only these cells change between refreshes, so the rows stay put.
    function paintTimes(at) {
      st.rows.forEach(function (r) {
        var td = st.cells[r.id];
        if (!td) { return; }
        var text = r.kind === 'running' ? timeLeft(r.expires, at) : '';
        td.textContent = text || 'n/a';
        td.className = text ? 'mono t2' : 't3';
        if (text && r.expires) { td.setAttribute('title', label(r.expires, 40)); } else { td.removeAttribute('title'); }
      });
    }

    // ----- actions

    function closeOpen() { st.open = null; render(true); }

    function run(r, what, fn, okText, failWord) {
      if (st.busy) { return; }
      st.busy = true;
      setError('');
      setNotice('');
      st.open = null;
      render(true);
      return fn().then(function () {
        st.busy = false;
        setNotice(okText);
        return ctrl.reload();
      }, function (err) {
        st.busy = false;
        setError(failWord + ' failed on ' + r.name + ': ' + label(err && err.message, 200));
        render(true);
      });
    }

    function doStart(r) {
      var ttl = st.ttl[r.id] || DEFAULT_TTL;
      run(r, 'start', function () { return hl.api('POST', '/sessions/' + encodeURIComponent(r.id) + '/start', { ttl_seconds: ttl }); },
        'Started a session on ' + r.name + '.', 'Start');
    }
    function doStop(r) {
      run(r, 'stop', function () { return hl.api('POST', '/sessions/' + encodeURIComponent(r.id) + '/stop', {}); },
        'Stopped the session on ' + r.name + '.', 'Stop');
    }

    function openStop(r) {
      if (st.busy || (st.open && st.open.key === r.id)) { return; }
      st.open = { key: r.id, el: null, cell: null };
      render(true);
      if (st.open && st.open.cell) {
        st.open.el = hl.confirmPanel({
          title: 'Stop the session on ' + r.name + '?', object: r.name,
          consequence: 'The browser session on ' + r.name + ' ends now and anyone using it is disconnected. Start a new one to continue.',
          confirmLabel: 'Stop session on ' + r.name, cancelLabel: 'Keep it running', icon: 'stop', into: st.open.cell,
          onConfirm: function () { doStop(r); }, onCancel: closeOpen
        });
      }
    }

    // ----- the table

    function actionsOf(r, box) {
      if (r.kind === 'running') {
        if (r.href) {
          var a = hl.el('a', 'btn sm');
          var ic = hl.icon('external');
          if (ic) { a.appendChild(ic); }
          a.appendChild(hl.el('span', null, 'Open'));
          a.setAttribute('href', r.href);
          a.setAttribute('target', '_blank');
          a.setAttribute('rel', 'noopener noreferrer');
          a.setAttribute('aria-label', 'Open the session on ' + r.name);
          box.appendChild(keyed(a, 'open:' + r.id));
        }
        var stop = keyed(button('Stop', 'stop', 'sm quiet', function () { openStop(r); }), 'stop:' + r.id);
        stop.setAttribute('aria-label', 'Stop the session on ' + r.name);
        stop.disabled = st.busy;
        box.appendChild(stop);
      } else if (r.kind === 'idle') {
        var sel = keyed(hl.el('select', 'input sm'), 'ttl:' + r.id);
        sel.setAttribute('aria-label', 'Session length for ' + r.name);
        TTL_CHOICES.forEach(function (c) {
          var o = hl.el('option', null, c[1]);
          o.setAttribute('value', String(c[0]));
          sel.appendChild(o);
        });
        sel.value = String(st.ttl[r.id] || DEFAULT_TTL);
        sel.disabled = st.busy;
        sel.addEventListener('change', function () { st.ttl[r.id] = parseInt(sel.value, 10) || DEFAULT_TTL; });
        box.appendChild(sel);
        var start = keyed(button('Start', 'play', 'sm', function () { doStart(r); }), 'start:' + r.id);
        start.setAttribute('aria-label', 'Start a session on ' + r.name);
        start.disabled = st.busy;
        box.appendChild(start);
      }
    }

    function renderTable(force) {
      var rows = st.rows;
      if (st.open && !rows.some(function (r) { return r.id === st.open.key && r.kind === 'running'; })) { st.open = null; }
      var sig = JSON.stringify([rows.map(function (r) { return [r.id, r.kind, r.word, r.href]; }), st.open && st.open.key, st.busy, st.ttl]);
      if (!force && st.open && st.open.el) { return; }
      if (!force && sig === st.tableSig) { return; }
      st.tableSig = sig;
      var fk = focusKey(els.rows);
      clear(els.rows);
      st.cells = {};
      if (st.open) { st.open.cell = null; }
      rows.forEach(function (r) {
        var tr = hl.el('tr', st.open && st.open.key === r.id ? 'sel' : '');
        tr.appendChild(hl.el('td', 'mono name', r.name));
        var state = hl.el('td');
        state.appendChild(hl.status(r.level, r.word));
        tr.appendChild(state);
        var left = hl.el('td', 't3', 'n/a');
        st.cells[r.id] = left;
        tr.appendChild(left);
        var act = hl.el('td', 'act');
        if (r.kind === 'unavailable') {
          act.className = 'act t3 small';
          act.textContent = 'No runner on this node';
        } else {
          var box = hl.el('div', 'rowbtns');
          actionsOf(r, box);
          act.appendChild(box);
        }
        tr.appendChild(act);
        els.rows.appendChild(tr);
        if (st.open && st.open.key === r.id) {
          var dr = hl.el('tr');
          var cell = hl.el('td', 'detail');
          cell.setAttribute('colspan', String(COLUMNS));
          dr.appendChild(cell);
          els.rows.appendChild(dr);
          st.open.cell = cell;
        }
      });
      refocus(els.rows, fk);
    }

    function render(force) {
      els.sub.textContent = summary();
      els.stale.textContent = st.failed ? 'Could not refresh (' + st.failed + '). Showing the last data.' : '';
      els.stale.hidden = !st.failed || !st.loaded;
      var empty = st.loaded && st.rows.length === 0;
      els.empty.hidden = !empty;
      if (empty) { els.emptytext.textContent = 'A node offers browser sessions when it carries the sessions tag and runs the task runner.'; }
      els.tablewrap.hidden = st.rows.length === 0;
      renderTable(force);
      paintTimes(now());
    }

    // ----- control surface

    ctrl.update = function (d) {
      st.rows = sessionRows(obj(d).nodes);
      st.loaded = true;
      render();
    };
    ctrl.tick = function (at) { paintTimes(typeof at === 'number' ? at : now()); };
    ctrl.reload = function () {
      return hl.api('GET', '/sessions.json').then(function (j) {
        if (!Array.isArray(obj(j).nodes)) { throw new Error('unexpected answer'); }
        st.failed = '';
        ctrl.update(j);
      }).catch(function (err) {
        st.failed = label(err && err.message, 120) || 'request failed';
        render();
      });
    };
    ctrl.state = st;
    render();
    return ctrl;
  }

  function init() {
    var root = document.getElementById('sessions');
    if (!root) { return; }
    var $ = function (id) { return document.getElementById(id); };
    var ctrl = mount({
      sub: $('sub'), error: $('error'), notice: $('notice'), stale: $('stale'), empty: $('empty'), emptytext: $('emptytext'),
      tablewrap: $('tablewrap'), rows: $('rows')
    });
    var poller = hl.poll(ctrl.reload, POLL_MS);
    setInterval(function () { if (!document.hidden) { ctrl.tick(); } }, 1000);
    document.addEventListener('visibilitychange', function () { if (!document.hidden) { poller.now(); } });
  }

  hl.sessions = { timeLeft: timeLeft, safeUrl: safeUrl, sessionRows: sessionRows, mount: mount, init: init };
  init();
})();
