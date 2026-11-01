// headlamp console: the Devices page and its enrolment panel. Needs /_ui/hl.js first (the shell loads it) and,
// for the QR code, /enroll/qrcode.js. Everything that came from the network goes in through textContent only;
// markup is never built from strings.
//
// Secrets: the one-time link and the subscription URL exist only in the enrolment panel, right after they are
// made. They are never put in a URL, an anchor, storage or a table row, and a rebuilt table cannot show them. The
// panel drops them when it closes and when it moves on to the next device.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var POLL_MS = 15000;
  var WAIT_MS = 4000;
  var DAY = 86400000;
  var EXPIRING_MS = 7 * DAY;
  var SEEN_MS = DAY;
  var DEFAULT_DAYS = 90;
  var LINK_TTL = 86400;
  var CLIENTS = ['karing', 'sing-box', 'clash'];
  var CLIENT_KEY = 'enrollClient';
  var FILTERS = [['active', 'Active'], ['expiring', 'Expiring soon'], ['never', 'Never used'], ['revoked', 'Revoked'], ['all', 'All']];
  var COLUMNS = 8;
  var SYNC_REASON = {
    unreachable: 'Agent unreachable', unauthorized: 'Agent refused the credentials',
    rejected: 'Agent rejected the credentials', conflict: 'Agent reports a conflict', error: 'Sync ended in an error'
  };
  var ICON = { link: 'link', extend: 'extend', revoke: 'ban', delete: 'trash' };

  function pad(n) { return n < 10 ? '0' + n : String(n); }
  function ymd(d) { return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()); }

  // endOfDay: the last second of a YYYY-MM-DD date in the browser's time zone, as unix seconds. NaN for junk.
  function endOfDay(text) {
    var m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(String(text || ''));
    if (!m) { return NaN; }
    var d = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]), 23, 59, 59, 0);
    return isNaN(d.getTime()) ? NaN : Math.floor(d.getTime() / 1000);
  }

  function fmtDate(sec) {
    var d = new Date(sec * 1000);
    if (isNaN(d.getTime())) { return ''; }
    var sameYear = d.getFullYear() === new Date().getFullYear();
    return d.toLocaleDateString(undefined, sameYear ? { month: 'short', day: 'numeric' } : { month: 'short', day: 'numeric', year: 'numeric' });
  }

  // stateOf: where a device stands now. level and word are what the row shows; kind, expiring and neverUsed are
  // what the filters and the actions go by. A device that is both near its expiry and unused reads as expiring.
  function stateOf(d, nowMs) {
    var used = d.enrol_count > 0;
    var exp = d.expires_at * 1000;
    if (d.revoked) { return { kind: 'revoked', level: 'off', word: 'Revoked', expiring: false, neverUsed: false }; }
    if (exp < nowMs) { return { kind: 'expired', level: 'warn', word: 'Expired', expiring: false, neverUsed: false }; }
    var left = exp - nowMs;
    var expiring = left <= EXPIRING_MS;
    var out = { kind: 'active', level: 'ok', word: 'Active', expiring: expiring, neverUsed: !used };
    if (expiring) {
      var days = Math.ceil(left / DAY);
      out.level = 'warn';
      out.word = left < DAY ? 'Expires today' : (days === 1 ? 'Expires in 1 day' : 'Expires in ' + days + ' days');
    } else if (!used) {
      out.level = 'warn';
      out.word = 'Link not used yet';
    }
    return out;
  }

  // actionsFor: only what can do something for the device's state.
  //   revoked:                  Delete (nothing else works on it)
  //   expired:                  Extend, Revoke (a link would not work: the spend refuses an expired device)
  //   active, never enrolled:   New link, Extend, Revoke
  //   active, already enrolled: Extend, Revoke (it has its profile; a link is not needed)
  function actionsFor(d, nowMs) {
    var s = stateOf(d, nowMs);
    var a = function (key, label, danger) { return { key: key, label: label, icon: ICON[key], danger: !!danger }; };
    if (s.kind === 'revoked') { return [a('delete', 'Delete', true)]; }
    var out = [];
    if (s.kind === 'active' && !(d.enrol_count > 0)) { out.push(a('link', 'New link')); }
    out.push(a('extend', 'Extend'));
    out.push(a('revoke', 'Revoke', true));
    return out;
  }

  function matches(filter, s) {
    switch (filter) {
      case 'expiring': return s.expiring;
      case 'never': return s.neverUsed;
      case 'revoked': return s.kind === 'revoked';
      case 'all': return true;
      default: return s.kind !== 'revoked';
    }
  }

  function plainFilter(f) {
    for (var i = 0; i < FILTERS.length; i++) { if (FILTERS[i][0] === f) { return f; } }
    return 'active';
  }

  // lastClient: the client used for the last device made in this browser. A storage that is missing or throws
  // (private windows, blocked site data) gives the first client; a stored value that is not a client is ignored.
  function lastClient() {
    try {
      var v = window.localStorage.getItem(CLIENT_KEY);
      if (CLIENTS.indexOf(v) >= 0) { return v; }
    } catch (e) { /* no storage: use the default */ }
    return CLIENTS[0];
  }
  function rememberClient(c) {
    try { window.localStorage.setItem(CLIENT_KEY, c); } catch (e) { /* nothing to remember with */ }
  }

  // defaultExpiry: the date offered for a new device, 90 days out, as YYYY-MM-DD.
  function defaultExpiry(nowMs) {
    var d = new Date(nowMs);
    d.setDate(d.getDate() + DEFAULT_DAYS);
    return ymd(d);
  }

  // extendTo: the new expiry (unix seconds) for an extension by days: counted from the later of the current
  // expiry and now, to the end of that day.
  function extendTo(d, days, nowMs) {
    var base = Math.max(d.expires_at * 1000, nowMs);
    var t = new Date(base);
    t.setDate(t.getDate() + days);
    t.setHours(23, 59, 59, 0);
    return Math.floor(t.getTime() / 1000);
  }

  function keyed(el, key) { el.setAttribute('data-fk', key); return el; }

  // focusKey names the focused control inside box by its data-fk, so a rebuild can give focus back.

  // qrOf draws the link as an SVG inside a white box with a quiet zone, in the browser: the link never goes to
  // another server. Without the library, or for a link too long to encode, the code is just absent.
  function qrOf(text) {
    try {
      if (typeof qrcode !== 'function') { return null; }
      var q = qrcode(0, 'M');
      q.addData(text);
      q.make();
      var n = q.getModuleCount(), m = 4, NS = 'http://www.w3.org/2000/svg';
      var svg = document.createElementNS(NS, 'svg');
      svg.setAttribute('viewBox', '0 0 ' + (n + 2 * m) + ' ' + (n + 2 * m));
      svg.setAttribute('width', '100%');
      svg.setAttribute('height', '100%');
      svg.setAttribute('role', 'img');
      svg.setAttribute('aria-label', 'QR code of the enrolment link');
      svg.setAttribute('shape-rendering', 'crispEdges');
      var bg = document.createElementNS(NS, 'rect');
      bg.setAttribute('width', String(n + 2 * m));
      bg.setAttribute('height', String(n + 2 * m));
      bg.setAttribute('fill', '#fff');
      var path = document.createElementNS(NS, 'path');
      var d = '';
      for (var r = 0; r < n; r++) {
        for (var c = 0; c < n; c++) { if (q.isDark(r, c)) { d += 'M' + (c + m) + ',' + (r + m) + 'h1v1h-1z'; } }
      }
      path.setAttribute('d', d);
      path.setAttribute('fill', '#000');
      svg.appendChild(bg);
      svg.appendChild(path);
      var box = hl.el('div', 'qr');
      box.appendChild(svg);
      return box;
    } catch (e) {
      return null;
    }
  }

  // copyText puts text on the clipboard and says whether it did; selecting it by hand is the fallback.
  function copyText(text) {
    try {
      if (typeof navigator !== 'undefined' && navigator.clipboard && navigator.clipboard.writeText) {
        return navigator.clipboard.writeText(text).then(function () { return true; }, function () { return false; });
      }
    } catch (e) { /* fall through */ }
    return Promise.resolve(false);
  }
  function selectAll(el) {
    try {
      var sel = window.getSelection && window.getSelection();
      if (sel && sel.selectAllChildren) { sel.selectAllChildren(el); }
    } catch (e) { /* the text stays selectable by hand */ }
  }

  // mount wires the page's elements to the data. deps (all optional): now() for the clock, poll(fn, ms) for the
  // wait for first use, copy(text) for the clipboard, later(fn, ms) for the short "Copied" label.
  function mount(els, deps) {
    deps = deps || {};
    var now = deps.now || function () { return Date.now(); };
    var poll = deps.poll || function (fn, ms) { return hl.poll(fn, ms); };
    var copy = deps.copy || copyText;
    var later = deps.later || function (fn, ms) { setTimeout(fn, ms); };
    var st = {
      devices: [], sync: null, ingresses: [], partial: false, loaded: false, failed: false,
      filter: plainFilter(hl.queryParam('filter')), busy: {}, open: null, tableSig: null, chipSig: null,
      chips: {}, drawer: null
    };
    var ctrl = { loadDevices: null };

    function setError(text) {
      els.error.textContent = text;
      els.error.hidden = !text;
    }
    function setNotice(text) {
      els.notice.textContent = text;
      els.notice.hidden = !text;
    }

    function find(id) {
      for (var i = 0; i < st.devices.length; i++) { if (st.devices[i].id === id) { return st.devices[i]; } }
      return null;
    }

    // ----- the sync note

    function names(list) {
      if (list.length < 2) { return list.join(''); }
      return list.slice(0, -1).join(', ') + ' and ' + list[list.length - 1];
    }

    function renderSync() {
      var box = els.syncnote;
      hl.clear(box);
      var s = st.sync;
      if (!s || !s.pending) { box.hidden = true; return; }
      var behind = hl.arr(st.ingresses).filter(function (r) { return r && r.sync && r.sync.pending && typeof r.name === 'string'; });
      var list = behind.map(function (r) { return hl.plain(r.name, 63); });
      var title, where;
      if (list.length === 0) {
        title = 'The bridge host is out of sync';
        where = 'the bridge host';
      } else {
        title = names(list) + (list.length === 1 ? ' is' : ' are') + ' out of sync';
        where = names(list);
      }
      box.appendChild(hl.status('crit', title));
      var why = [];
      (list.length ? behind : [{ name: '', sync: s }]).forEach(function (r) {
        var reason = SYNC_REASON[r.sync.state] || '';
        var since = typeof r.sync.since === 'number' ? hl.relTime(new Date(r.sync.since * 1000).toISOString(), now()) : '';
        var bits = [reason, since ? 'since ' + since : ''].filter(Boolean).join(', ');
        if (bits) { why.push(list.length > 1 ? hl.plain(r.name, 63) + ': ' + bits : bits); }
      });
      var p = hl.el('p', 't2 small', 'Revoking or extending a device here will not take effect on ' + where + ' until it catches up.' + (why.length ? ' ' + why.join('; ') + '.' : '') + ' Retrying every 5 minutes.');
      box.appendChild(p);
      var links = hl.el('p', 'small');
      (list.length ? list : ['']).forEach(function (n, i) {
        if (i > 0) { links.appendChild(document.createTextNode(' ')); }
        var a = hl.el('a', null, list.length > 1 ? 'Open ' + n : 'Open bridge');
        a.setAttribute('href', n ? '/nodes?sel=' + encodeURIComponent(n) : '/nodes');
        links.appendChild(a);
      });
      box.appendChild(links);
      box.hidden = false;
    }

    // ----- the table

    function rowModel(d) {
      var t = now();
      var s = stateOf(d, t);
      var total = d.traffic && typeof d.traffic === 'object' ? (Number(d.traffic.up) || 0) + (Number(d.traffic.down) || 0) : null;
      return {
        id: d.id, name: hl.plain(d.name, 100), owner: hl.plain(d.owner, 100), client: hl.plain(d.client, 20),
        level: s.level, word: s.word, kind: s.kind,
        seen: d.last_active > 0 ? hl.relTime(new Date(d.last_active * 1000).toISOString(), t) : 'Never',
        traffic: total === null ? '-' : (hl.fmtBytes(total) || '-'),
        trafficTitle: d.traffic ? 'Up ' + (hl.fmtBytes(Number(d.traffic.up) || 0) || '0 B') + ', down ' + (hl.fmtBytes(Number(d.traffic.down) || 0) || '0 B') : '',
        exp: s.kind === 'revoked' ? 'n/a' : fmtDate(d.expires_at),
        actions: actionsFor(d, t), expiresAt: d.expires_at
      };
    }

    function shownRows() {
      var t = now();
      var rank = { crit: 0, warn: 1, ok: 2, off: 3 };
      return st.devices.filter(function (d) { return matches(st.filter, stateOf(d, t)); }).map(rowModel).sort(function (a, b) {
        return (rank[a.level] - rank[b.level]) || (a.expiresAt - b.expiresAt) || (a.name < b.name ? -1 : a.name > b.name ? 1 : 0);
      });
    }

    function closeOpen() {
      st.open = null;
      render();
    }

    function runAction(d, key, fn) {
      if (st.busy[d.id]) { return; }
      st.busy[d.id] = true;
      setError('');
      setNotice('');
      st.open = null;
      render();
      fn().then(function (res) {
        delete st.busy[d.id];
        if (res && res.notice) { setNotice(res.notice); }
        if (res && res.sync !== undefined) { st.sync = res.sync; st.ingresses = hl.arr(res.ingresses); renderSync(); }
        render();
        if (ctrl.onAction) { ctrl.onAction(); }
      }, function (err) {
        delete st.busy[d.id];
        setError(key + ' failed for ' + hl.plain(d.name, 100) + ': ' + hl.plain(err && err.message, 200));
        render();
        if (ctrl.onAction) { ctrl.onAction(); }
      });
    }

    function doRevoke(d) {
      runAction(d, 'Revoke', function () {
        return hl.api('POST', '/enroll/devices/' + encodeURIComponent(d.id) + '/revoke', {}).then(function (j) {
          return { notice: 'Revoked ' + hl.plain(d.name, 100) + '.', sync: hl.obj(j).sync || null, ingresses: hl.obj(j).ingresses };
        });
      });
    }
    function doDelete(d) {
      runAction(d, 'Delete', function () {
        return hl.api('DELETE', '/enroll/devices/' + encodeURIComponent(d.id), {}).then(function () {
          return { notice: 'Deleted ' + hl.plain(d.name, 100) + '.' };
        });
      });
    }
    function doExtend(d, seconds) {
      runAction(d, 'Extend', function () {
        return hl.api('POST', '/enroll/devices/' + encodeURIComponent(d.id) + '/extend', { expires_at: seconds }).then(function (j) {
          return { notice: 'Extended ' + hl.plain(d.name, 100) + ' to ' + fmtDate(seconds) + '.', sync: hl.obj(j).sync || null, ingresses: hl.obj(j).ingresses };
        });
      });
    }
    function doLink(d) {
      if (st.busy[d.id]) { return; }
      st.busy[d.id] = true;
      setError('');
      render();
      hl.api('POST', '/enroll/devices/' + encodeURIComponent(d.id) + '/links', { ttl: LINK_TTL, max_uses: 1 }).then(function (j) {
        delete st.busy[d.id];
        render();
        openDrawer({ mode: 'link', device: { id: d.id, name: hl.plain(d.name, 100), owner: hl.plain(d.owner, 100), expires_at: d.expires_at }, link: String(hl.obj(j).url || '') });
      }, function (err) {
        delete st.busy[d.id];
        setError('New link failed for ' + hl.plain(d.name, 100) + ': ' + hl.plain(err && err.message, 200));
        render();
      });
    }

    function openConfirm(d, key, td) {
      var name = hl.plain(d.name, 100);
      var panel;
      if (key === 'revoke') {
        panel = hl.confirmPanel({
          title: 'Revoke ' + name + '?', object: name,
          consequence: 'It stops working on every bridge as soon as they sync. A revoked device cannot be switched back on: enrol a new one instead.',
          confirmLabel: 'Revoke ' + name, cancelLabel: 'Keep', icon: 'ban', into: td,
          onConfirm: function () { doRevoke(d); }, onCancel: closeOpen
        });
      } else {
        panel = hl.confirmPanel({
          title: 'Delete ' + name + '?', object: name,
          consequence: 'It is removed for good, with its history. This cannot be undone.',
          confirmLabel: 'Delete ' + name, cancelLabel: 'Keep', icon: 'trash', into: td,
          onConfirm: function () { doDelete(d); }, onCancel: closeOpen
        });
      }
      return panel;
    }

    // The extension panel: 30 or 90 days with the resulting date on the button, or a date of one's own.
    function openExtend(d, td) {
      var t = now();
      var name = hl.plain(d.name, 100);
      var panel = hl.el('div', 'note plain confirm');
      panel.setAttribute('role', 'group');
      panel.setAttribute('aria-label', 'Extend ' + name);
      panel.appendChild(hl.el('h2', null, 'Extend ' + name));
      panel.appendChild(hl.el('p', 't2', (d.expires_at * 1000 < t ? 'Expired ' : 'Expires ') + fmtDate(d.expires_at) + '.'));
      var row = hl.el('div', 'rowbtns');
      [30, 90].forEach(function (days) {
        var target = extendTo(d, days, t);
        var b = hl.button(days + ' days, to ' + fmtDate(target), 'extend', '', function () { doExtend(d, target); });
        keyed(b, 'extend:' + days);
        row.appendChild(b);
      });
      panel.appendChild(row);
      var field = hl.el('label', 'field', 'Or pick a date');
      var input = hl.el('input', 'input');
      input.setAttribute('type', 'date');
      input.setAttribute('aria-label', 'New expiry date');
      input.value = '';
      field.appendChild(input);
      panel.appendChild(field);
      var err = hl.el('p', 't2 small');
      err.hidden = true;
      panel.appendChild(err);
      var go = hl.button('Set date', 'extend', '', function () {
        var sec = endOfDay(input.value);
        if (isNaN(sec) || sec * 1000 <= now()) {
          err.textContent = 'Pick a date in the future.';
          err.hidden = false;
          return;
        }
        doExtend(d, sec);
      });
      var cancel = hl.button('Cancel', null, 'quiet', closeOpen);
      var row2 = hl.el('div', 'rowbtns');
      row2.appendChild(go);
      row2.appendChild(cancel);
      panel.appendChild(row2);
      panel.addEventListener('keydown', function (ev) { if (ev && ev.key === 'Escape') { closeOpen(); } });
      td.appendChild(panel);
      return panel;
    }

    function openPanel(d, key) {
      if (st.busy[d.id]) { return; }
      if (st.open && st.open.id === d.id && st.open.key === key) { return; }
      st.open = { id: d.id, key: key, el: null };
      render(true);
      var cell = st.open && st.open.cell;
      if (!cell) { return; }
      var panel = key === 'extend' ? openExtend(d, cell) : openConfirm(d, key, cell);
      st.open.el = panel;
      if (key === 'extend') {
        hl.refocus(panel, 'extend:30');
      }
    }

    function rowAction(d, a) {
      if (a.key === 'link') { doLink(d); } else { openPanel(d, a.key); }
    }

    // render rebuilds the table unless a confirmation or the extension panel is open: that is left as it is until
    // it is answered; closing it renders again. force starts a rebuild for a change the
    // operator made (opening a panel).
    function renderTable(force) {
      var rows = shownRows();
      if (st.open && !find(st.open.id)) { st.open = null; }
      var sig = JSON.stringify([rows, st.filter, st.open && [st.open.id, st.open.key], st.busy]);
      if (!force && st.open && st.open.el) {
        return;
      }
      if (!force && sig === st.tableSig) { return; }
      st.tableSig = sig;
      var fk = hl.focusKey(els.rows);
      hl.clear(els.rows);
      if (st.open) { st.open.cell = null; }
      if (rows.length === 0) {
        var tr0 = hl.el('tr');
        var td0 = hl.el('td', 't2', 'No devices match this filter.');
        td0.setAttribute('colspan', String(COLUMNS));
        tr0.appendChild(td0);
        els.rows.appendChild(tr0);
      }
      rows.forEach(function (r) {
        var d = find(r.id);
        var tr = hl.el('tr', st.open && st.open.id === r.id ? 'sel' : '');
        tr.appendChild(hl.el('td', 'name', r.name));
        tr.appendChild(hl.el('td', 't2', r.owner));
        tr.appendChild(hl.el('td', 'mono t2', r.client));
        var state = hl.el('td');
        state.appendChild(hl.status(r.level, r.word));
        tr.appendChild(state);
        tr.appendChild(hl.el('td', 't2', r.seen));
        var tc = hl.el('td', 'mono t2', r.traffic);
        if (r.trafficTitle) { tc.setAttribute('title', r.trafficTitle); }
        tr.appendChild(tc);
        tr.appendChild(hl.el('td', 'mono t2', r.exp));
        var actCell = hl.el('td', 'act');
        var box = hl.el('div', 'rowbtns');
        r.actions.forEach(function (a) {
          var b = keyed(hl.button(a.label, a.icon, 'sm quiet', function () { rowAction(d, a); }), 'act:' + r.id + ':' + a.key);
          b.disabled = !!st.busy[r.id];
          box.appendChild(b);
        });
        actCell.appendChild(box);
        tr.appendChild(actCell);
        els.rows.appendChild(tr);
        if (st.open && st.open.id === r.id) {
          var dr = hl.el('tr');
          var cell = hl.el('td', 'detail');
          cell.setAttribute('colspan', String(COLUMNS));
          dr.appendChild(cell);
          els.rows.appendChild(dr);
          st.open.cell = cell;
        }
      });
      hl.refocus(els.rows, fk);
    }

    function renderChips() {
      var sig = st.devices.length ? 'on' : 'off';
      els.chips.hidden = st.devices.length === 0;
      if (sig !== st.chipSig) {
        st.chipSig = sig;
        st.chips = {};
        hl.clear(els.chips);
        FILTERS.forEach(function (f) {
          var b = hl.el('button', 'chip', f[1]);
          b.setAttribute('type', 'button');
          b.addEventListener('click', function () { ctrl.setFilter(f[0]); });
          st.chips[f[0]] = b;
          els.chips.appendChild(b);
        });
      }
      Object.keys(st.chips).forEach(function (k) {
        st.chips[k].className = 'chip' + (k === st.filter ? ' on' : '');
        st.chips[k].setAttribute('aria-pressed', k === st.filter ? 'true' : 'false');
      });
    }

    function summary() {
      var t = now();
      var active = 0, seen = 0;
      st.devices.forEach(function (d) {
        if (stateOf(d, t).kind !== 'active') { return; }
        active++;
        if (d.last_active > 0 && d.last_active * 1000 >= t - SEEN_MS) { seen++; }
      });
      return active + ' active, ' + seen + ' seen today';
    }

    function render(force) {
      var text = st.loaded ? summary() : '';
      if (st.failed) { text = st.loaded ? text + '. The data may be out of date.' : 'Could not load the devices. Retrying.'; }
      els.sub.textContent = text;
      els.empty.hidden = !(st.loaded && st.devices.length === 0);
      els.tablewrap.hidden = st.devices.length === 0;
      els.foot.hidden = st.devices.length === 0;
      renderChips();
      renderTable(force);
    }

    // ----- the enrolment panel

    function closeDrawer() {
      var dr = st.drawer;
      // While the device is being made the panel stays: closing now would lose the link it is about to show.
      if (!dr || dr.creating) { return; }
      if (dr.poller) { dr.poller.stop(); dr.poller = null; }
      dr.root.remove();
      dr.link = '';
      dr.subscription = '';
      dr.linkBox = null;
      hl.clear(dr.body);
      st.drawer = null;
      hl.setQueryParam('enrol', null);
      var back = dr.opener;
      if (back && back.isConnected !== false && back.focus) { back.focus(); }
    }

    function drawerFocusables(dr) {
      var out = [];
      (function walk(n) {
        if (n.tag === 'button' || n.tag === 'input' || n.tag === 'select' || n.tag === 'summary') {
          if (!n.disabled && !n.hidden) { out.push(n); }
        }
        Array.prototype.slice.call(n.children || []).forEach(walk);
      })(dr.aside);
      return out;
    }

    // setWaiting fills the status under the link: waiting for the first use, or enrolled.
    function setWaiting(dr) {
      if (!dr.waitBox) { return; }
      hl.clear(dr.waitBox);
      if (dr.enrolled) {
        dr.waitBox.appendChild(hl.status('ok', 'Enrolled'));
        dr.waitBox.appendChild(hl.el('p', 't3 small', 'The device added its profile' + (dr.enrolledAt ? ' ' + hl.relTime(new Date(dr.enrolledAt * 1000).toISOString(), now()) : '') + '. The link is used up.'));
        if (dr.linkBox) { dr.linkBox.hidden = true; }
        if (dr.poller) { dr.poller.stop(); dr.poller = null; }
        return;
      }
      dr.waitBox.appendChild(hl.status('ok', 'Waiting for first use'));
      dr.waitBox.appendChild(hl.el('p', 't3 small', 'This updates by itself when the phone adds the profile. You can close this panel: the link stays valid.'));
    }

    // checkEnrolled looks for the panel's device in fresh data and notes its first use.
    function checkEnrolled() {
      var dr = st.drawer;
      if (!dr || dr.step !== 'link' || dr.enrolled || !dr.device) { return; }
      var d = find(dr.device.id);
      if (d && d.enrol_count > 0) {
        dr.enrolled = true;
        dr.enrolledAt = d.enrolled_at || 0;
        setWaiting(dr);
      }
    }

    function showCopied(btn, label) {
      var span = btn.children[btn.children.length - 1];
      var prev = span.textContent;
      span.textContent = label;
      later(function () { span.textContent = prev; }, 2000);
    }

    // copyButton: the clipboard if there is one, else the text is selected for a copy by hand.
    function copyButton(label, text, shown, note, primary) {
      var b = hl.button(label, 'copy', primary ? 'primary' : '', function () {
        copy(text).then(function (ok) {
          if (ok) { showCopied(b, 'Copied'); return; }
          selectAll(shown);
          note.textContent = 'Copying is not available here. The text is selected: copy it by hand.';
          note.hidden = false;
        });
      });
      return b;
    }

    function secretBox(text, label) {
      var s = hl.el('div', 'secret', text);
      s.setAttribute('aria-label', label);
      return s;
    }

    function buildLink(dr) {
      var body = dr.body;
      hl.clear(body);
      dr.step = 'link';
      var dev = dr.device;
      var head = dr.mode === 'create'
        ? hl.status('ok', dev.name + ' created for ' + dev.owner + ', expires ' + fmtDate(dev.expires_at))
        : hl.status('ok', 'One-time link for ' + dev.name);
      body.appendChild(head);
      dr.copyNote = hl.el('p', 't2 small');
      dr.copyNote.hidden = true;
      dr.linkBox = hl.el('div', 'stack');
      if (dr.linkError) {
        dr.linkBox.appendChild(hl.el('div', 'note crit', 'The one-time link could not be made: ' + dr.linkError + ' Use New link in the table, or set the device up by hand with the subscription URL below.'));
      } else {
        var top = hl.el('div', 'row');
        var q = qrOf(dr.link);
        if (q) { top.appendChild(q); }
        var words = hl.el('div', 'stack beside');
        words.appendChild(hl.el('p', null, 'Scan with the phone, or send the link to its owner.'));
        words.appendChild(hl.el('p', 't2 small', 'Works once, valid for 24 hours. Opening it here would use it up, so it is not clickable.'));
        top.appendChild(words);
        dr.linkBox.appendChild(top);
        var secret = secretBox(dr.link, 'Enrolment link');
        dr.linkBox.appendChild(secret);
        var btns = hl.el('div', 'rowbtns start');
        btns.appendChild(copyButton('Copy link', dr.link, secret, dr.copyNote, true));
        dr.linkBox.appendChild(btns);
      }
      body.appendChild(dr.linkBox);
      body.appendChild(dr.copyNote);
      if (dr.mode === 'create') {
        var again = hl.el('div', 'rowbtns start');
        again.appendChild(hl.button('Enrol another device', null, 'quiet', function () { dr.enrolled = false; buildDetails(dr); }));
        body.appendChild(again);
      }
      if (!dr.linkError) {
        body.appendChild(hl.el('hr', 'divider'));
        dr.waitBox = hl.el('div', 'stack');
        body.appendChild(dr.waitBox);
        setWaiting(dr);
      }
      if (dr.subscription) {
        var det = hl.el('details');
        if (dr.linkError) { det.setAttribute('open', ''); }
        det.appendChild(hl.el('summary', 't2 small', 'Manual setup: subscription URL'));
        var inner = hl.el('div', 'stack');
        inner.appendChild(hl.el('p', 't3 small', 'For when the link does not open the app. Paste it into the client by hand.'));
        var sub = secretBox(dr.subscription, 'Subscription URL');
        inner.appendChild(sub);
        var row = hl.el('div', 'rowbtns start');
        row.appendChild(copyButton('Copy subscription URL', dr.subscription, sub, dr.copyNote, false));
        inner.appendChild(row);
        det.appendChild(inner);
        body.appendChild(det);
      }
      if (!dr.enrolled && !dr.linkError && !dr.poller) {
        dr.poller = poll(function () { return ctrl.loadDevices ? ctrl.loadDevices() : undefined; }, WAIT_MS);
      }
      checkEnrolled();
    }

    function daysHint(value) {
      var sec = endOfDay(value);
      if (isNaN(sec)) { return 'Pick a date.'; }
      var days = Math.round((sec * 1000 - now()) / DAY);
      if (sec * 1000 <= now()) { return 'That date has passed.'; }
      return days === 1 ? '1 day from today' : days + ' days from today';
    }

    function buildDetails(dr) {
      var body = dr.body;
      hl.clear(body);
      dr.step = 'details';
      dr.mode = 'create';
      dr.waitBox = null;
      dr.linkBox = null;
      dr.link = '';
      dr.subscription = '';
      if (dr.poller) { dr.poller.stop(); dr.poller = null; }
      var form = hl.el('form', 'stack');
      form.setAttribute('novalidate', '');
      var fields = {};
      function field(label, key, input) {
        var l = hl.el('label', 'field', label);
        l.appendChild(input);
        var e = hl.el('div');
        e.hidden = true;
        l.appendChild(e);
        fields[key] = { input: input, err: e };
        form.appendChild(l);
        return input;
      }
      var name = hl.el('input', 'input');
      name.setAttribute('type', 'text');
      name.setAttribute('maxlength', '100');
      name.setAttribute('autocomplete', 'off');
      name.setAttribute('data-fk', 'name');
      field('Device name', 'name', name);
      var owner = hl.el('input', 'input');
      owner.setAttribute('type', 'text');
      owner.setAttribute('maxlength', '100');
      owner.setAttribute('autocomplete', 'off');
      field('Owner', 'owner', owner);
      var pair = hl.el('div', 'row');
      var client = hl.el('select', 'input');
      CLIENTS.forEach(function (c) {
        var o = hl.el('option', null, c);
        o.setAttribute('value', c);
        client.appendChild(o);
      });
      client.value = lastClient();
      var expires = hl.el('input', 'input');
      expires.setAttribute('type', 'date');
      expires.value = defaultExpiry(now());
      var cl = hl.el('label', 'field grow', 'Client');
      cl.appendChild(client);
      var el2 = hl.el('label', 'field grow', 'Expires');
      el2.appendChild(expires);
      var hint = hl.el('div', 't3 small', daysHint(expires.value));
      el2.appendChild(hint);
      var ee = hl.el('div');
      ee.hidden = true;
      el2.appendChild(ee);
      fields.expires = { input: expires, err: ee };
      pair.appendChild(cl);
      pair.appendChild(el2);
      form.appendChild(pair);
      var refreshHint = function () { hint.textContent = daysHint(expires.value); };
      expires.addEventListener('input', refreshHint);
      expires.addEventListener('change', refreshHint);
      dr.formError = hl.el('div', 'note crit');
      dr.formError.setAttribute('role', 'alert');
      dr.formError.hidden = true;
      form.appendChild(dr.formError);
      var submit = hl.el('button', 'btn primary', 'Create and get link');
      submit.setAttribute('type', 'submit');
      form.appendChild(submit);
      body.appendChild(form);
      dr.fields = fields;

      function invalid(key, text) {
        fields[key].err.textContent = '';
        fields[key].err.appendChild(hl.status('crit', text));
        fields[key].err.hidden = false;
        fields[key].input.setAttribute('aria-invalid', 'true');
      }
      function valid(key) {
        hl.clear(fields[key].err);
        fields[key].err.hidden = true;
        fields[key].input.removeAttribute('aria-invalid');
      }
      form.addEventListener('submit', function (ev) {
        if (ev && ev.preventDefault) { ev.preventDefault(); }
        if (dr.creating) { return; }
        var n = String(name.value || '').trim();
        var o = String(owner.value || '').trim();
        var sec = endOfDay(expires.value);
        var first = null;
        ['name', 'owner', 'expires'].forEach(valid);
        if (!n) { invalid('name', 'Name is required.'); first = first || name; }
        if (!o) { invalid('owner', 'Owner is required.'); first = first || owner; }
        if (isNaN(sec) || sec * 1000 <= now()) { invalid('expires', 'Pick an expiry date in the future.'); first = first || expires; }
        if (first) { first.focus(); return; }
        var c = CLIENTS.indexOf(client.value) >= 0 ? client.value : CLIENTS[0];
        rememberClient(c);
        dr.creating = true;
        submit.disabled = true;
        submit.textContent = 'Creating…';
        dr.formError.hidden = true;
        var made = null;
        hl.api('POST', '/enroll/devices', { name: n, owner: o, client: c, expires_at: sec }).then(function (j) {
          made = hl.obj(j);
          dr.device = { id: made.id, name: hl.plain(n, 100), owner: hl.plain(o, 100), expires_at: sec };
          dr.subscription = typeof made.subscription_url === 'string' ? made.subscription_url : '';
          dr.link = '';
          dr.linkError = '';
          dr.enrolled = false;
          if (made.sync !== undefined) { st.sync = made.sync || null; st.ingresses = hl.arr(made.ingresses); renderSync(); }
          if (ctrl.loadDevices) { ctrl.loadDevices(); }
          return hl.api('POST', '/enroll/devices/' + encodeURIComponent(made.id) + '/links', { ttl: LINK_TTL, max_uses: 1 }).then(function (l) {
            dr.link = String(hl.obj(l).url || '');
          }, function (e) {
            dr.linkError = hl.plain(e && e.message, 200);
          });
        }).then(function () {
          dr.creating = false;
          if (st.drawer === dr) {
            buildLink(dr);
            // The form (and the button that was focused) is gone: land on the first button of the result, the copy button.
            var first = dr.body.querySelector ? dr.body.querySelector('button') : null;
            if (first) { first.focus(); }
          }
        }).catch(function (e) {
          dr.creating = false;
          var msg = hl.plain(e && e.message, 200);
          if (made) {
            // The device exists: the form stays shut so a second one is not made by mistake.
            var text = 'The device was created, but this panel could not show its link' + (msg ? ': ' + msg : '') + '. Find it in the table and use New link.';
            if (dr.step === 'link') {
              dr.body.appendChild(hl.el('div', 'note crit', text));
            } else {
              dr.formError.textContent = text;
              dr.formError.hidden = false;
              submit.textContent = 'Created';
            }
            try { if (ctrl.loadDevices) { ctrl.loadDevices(); } } catch (e2) { /* the next poll reads it */ }
            return;
          } else {
            submit.disabled = false;
            submit.textContent = 'Create and get link';
            dr.formError.textContent = msg;
          }
          dr.formError.hidden = false;
        });
      });
      name.focus();
    }

    function openDrawer(opts) {
      opts = opts || {};
      if (st.drawer) { return st.drawer; }
      var dr = { step: '', mode: opts.mode || 'create', device: opts.device || null, link: opts.link || '', linkError: '', subscription: '',
        enrolled: false, enrolledAt: 0, poller: null, creating: false, opener: typeof document !== 'undefined' ? document.activeElement : null };
      dr.root = hl.el('div');
      var scrim = hl.el('div', 'scrim');
      scrim.addEventListener('click', closeDrawer);
      dr.aside = hl.el('aside', 'drawer');
      dr.aside.setAttribute('role', 'dialog');
      dr.aside.setAttribute('aria-modal', 'true');
      dr.aside.setAttribute('aria-label', 'Enrol device');
      var head = hl.el('div', 'panelhead');
      head.appendChild(hl.el('h2', null, 'Enrol device'));
      var close = hl.el('button', 'btn quiet sm');
      close.setAttribute('type', 'button');
      close.setAttribute('aria-label', 'Close');
      close.setAttribute('title', 'Close');
      var ic = hl.icon('close');
      if (ic) { close.appendChild(ic); }
      close.addEventListener('click', closeDrawer);
      head.appendChild(close);
      dr.aside.appendChild(head);
      dr.body = hl.el('div', 'stack');
      dr.aside.appendChild(dr.body);
      dr.aside.addEventListener('keydown', function (ev) {
        if (!ev) { return; }
        if (ev.key === 'Escape') { closeDrawer(); return; }
        if (ev.key === 'Tab') {
          var list = drawerFocusables(dr);
          if (!list.length) { return; }
          var ae = document.activeElement;
          var last = list[list.length - 1], first = list[0];
          if (!ev.shiftKey && ae === last) { if (ev.preventDefault) { ev.preventDefault(); } first.focus(); }
          else if (ev.shiftKey && ae === first) { if (ev.preventDefault) { ev.preventDefault(); } last.focus(); }
        }
      });
      dr.root.appendChild(scrim);
      dr.root.appendChild(dr.aside);
      els.drawer.appendChild(dr.root);
      st.drawer = dr;
      hl.setQueryParam('enrol', '1');
      if (dr.mode === 'link') { buildLink(dr); close.focus(); } else { buildDetails(dr); }
      return dr;
    }

    // ----- control surface

    ctrl.setFilter = function (f) {
      st.filter = plainFilter(f);
      hl.setQueryParam('filter', st.filter === 'active' ? null : st.filter);
      st.open = null;
      render(true);
    };
    ctrl.update = function (d) {
      d = hl.obj(d);
      st.loaded = true;
      st.devices = hl.arr(d.devices);
      st.sync = d.sync || null;
      st.ingresses = hl.arr(d.ingresses);
      st.partial = !!d.stats_partial;
      els.traffic.textContent = st.partial ? 'Traffic*' : 'Traffic';
      if (st.partial) { els.traffic.setAttribute('title', 'Partial: a bridge did not answer, so its traffic is missing.'); } else { els.traffic.removeAttribute('title'); }
      renderSync();
      checkEnrolled();
      render();
    };
    ctrl.setFailed = function (f) { st.failed = !!f; render(); };
    ctrl.openDrawer = openDrawer;
    ctrl.closeDrawer = closeDrawer;
    ctrl.state = st;
    ctrl.onAction = null;

    if (els.empty) {
      els.empty.appendChild(hl.el('div', 'rowbtns start')).appendChild(hl.button('Enrol device', 'plus', 'primary', function () { openDrawer(); }));
    }
    render();
    // The top-bar action links to /enroll?enrol=1: the panel is open on arrival.
    if (hl.queryParam('enrol') === '1') { openDrawer(); }
    return ctrl;
  }

  function init() {
    var root = document.getElementById('devices');
    if (!root) { return; }
    var $ = function (id) { return document.getElementById(id); };
    var ctrl = mount({
      sub: $('sub'), chips: $('chips'), error: $('error'), notice: $('notice'), syncnote: $('syncnote'), empty: $('empty'),
      tablewrap: $('tablewrap'), rows: $('rows'), foot: $('foot'), traffic: $('trafficth'), drawer: $('drawer')
    });

    function load() {
      return hl.api('GET', '/enroll/devices').then(function (j) {
        ctrl.update(j);
        ctrl.setFailed(false);
      }, function () { ctrl.setFailed(true); });
    }
    ctrl.loadDevices = load;
    var poller = hl.poll(load, POLL_MS);
    if (document.hidden) { load(); }
    document.addEventListener('visibilitychange', function () { if (!document.hidden) { poller.now(); } });
    ctrl.onAction = function () { poller.now(); };
  }

  hl.devices = {
    stateOf: stateOf, actionsFor: actionsFor, mount: mount, init: init, endOfDay: endOfDay, defaultExpiry: defaultExpiry,
    extendTo: extendTo, lastClient: lastClient
  };
  init();
})();
