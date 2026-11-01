// headlamp console: the Namespaces & keys page. Needs /_ui/hl.js first (the shell loads it). Everything that came
// from the network goes in through textContent only; markup is never built from strings.
//
// Secrets: a publisher key exists only in the "New key" panel, right after it was minted. It is never put in a
// URL, storage, an anchor or a table row; Done, its namespace going away and leaving the page remove it.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var POLL_MS = 15000;
  var COLUMNS = 6;
  var KEY_COLUMNS = 4;
  var DEFAULT_SUB = 'Changing a namespace edits the access policy and needs confirming.';
  var NAME_RE = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;

  function arr(v) { return Array.isArray(v) ? v : []; }
  function obj(v) { return v && typeof v === 'object' && !Array.isArray(v) ? v : {}; }
  function clear(node) { while (node.firstChild) { node.removeChild(node.firstChild); } }
  function label(v, n) { return hl.plain(v, n || 100); }
  function keyed(el, key) { el.setAttribute('data-fk', key); return el; }

  function fmtPorts(ps) {
    return arr(ps).map(function (p) { p = obj(p); return p.lo === p.hi ? String(p.lo) : p.lo + '-' + p.hi; }).join(', ');
  }

  // parsePorts: "9000-9010, 9100" as ranges. Throws an Error with a message fit for the form.
  function parsePorts(text) {
    var out = [];
    String(text || '').split(',').forEach(function (part) {
      part = part.trim();
      if (!part) { return; }
      var m = /^(\d{1,5})(?:\s*-\s*(\d{1,5}))?$/.exec(part);
      if (!m) { throw new Error('Ports must look like 9000-9010, 9100.'); }
      var lo = parseInt(m[1], 10), hi = parseInt(m[2] || m[1], 10);
      if (lo < 1 || hi > 65535 || lo > hi) { throw new Error('Ports must be between 1 and 65535, lowest first.'); }
      out.push({ lo: lo, hi: hi });
    });
    if (!out.length) { throw new Error('Give at least one port.'); }
    return out;
  }

  // namespaceState: the word, the level and the flags of a namespace row. A change that waits for the policy
  // console and a change that was reverted are both attention (amber); only an active namespace reads as ok.
  function namespaceState(n) {
    n = obj(n);
    var pendingPorts = arr(n.pending_ports).length > 0;
    if (n.state === 'active' && pendingPorts) { return { level: 'warn', word: 'Ports change waiting for policy confirm', waiting: true, failed: false }; }
    if (n.state === 'active') { return { level: 'ok', word: 'Active', waiting: false, failed: false }; }
    if (n.state === 'pending') { return { level: 'warn', word: 'Waiting for policy confirm', waiting: true, failed: false }; }
    if (n.state === 'failed') { return { level: 'warn', word: 'Reverted', waiting: false, failed: true }; }
    return { level: 'off', word: label(n.state, 20) || 'Unknown', waiting: false, failed: false };
  }

  // routeCount: public names in a namespace, from the ingress report and the manual routes, once each.
  function routeCount(state, ns) {
    state = obj(state);
    var seen = {};
    arr(obj(obj(state.ingress).report).routes).concat(arr(state.routes)).forEach(function (r) {
      r = obj(r);
      if (r.ns === ns && r.service) { seen[r.service] = true; }
    });
    return Object.keys(seen).length;
  }

  function fmtDate(iso) {
    var t = Date.parse(iso);
    if (isNaN(t) || new Date(t).getFullYear() < 2000) { return 'Never'; }
    var d = new Date(t);
    var sameYear = d.getFullYear() === new Date().getFullYear();
    return d.toLocaleDateString(undefined, sameYear ? { month: 'short', day: 'numeric' } : { month: 'short', day: 'numeric', year: 'numeric' });
  }

  function keyFlags(k) {
    return [k.reusable ? 'Reusable' : 'Single use', k.ephemeral ? 'ephemeral' : '', k.used ? 'used' : '', k.expired ? 'expired' : ''].filter(Boolean).join(', ');
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

  // mount wires the page's elements to the data. deps (all optional): copy(text) for the clipboard, later(fn, ms)
  // for the short "Copied" label.
  function mount(els, deps) {
    deps = deps || {};
    var copy = deps.copy || copyText;
    var later = deps.later || function (fn, ms) { setTimeout(fn, ms); };
    var st = {
      state: null, loaded: false, failed: false, busy: false, open: null, tableSig: null, sel: '', keys: [], keysLoaded: false,
      keysFailed: '', keyOpen: null, keySig: null, panel: null, minting: false, secrets: []
    };
    var ctrl = {};

    function setError(text) { els.error.textContent = text; els.error.hidden = !text; }
    // setNotice shows a line, with a link to the policy page when the change it describes waits there.
    function setNotice(text, policy) {
      clear(els.notice);
      els.notice.appendChild(hl.el('span', null, text || ''));
      if (policy) {
        els.notice.appendChild(document.createTextNode(' '));
        var a = hl.el('a', null, 'Open policy');
        a.setAttribute('href', '/policy');
        els.notice.appendChild(a);
      }
      els.notice.hidden = !text;
    }

    function namespaces() { return arr(obj(st.state).namespaces).map(obj); }
    function findNS(name) {
      var list = namespaces();
      for (var i = 0; i < list.length; i++) { if (list[i].name === name) { return list[i]; } }
      return null;
    }

    // ----- waiting note

    function renderWait() {
      var box = els.waitnote;
      clear(box);
      var waiting = namespaces().filter(function (n) { return namespaceState(n).waiting; });
      if (!waiting.length) { box.hidden = true; return; }
      box.appendChild(hl.status('warn', waiting.length === 1 ? '1 namespace change waits for policy confirm' : waiting.length + ' namespace changes wait for policy confirm'));
      var p = hl.el('p', 't2 small', 'Confirm the change on the policy page, or it reverts by itself. A namespace turns active once it is confirmed. ');
      var a = hl.el('a', null, 'Open policy');
      a.setAttribute('href', '/policy');
      p.appendChild(a);
      box.appendChild(p);
      box.hidden = false;
    }

    // ----- actions

    function closeOpen() { st.open = null; render(true); }

    function run(fn, failWord) {
      if (st.busy) { return; }
      st.busy = true;
      setError('');
      setNotice('');
      st.open = null;
      render(true);
      return fn().then(function (r) {
        st.busy = false;
        r = r || {};
        if (r.notice) { setNotice(r.notice, !!r.policy); }
        return ctrl.reload();
      }, function (err) {
        st.busy = false;
        setError(failWord + ': ' + label(err && err.message, 200));
        render(true);
      });
    }

    function nsUrl(name) { return '/funnel/namespaces/' + encodeURIComponent(name); }

    function doPorts(n, ports) {
      run(function () {
        return hl.api('PATCH', nsUrl(n.name), { ports: ports }).then(function () {
          return { notice: 'Ports change for ' + label(n.name, 63) + ' submitted. It applies once the policy change is confirmed.', policy: true };
        });
      }, 'Changing the ports of ' + label(n.name, 63) + ' failed');
    }
    function doDelete(n) {
      run(function () {
        return hl.api('DELETE', nsUrl(n.name)).then(function (j) {
          if (st.sel === n.name) { select(''); }
          var left = obj(j).unexpired_keys;
          var text = 'Deleted ' + label(n.name, 63) + '. Its access entries are removed through the policy console.';
          if (left) { text += ' ' + left + ' publisher key' + (left === 1 ? ' has' : 's have') + ' not expired yet: expire ' + (left === 1 ? 'it' : 'them') + ' under Keys.'; }
          return { notice: text, policy: true };
        });
      }, 'Deleting ' + label(n.name, 63) + ' failed');
    }
    function doExpire(k) {
      if (st.busy) { return; }
      st.busy = true;
      setError('');
      st.keyOpen = null;
      renderKeys(true);
      hl.api('DELETE', '/funnel/keys/' + encodeURIComponent(k.id)).then(function () {
        st.busy = false;
        setNotice('Expired key ' + label(k.id, 20) + '.');
        return loadKeys();
      }, function (err) {
        st.busy = false;
        setError('Expiring key ' + label(k.id, 20) + ' failed: ' + label(err && err.message, 200));
        renderKeys(true);
      });
    }

    // ----- namespaces table

    function checklistDetail(n, td) {
      var cl = obj(n.checklist);
      var box = hl.el('div', 'stack');
      box.appendChild(hl.el('h2', null, 'DNS and certificate for ' + label(n.name, 63)));
      box.appendChild(hl.el('p', 't2 small', 'Both are set up by hand, outside this console. They are not checked from here.'));
      var recs = arr(cl.records);
      if (recs.length) {
        var dl = hl.el('dl', 'kv');
        recs.forEach(function (r) {
          r = obj(r);
          dl.appendChild(hl.el('dt', 'mono', label(r.type, 12)));
          dl.appendChild(hl.el('dd', 'mono', label(r.name, 253) + '  ->  ' + label(r.value, 300)));
        });
        box.appendChild(dl);
      }
      arr(cl.commands).forEach(function (c) { box.appendChild(hl.el('pre', 'code', label(c, 400))); });
      arr(cl.notes).forEach(function (t) { box.appendChild(hl.el('p', 't2 small', label(t, 400))); });
      var row = hl.el('div', 'rowbtns start');
      row.appendChild(keyed(button('Close', null, 'quiet', closeOpen), 'close'));
      box.appendChild(row);
      box.addEventListener('keydown', function (ev) { if (ev && ev.key === 'Escape') { closeOpen(); } });
      td.appendChild(box);
      return box;
    }

    function portsDetail(n, td) {
      var name = label(n.name, 63);
      var form = hl.el('form', 'stack');
      form.setAttribute('novalidate', '');
      form.setAttribute('aria-label', 'Change ports of ' + name);
      form.appendChild(hl.el('h2', null, 'Change ports of ' + name));
      form.appendChild(hl.el('p', 't2 small', 'This edits the access policy. The new ports apply once you confirm the change on the policy page.'));
      var l = hl.el('label', 'field', 'Ports');
      var input = hl.el('input', 'input');
      input.setAttribute('type', 'text');
      input.setAttribute('autocomplete', 'off');
      input.value = fmtPorts(n.ports);
      keyed(input, 'ports');
      l.appendChild(input);
      form.appendChild(l);
      var err = hl.el('div', 'note crit');
      err.setAttribute('role', 'alert');
      err.hidden = true;
      form.appendChild(err);
      var row = hl.el('div', 'rowbtns start');
      var save = hl.el('button', 'btn primary', 'Submit change');
      save.setAttribute('type', 'submit');
      row.appendChild(save);
      row.appendChild(button('Cancel', null, 'quiet', closeOpen));
      form.appendChild(row);
      form.addEventListener('submit', function (ev) {
        if (ev && ev.preventDefault) { ev.preventDefault(); }
        var ports;
        try { ports = parsePorts(input.value); } catch (e) { err.textContent = e.message; err.hidden = false; return; }
        doPorts(n, ports);
      });
      form.addEventListener('keydown', function (ev) { if (ev && ev.key === 'Escape') { closeOpen(); } });
      td.appendChild(form);
      input.focus();
      return form;
    }

    function deleteDetail(n, td) {
      var name = label(n.name, 63);
      return hl.confirmPanel({
        title: 'Delete namespace ' + name + '?', object: name,
        consequence: 'Its routes stop being served and its access entries are removed through the policy console, which needs confirming. Publisher keys are not expired by this: expire them under Keys.',
        confirmLabel: 'Delete ' + name, cancelLabel: 'Keep', icon: 'trash', into: td,
        onConfirm: function () { doDelete(n); }, onCancel: closeOpen
      });
    }

    function openPanel(n, kind) {
      if (st.busy) { return; }
      if (st.open && st.open.name === n.name && st.open.kind === kind) { closeOpen(); return; }
      st.open = { name: n.name, kind: kind, el: null, cell: null };
      render(true);
      if (st.open && st.open.cell) {
        st.open.el = kind === 'dns' ? checklistDetail(n, st.open.cell) : (kind === 'ports' ? portsDetail(n, st.open.cell) : deleteDetail(n, st.open.cell));
      }
    }

    function renderTable(force) {
      var list = namespaces();
      if (st.open && !findNS(st.open.name)) { st.open = null; }
      var model = list.map(function (n) {
        var s = namespaceState(n);
        var ports = fmtPorts(n.ports);
        if (arr(n.pending_ports).length) { ports += ' -> ' + fmtPorts(n.pending_ports); }
        return [label(n.name, 63), s.level, s.word, ports, routeCount(st.state, n.name), n.state, s.failed];
      });
      var sig = JSON.stringify([model, st.sel, st.open && [st.open.name, st.open.kind], st.busy]);
      if (!force && st.open && st.open.el) { return; }
      if (!force && sig === st.tableSig) { return; }
      st.tableSig = sig;
      var fk = focusKey(els.rows);
      clear(els.rows);
      if (st.open) { st.open.cell = null; }
      list.forEach(function (n, i) {
        var m = model[i];
        var s = namespaceState(n);
        var tr = hl.el('tr', st.sel === n.name || (st.open && st.open.name === n.name) ? 'sel' : '');
        tr.appendChild(hl.el('td', 'name mono', m[0]));
        var state = hl.el('td');
        state.appendChild(hl.status(s.level, s.word));
        if (s.waiting) {
          var a = hl.el('a', 'small', 'Open policy');
          a.setAttribute('href', '/policy');
          state.appendChild(document.createTextNode(' '));
          state.appendChild(a);
        }
        if (s.failed) {
          state.appendChild(hl.el('div', 't3 small', 'Not confirmed in time. Delete it and add it again.'));
        }
        tr.appendChild(state);
        tr.appendChild(hl.el('td', 'mono t2', m[3]));
        tr.appendChild(hl.el('td', 't2', String(m[4])));
        var dns = hl.el('td', 't2');
        if (s.failed) { dns.className = 't3'; dns.textContent = 'Not checked'; }
        else { dns.appendChild(keyed(button('Show steps', null, 'sm quiet', function () { openPanel(n, 'dns'); }), 'dns:' + m[0])); }
        tr.appendChild(dns);
        var act = hl.el('td', 'act');
        var box = hl.el('div', 'rowbtns');
        var add = function (key, text, icon, fn) {
          var b = keyed(button(text, icon, 'sm quiet', fn), key + ':' + m[0]);
          b.disabled = st.busy;
          box.appendChild(b);
        };
        if (n.state === 'active') {
          add('keys', 'Keys', 'key', function () { select(n.name); });
          if (!s.waiting) { add('ports', 'Ports', 'edit', function () { openPanel(n, 'ports'); }); }
        }
        add('delete', 'Delete', 'trash', function () { openPanel(n, 'delete'); });
        act.appendChild(box);
        tr.appendChild(act);
        els.rows.appendChild(tr);
        if (st.open && st.open.name === n.name) {
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

    // ----- publisher keys

    function select(name) {
      if (name === st.sel) { return; }
      closeMintPanel();
      st.sel = name;
      st.keys = [];
      st.keysLoaded = false;
      st.keysFailed = '';
      st.keyOpen = null;
      hl.setQueryParam('ns', name || null);
      render(true);
      if (name) { loadKeys(); }
    }

    function loadKeys() {
      var name = st.sel;
      if (!name) { return Promise.resolve(); }
      return hl.api('GET', nsUrl(name) + '/keys').then(function (j) {
        if (st.sel !== name) { return; }
        st.keys = arr(obj(j).keys).map(obj);
        st.keysLoaded = true;
        st.keysFailed = '';
        renderKeys();
      }, function (err) {
        if (st.sel !== name) { return; }
        st.keysFailed = label(err && err.message, 200) || 'The keys could not be loaded.';
        renderKeys(true);
      });
    }

    function renderKeys(force) {
      var n = st.sel ? findNS(st.sel) : null;
      if (!n || n.state !== 'active') { els.keys.hidden = true; return; }
      els.keys.hidden = false;
      els.keystitle.textContent = 'Publisher keys for ' + label(n.name, 63);
      var failed = st.keysFailed;
      els.keyerror.textContent = failed;
      els.keyerror.hidden = !failed;
      var sigAdd = 'x';
      if (st.keyAddSig !== sigAdd) {
        st.keyAddSig = sigAdd;
        clear(els.keyaddslot);
        els.keyaddslot.appendChild(keyed(button('Mint key', 'key', 'sm', function () { openMintPanel(); }), 'mint'));
      }
      var model = st.keys.map(function (k) { return [k.id, k.expiration, k.expired, k.reusable, k.ephemeral, k.used]; });
      var sig = JSON.stringify([st.sel, model, st.keyOpen, st.keysLoaded, st.busy]);
      if (!force && st.keyOpen !== null && st.keyEl) { return; }
      if (!force && sig === st.keySig) { return; }
      st.keySig = sig;
      var fk = focusKey(els.keyrows);
      clear(els.keyrows);
      st.keyCell = null;
      if (st.keys.length === 0) {
        var tr0 = hl.el('tr');
        var td0 = hl.el('td', 't2', st.keysLoaded ? 'No keys yet. Mint one for a node that should publish into this namespace.' : 'Loading the keys.');
        td0.setAttribute('colspan', String(KEY_COLUMNS));
        tr0.appendChild(td0);
        els.keyrows.appendChild(tr0);
      }
      st.keys.forEach(function (k) {
        var tr = hl.el('tr', st.keyOpen === k.id ? 'sel' : '');
        tr.appendChild(hl.el('td', 'mono', label(k.id, 20)));
        tr.appendChild(hl.el('td', 'mono ' + (k.expired ? 't3' : 't2'), fmtDate(k.expiration)));
        tr.appendChild(hl.el('td', 't2', keyFlags(k)));
        var act = hl.el('td', 'act');
        if (!k.expired) {
          var b = keyed(button('Expire', 'ban', 'sm quiet', function () { openExpire(k); }), 'expire:' + k.id);
          b.disabled = st.busy;
          act.appendChild(b);
        }
        tr.appendChild(act);
        els.keyrows.appendChild(tr);
        if (st.keyOpen === k.id) {
          var dr = hl.el('tr');
          var cell = hl.el('td', 'detail');
          cell.setAttribute('colspan', String(KEY_COLUMNS));
          dr.appendChild(cell);
          els.keyrows.appendChild(dr);
          st.keyCell = cell;
        }
      });
      refocus(els.keyrows, fk);
    }

    function closeExpire() { st.keyOpen = null; st.keyEl = null; renderKeys(true); }
    function openExpire(k) {
      if (st.busy) { return; }
      st.keyOpen = k.id;
      st.keyEl = null;
      renderKeys(true);
      if (st.keyCell) {
        st.keyEl = hl.confirmPanel({
          title: 'Expire key ' + label(k.id, 20) + '?', object: 'Key ' + label(k.id, 20) + ' for ' + label(st.sel, 63),
          consequence: 'Nodes can no longer register with it. Nodes that already joined keep working.',
          confirmLabel: 'Expire key ' + label(k.id, 20), cancelLabel: 'Keep', icon: 'ban', into: st.keyCell,
          onConfirm: function () { st.keyEl = null; doExpire(k); }, onCancel: closeExpire
        });
      }
    }

    // ----- minting

    function closeMintPanel() {
      if (!st.mint) { return; }
      var back = st.mint.opener;
      st.mint.root.remove();
      st.mint = null;
      if (back && back.focus) { back.focus(); }
    }

    function openMintPanel() {
      if (st.mint || !st.sel) { return; }
      var name = st.sel;
      var p = { opener: typeof document !== 'undefined' ? document.activeElement : null, saving: false };
      p.root = hl.el('form', 'panel');
      p.root.setAttribute('novalidate', '');
      p.root.setAttribute('aria-label', 'Mint a publisher key for ' + name);
      p.root.appendChild(hl.el('h2', null, 'Mint a publisher key for ' + label(name, 63)));
      p.root.appendChild(hl.el('p', 't2 small', 'A node joins the tailnet with this key and can then publish services into the namespace. The key is shown once.'));
      var row = hl.el('div', 'row');
      var l = hl.el('label', 'field grow', 'Valid for (hours, blank for the default)');
      var ttl = hl.el('input', 'input');
      ttl.setAttribute('type', 'text');
      ttl.setAttribute('inputmode', 'numeric');
      ttl.setAttribute('autocomplete', 'off');
      keyed(ttl, 'ttl');
      l.appendChild(ttl);
      row.appendChild(l);
      p.root.appendChild(row);
      function check(text, on) {
        var lab = hl.el('label', 'row');
        var c = hl.el('input');
        c.setAttribute('type', 'checkbox');
        c.checked = on;
        lab.appendChild(c);
        lab.appendChild(hl.el('span', null, text));
        p.root.appendChild(lab);
        return c;
      }
      var reusable = check('Reusable', true);
      var ephemeral = check('Ephemeral: the node is removed when it goes offline', false);
      var err = hl.el('div', 'note crit');
      err.setAttribute('role', 'alert');
      err.hidden = true;
      p.root.appendChild(err);
      var btns = hl.el('div', 'rowbtns start');
      var submit = hl.el('button', 'btn primary', 'Mint key');
      submit.setAttribute('type', 'submit');
      btns.appendChild(submit);
      btns.appendChild(button('Cancel', null, 'quiet', closeMintPanel));
      p.root.appendChild(btns);
      p.root.addEventListener('keydown', function (ev) { if (ev && ev.key === 'Escape') { closeMintPanel(); } });
      p.root.addEventListener('submit', function (ev) {
        if (ev && ev.preventDefault) { ev.preventDefault(); }
        if (p.saving) { return; }
        var body = { ephemeral: !!ephemeral.checked, reusable: !!reusable.checked };
        var text = String(ttl.value || '').trim();
        if (text) {
          if (!/^\d+$/.test(text) || parseInt(text, 10) < 1 || parseInt(text, 10) > 8760) {
            err.textContent = 'Hours must be a number from 1 to 8760.';
            err.hidden = false;
            return;
          }
          body.ttl_hours = parseInt(text, 10);
        }
        err.hidden = true;
        p.saving = true;
        submit.disabled = true;
        hl.api('POST', nsUrl(name) + '/keys', body).then(function (j) {
          p.saving = false;
          var key = typeof obj(j).key === 'string' ? j.key : '';
          closeMintPanel();
          // Shown even when the selection moved on meanwhile: the panel names its namespace, and the key is not lost.
          if (key) { showSecret(key, name); }
          return loadKeys();
        }, function (e) {
          p.saving = false;
          submit.disabled = false;
          err.textContent = label(e && e.message, 200);
          err.hidden = false;
        });
      });
      els.keyform.appendChild(p.root);
      st.mint = p;
      ttl.focus();
    }

    // showSecret adds the key to the side panel, the only place the text lives. A key is shown once, so another one
    // arriving or a change of selection leaves it there, and its panel names its namespace. Its own Done, its namespace
    // going away or leaving the page removes it.
    function showSecret(key, ns) {
      var root = hl.el('div', 'panel');
      root.setAttribute('role', 'group');
      root.setAttribute('aria-label', 'New key');
      root.appendChild(hl.el('h2', null, 'New key: copy it now'));
      root.appendChild(hl.el('p', 't2 small', 'This key for ' + label(ns, 63) + ' is shown once and is never stored, logged or audited. Done removes it from this page.'));
      var box = hl.el('div', 'secret', key);
      box.setAttribute('aria-label', 'Publisher key');
      root.appendChild(box);
      var note = hl.el('p', 't2 small');
      note.hidden = true;
      root.appendChild(note);
      var row = hl.el('div', 'rowbtns start');
      var cp = button('Copy key', 'copy', 'primary', function () {
        copy(box.textContent).then(function (ok) {
          if (ok) {
            var span = cp.children[cp.children.length - 1];
            span.textContent = 'Copied';
            later(function () { span.textContent = 'Copy key'; }, 2000);
            return;
          }
          selectAll(box);
          note.textContent = 'Copying is not available here. The key is selected: copy it by hand.';
          note.hidden = false;
        });
      });
      row.appendChild(cp);
      var secret = { root: root, box: box, ns: ns };
      row.appendChild(button('Done', null, 'quiet', function () { clearSecret(secret); }));
      root.appendChild(row);
      els.side.appendChild(root);
      st.secrets.push(secret);
      cp.focus();
    }

    function clearSecret(secret) {
      secret.box.textContent = '';
      secret.root.remove();
      st.secrets = st.secrets.filter(function (s) { return s !== secret; });
    }

    function clearSecrets() { st.secrets.slice().forEach(clearSecret); }

    // ----- adding a namespace

    function closeAddPanel() {
      if (!st.panel) { return; }
      var back = st.panel.opener;
      st.panel.root.remove();
      st.panel = null;
      if (back && back.focus) { back.focus(); }
    }

    function openAddPanel() {
      if (st.panel) { return; }
      var p = { opener: typeof document !== 'undefined' ? document.activeElement : null, saving: false };
      p.root = hl.el('form', 'panel');
      p.root.setAttribute('novalidate', '');
      p.root.setAttribute('aria-label', 'Add namespace');
      p.root.appendChild(hl.el('h2', null, 'Add a namespace'));
      p.root.appendChild(hl.el('p', 't2 small', 'This edits the access policy. The namespace turns active once you confirm the change on the policy page; if you do not, it reverts by itself.'));
      function field(text, input) { var l = hl.el('label', 'field grow', text); l.appendChild(input); return l; }
      function text(max) { var i = hl.el('input', 'input'); i.setAttribute('type', 'text'); i.setAttribute('autocomplete', 'off'); i.setAttribute('maxlength', String(max)); return i; }
      var name = text(63);
      keyed(name, 'name');
      var ports = text(200);
      ports.setAttribute('placeholder', '9000-9010, 9100');
      var note = text(200);
      var row = hl.el('div', 'row');
      row.appendChild(field('Name (lowercase letters, digits, hyphens)', name));
      row.appendChild(field('Ports', ports));
      p.root.appendChild(row);
      p.root.appendChild(field('Note (optional)', note));
      var err = hl.el('div', 'note crit');
      err.setAttribute('role', 'alert');
      err.hidden = true;
      p.root.appendChild(err);
      var btns = hl.el('div', 'rowbtns start');
      var submit = hl.el('button', 'btn primary', 'Submit change');
      submit.setAttribute('type', 'submit');
      btns.appendChild(submit);
      btns.appendChild(button('Cancel', null, 'quiet', closeAddPanel));
      p.root.appendChild(btns);
      p.root.addEventListener('keydown', function (ev) { if (ev && ev.key === 'Escape') { closeAddPanel(); } });
      p.root.addEventListener('submit', function (ev) {
        if (ev && ev.preventDefault) { ev.preventDefault(); }
        if (p.saving) { return; }
        var nm = String(name.value || '').trim();
        var rs;
        try {
          if (!NAME_RE.test(nm) || nm.length > 63) { throw new Error('Name must be lowercase letters, digits and hyphens, not starting or ending with a hyphen.'); }
          rs = parsePorts(ports.value);
        } catch (e) { err.textContent = e.message; err.hidden = false; return; }
        err.hidden = true;
        p.saving = true;
        submit.disabled = true;
        hl.api('POST', '/funnel/namespaces', { name: nm, ports: rs, note: String(note.value || '').trim() }).then(function () {
          p.saving = false;
          closeAddPanel();
          setNotice('Namespace ' + nm + ' submitted. It turns active once the policy change is confirmed.', true);
          return ctrl.reload();
        }, function (e) {
          p.saving = false;
          submit.disabled = false;
          err.textContent = label(e && e.message, 200);
          err.hidden = false;
        });
      });
      els.panel.appendChild(p.root);
      st.panel = p;
      name.focus();
    }

    // ----- render

    function renderAdd() {
      if (st.addDone) { return; }
      st.addDone = true;
      els.addslot.appendChild(keyed(button('Add namespace', 'plus', '', function () { openAddPanel(); }), 'add'));
    }

    function render(force) {
      var list = namespaces();
      var shown = st.loaded && list.length > 0;
      els.empty.hidden = !(st.loaded && list.length === 0);
      els.cols.hidden = !shown;
      els.sub.textContent = st.failed ? (st.loaded ? DEFAULT_SUB + ' The data may be out of date.' : 'Could not load the namespaces. Retrying.') : DEFAULT_SUB;
      // A key whose namespace is no longer active is removed with it.
      st.secrets.slice().forEach(function (secret) {
        var sn = findNS(secret.ns);
        if (!sn || sn.state !== 'active') { clearSecret(secret); }
      });
      renderAdd();
      renderWait();
      renderTable(force);
      renderKeys(force);
    }

    ctrl.update = function (d) {
      st.state = obj(d);
      st.loaded = true;
      if (st.sel && !findNS(st.sel)) { select(''); }
      render();
    };
    ctrl.setFailed = function (f) { st.failed = !!f; render(); };
    ctrl.reload = function () {
      return hl.api('GET', '/funnel/state').then(function (j) {
        ctrl.update(j);
        ctrl.setFailed(false);
        return st.sel ? loadKeys() : undefined;
      }, function () { ctrl.setFailed(true); });
    };
    ctrl.select = select;
    ctrl.clearSecrets = clearSecrets;
    ctrl.openAddPanel = openAddPanel;
    ctrl.state = st;

    // Leaving the page removes the key from it.
    if (typeof window.addEventListener === 'function') { window.addEventListener('pagehide', clearSecrets); }
    var wanted = hl.queryParam('ns');
    if (wanted && NAME_RE.test(wanted)) { st.sel = wanted; }
    render();
    return ctrl;
  }

  function init() {
    var root = document.getElementById('funnelconfig');
    if (!root) { return; }
    var $ = function (id) { return document.getElementById(id); };
    var ctrl = mount({
      sub: $('sub'), error: $('error'), notice: $('notice'), waitnote: $('waitnote'), addslot: $('addslot'), panel: $('panel'),
      empty: $('empty'), cols: $('cols'), rows: $('rows'), keys: $('keys'), keystitle: $('keystitle'), keyaddslot: $('keyaddslot'),
      keyform: $('keyform'), keyerror: $('keyerror'), keyrows: $('keyrows'), side: $('side')
    });
    var poller = hl.poll(ctrl.reload, POLL_MS);
    document.addEventListener('visibilitychange', function () { if (!document.hidden) { poller.now(); } });
  }

  hl.funnelConfig = { namespaceState: namespaceState, routeCount: routeCount, parsePorts: parsePorts, mount: mount, init: init };
  init();
})();
