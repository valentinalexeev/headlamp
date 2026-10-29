(function () {
  'use strict';

  var POLL_MS = 5000;

  // ---- pure helpers (also exported for the node self-test) ----------------------------------

  // "m:ss" or "h:mm:ss" left until iso; "expired" once it has passed, '' when unknown.
  function countdown(iso, nowMs) {
    if (!iso) { return ''; }
    var t = Date.parse(iso);
    if (isNaN(t)) { return ''; }
    var s = Math.round((t - nowMs) / 1000);
    if (s <= 0) { return 'expired'; }
    var h = Math.floor(s / 3600);
    var m = Math.floor((s % 3600) / 60);
    var sec = s % 60;
    var two = function (n) { return (n < 10 ? '0' : '') + n; };
    return h > 0 ? h + ':' + two(m) + ':' + two(sec) : m + ':' + two(sec);
  }

  // The API answers {"error": ...}; helpers shared with other modules answer {"detail": ...}.
  function errorMessage(status, data) {
    if (data && typeof data === 'object') {
      if (typeof data.error === 'string' && data.error) { return data.error; }
      if (typeof data.detail === 'string' && data.detail) { return data.detail; }
    }
    return 'request failed (HTTP ' + status + ')';
  }

  // One of 'running', 'idle', 'unavailable' (the dot and the words next to it).
  function stateOf(n) {
    if (!n.available) { return 'unavailable'; }
    return n.session && n.session.state === 'running' ? 'running' : 'idle';
  }

  // Only a plain http link is ever put in an href.
  function safeUrl(u) {
    return typeof u === 'string' && /^http:\/\//.test(u) ? u : '';
  }

  if (typeof document === 'undefined') {
    if (typeof module !== 'undefined' && module.exports) {
      module.exports = { countdown: countdown, errorMessage: errorMessage, stateOf: stateOf, safeUrl: safeUrl };
    }
    return;
  }

  // ---- DOM helpers (textContent only: node names come from headscale and are never markup) --

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) { e.className = cls; }
    if (text !== undefined && text !== null) { e.textContent = text; }
    return e;
  }

  function clear(node) { while (node.firstChild) { node.removeChild(node.firstChild); } }

  var $ = function (id) { return document.getElementById(id); };
  var rows = Object.create(null); // node id -> row parts
  var counter = 0;
  var placeholder = null;
  var haveData = false;
  var inflight = false;
  var again = false;
  var timer = null;
  var ticker = null;

  var TTL_CHOICES = [[1800, '30 min'], [3600, '1 h'], [14400, '4 h']];

  function setStatus(text, isError) {
    var s = $('status');
    s.textContent = text || '';
    s.className = 'status' + (isError ? ' error' : '');
  }

  function setBanner(text) {
    var b = $('banner');
    b.textContent = text || '';
    b.hidden = !text;
  }

  // Resolves to {status, data}; a network failure resolves to {status: 0, data: null}.
  function call(method, path, body) {
    var opts = { method: method, headers: {}, credentials: 'same-origin' };
    if (method === 'POST') {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body || {});
    }
    return fetch(path, opts).then(function (r) {
      if (r.status === 204) { return { status: 204, data: null }; }
      return r.json().catch(function () { return null; }).then(function (d) {
        return { status: r.status, data: d };
      });
    }, function () { return { status: 0, data: null }; });
  }

  function bannerFor(res) {
    if (res.status === 0) { return 'Cannot reach the server: showing the last data.'; }
    return 'Could not refresh (' + errorMessage(res.status, res.data) + '): showing the last data.';
  }

  // ---- rows ----------------------------------------------------------------------------------

  function makeRow(id) {
    var root = el('section', 'task');
    var hid = 'n-' + (counter++);
    root.setAttribute('aria-labelledby', hid);
    var head = el('div', 'head');
    var dot = el('span', 'dot');
    dot.setAttribute('aria-hidden', 'true');
    var title = el('h2', null, id);
    title.id = hid;
    var state = el('span', 'badge');
    head.appendChild(dot); head.appendChild(title); head.appendChild(state);
    var controls = el('div', 'session-controls');
    var ttl = el('select');
    ttl.setAttribute('aria-label', 'Session length for ' + id);
    TTL_CHOICES.forEach(function (c) {
      var o = el('option', null, c[1]);
      o.value = String(c[0]);
      if (c[0] === 3600) { o.selected = true; }
      ttl.appendChild(o);
    });
    var start = el('button', 'primary', 'Start');
    start.type = 'button';
    var stop = el('button', null, 'Stop');
    stop.type = 'button';
    var open = el('a', 'button', 'Open');
    open.target = '_blank';
    open.rel = 'noopener noreferrer';
    open.hidden = true;
    var left = el('span', 'muted');
    controls.appendChild(ttl); controls.appendChild(start); controls.appendChild(stop);
    controls.appendChild(open); controls.appendChild(left);
    root.appendChild(head); root.appendChild(controls);
    var row = { id: id, root: root, dot: dot, state: state, ttl: ttl, start: start, stop: stop,
      open: open, left: left, busy: false, data: null };
    start.addEventListener('click', function () { act(row, 'start'); });
    stop.addEventListener('click', function () { act(row, 'stop'); });
    return row;
  }

  function paint(row) {
    var n = row.data;
    var st = stateOf(n);
    row.dot.className = 'dot ' + st;
    var running = st === 'running';
    row.state.className = 'badge ' + (running ? 'running' : st === 'idle' ? 'ok' : 'failing');
    var text = running ? 'running' : st === 'idle' ? 'no session' : 'unavailable';
    if (st === 'idle' && n.session && n.session.state === 'ended') {
      text = 'ended' + (n.session.reason ? ' (' + n.session.reason + ')' : '');
    }
    row.state.textContent = text;
    row.start.disabled = row.busy || st !== 'idle';
    row.stop.disabled = row.busy || !running;
    row.ttl.disabled = row.busy || st !== 'idle';
    var href = running ? safeUrl(n.url) : '';
    if (href) { row.open.href = href; } else { row.open.removeAttribute('href'); }
    row.open.hidden = !href;
    var cd = running ? countdown(n.session.expires_at, Date.now()) : '';
    row.left.textContent = cd ? (cd === 'expired' ? 'expired' : cd + ' left') : '';
    if (running && n.session.expires_at) { row.left.title = n.session.expires_at; }
  }

  function setPlaceholder(text) {
    var host = $('nodes');
    if (!text) {
      if (placeholder && placeholder.parentNode === host) { host.removeChild(placeholder); }
      placeholder = null;
      return;
    }
    if (!placeholder || placeholder.parentNode !== host) {
      clear(host);
      placeholder = el('p', 'muted');
      host.appendChild(placeholder);
    }
    placeholder.textContent = text;
  }

  function render(list) {
    var host = $('nodes');
    host.setAttribute('aria-busy', 'false');
    var seen = {};
    var prev = null;
    list.forEach(function (n) {
      if (!n || typeof n.id !== 'string') { return; }
      seen[n.id] = true;
      var row = rows[n.id];
      if (!row) { row = rows[n.id] = makeRow(n.id); }
      row.data = n;
      paint(row);
      if (placeholder) { setPlaceholder(''); }
      var want = prev ? prev.root.nextSibling : host.firstChild;
      if (want !== row.root) { host.insertBefore(row.root, want); }
      prev = row;
    });
    Object.keys(rows).forEach(function (id) {
      if (!seen[id]) {
        if (rows[id].root.parentNode) { rows[id].root.parentNode.removeChild(rows[id].root); }
        delete rows[id];
      }
    });
    setPlaceholder(list.length ? '' : 'No nodes offer browser sessions.');
    $('updated').textContent = 'Updated ' + new Date().toLocaleTimeString();
  }

  // ---- actions -------------------------------------------------------------------------------

  function act(row, what) {
    row.busy = true;
    paint(row);
    var body = what === 'start' ? { ttl_seconds: parseInt(row.ttl.value, 10) } : {};
    call('POST', '/sessions/' + encodeURIComponent(row.id) + '/' + what, body).then(function (res) {
      row.busy = false;
      if (res.status === 201 || res.status === 204) {
        setStatus((what === 'start' ? 'Started a session on ' : 'Stopped the session on ') + row.id + '.', false);
      } else if (res.status === 0) {
        setStatus('Cannot reach the server.', true);
      } else {
        setStatus((what === 'start' ? 'Start' : 'Stop') + ' failed on ' + row.id + ': ' + errorMessage(res.status, res.data), true);
      }
      if (rows[row.id] === row) { paint(row); }
      refresh();
    });
  }

  // ---- refresh loop --------------------------------------------------------------------------

  function refresh() {
    if (inflight) { again = true; return Promise.resolve(); }
    inflight = true;
    return call('GET', '/sessions.json').then(function (res) {
      if (res.status === 200 && res.data && Array.isArray(res.data.nodes)) {
        haveData = true;
        setBanner('');
        render(res.data.nodes);
        return;
      }
      setBanner(bannerFor(res));
      if (!haveData) {
        $('nodes').setAttribute('aria-busy', 'false');
        setPlaceholder('No data yet.');
      }
    }).catch(function () {
      setBanner('Could not refresh: showing the last data.');
    }).then(function () {
      inflight = false;
      if (again) { again = false; return refresh(); }
    });
  }

  function visible() { return document.visibilityState !== 'hidden'; }

  function tick() {
    Object.keys(rows).forEach(function (id) { if (rows[id].data) { paint(rows[id]); } });
  }

  function schedule() {
    clearTimeout(timer);
    timer = null;
    if (!visible()) { return; }
    timer = setTimeout(function () { refresh().then(schedule); }, POLL_MS);
  }

  placeholder = $('nodes').querySelector('p');
  ticker = setInterval(function () { if (visible()) { tick(); } }, 1000);
  document.addEventListener('visibilitychange', function () {
    if (visible()) { refresh().then(schedule); } else { clearTimeout(timer); timer = null; }
  });
  if (visible()) { refresh().then(schedule); }
}());
