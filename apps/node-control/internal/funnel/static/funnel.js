// headlamp console: the Funnel page (public routes). Needs /_ui/hl.js first (the shell loads it). Everything that
// came from the network, the ingress report included, goes in through textContent only; markup is never built
// from strings. Namespaces and publisher keys live on /funnel/config (config.js).
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var POLL_MS = 15000;
  var COLUMNS = 6;
  var FILTERS = [['all', 'All'], ['offline', 'Offline'], ['manual', 'Manual'], ['registered', 'Registered'], ['blocked', 'Blocked']];
  var RANK = { crit: 0, warn: 1, ok: 2, off: 3 };
  var SOURCE_WORD = { registered: 'Registered', manual: 'Manual', none: 'Blocked name' };

  function label(v, n) { return hl.plain(v, n || 100); }
  function keyed(el, key) { el.setAttribute('data-fk', key); return el; }

  // serviceRows merges the three sources of the page into one row per public name: the routes the ingress
  // reports (registered by a node, or manual ones it serves), the manual routes in the store (also those the
  // ingress has not pulled yet) and the blocks (also those that match no route). A block wins over everything.
  function serviceRows(state) {
    state = hl.obj(state);
    var ing = hl.obj(state.ingress);
    var rep = hl.obj(ing.report);
    var stale = !!ing.stale;
    var domain = label(state.public_domain, 200);
    var map = {};
    var order = [];
    function slot(ns, service) {
      var k = ns + '/' + service;
      if (!map[k]) {
        map[k] = { key: k, ns: ns, service: service, name: service + '.' + ns + (domain ? '.' + domain : ''), reported: false,
          manual: false, registered: false, blocked: false, reason: '', online: false, target: '', owner: '' };
        order.push(k);
      }
      return map[k];
    }
    hl.arr(rep.routes).forEach(function (r) {
      r = hl.obj(r);
      var ns = label(r.ns, 63), svc = label(r.service, 63);
      if (!ns || !svc) { return; }
      var row = slot(ns, svc);
      row.reported = true;
      row.online = !!r.target_online;
      row.target = label(r.target, 200) + (r.port ? ':' + label(r.port, 6) : '');
      if (r.owner_node_id) {
        row.registered = true;
        row.owner = label(r.owner_name || r.owner_node_id, 100);
      } else {
        row.manual = true;
      }
    });
    hl.arr(state.routes).forEach(function (r) {
      r = hl.obj(r);
      var ns = label(r.ns, 63), svc = label(r.service, 63);
      if (!ns || !svc) { return; }
      var row = slot(ns, svc);
      row.manual = true;
      row.registered = false;
      row.target = label(r.target, 200) + ':' + label(r.port, 6);
    });
    hl.arr(state.blocks).forEach(function (b) {
      b = hl.obj(b);
      var ns = label(b.ns, 63), svc = label(b.service, 63);
      if (!ns || !svc) { return; }
      var row = slot(ns, svc);
      row.blocked = true;
      row.reason = label(b.reason, 500);
    });
    var rows = order.map(function (k) {
      var r = map[k];
      var source = r.registered ? 'registered' : (r.manual ? 'manual' : 'none');
      var kind, level, word;
      if (r.blocked) { kind = 'blocked'; level = 'off'; word = 'Blocked'; }
      else if (!r.reported) { kind = 'unreported'; level = 'off'; word = 'Not reported yet'; }
      else if (r.online) { kind = 'online'; level = 'ok'; word = 'Online'; }
      else { kind = 'offline'; level = 'warn'; word = 'Offline'; }
      if (stale && r.reported && !r.blocked) { word += ' (stale)'; }
      return {
        key: r.key, ns: r.ns, service: r.service, name: r.name, source: source, sourceWord: SOURCE_WORD[source],
        kind: kind, level: level, word: word, target: r.target || '-', owner: r.registered ? r.owner : (r.manual ? 'Operator' : '-'),
        manual: r.manual && !r.registered, blocked: r.blocked, reason: r.reason
      };
    });
    rows.sort(function (a, b) {
      return (RANK[a.level] - RANK[b.level]) || (a.name < b.name ? -1 : a.name > b.name ? 1 : 0);
    });
    return rows;
  }

  function matches(filter, r) {
    switch (filter) {
      case 'offline': return r.kind === 'offline';
      case 'manual': return r.source === 'manual';
      case 'registered': return r.source === 'registered';
      case 'blocked': return r.blocked;
      default: return true;
    }
  }

  function plainFilter(f) {
    for (var i = 0; i < FILTERS.length; i++) { if (FILTERS[i][0] === f) { return f; } }
    return 'all';
  }



  function parsePort(text) {
    var s = String(text || '').trim();
    if (!/^\d+$/.test(s)) { return NaN; }
    var n = parseInt(s, 10);
    return n >= 1 && n <= 65535 ? n : NaN;
  }

  // mount wires the page's elements to the data. deps (all optional): now() for the clock.
  function mount(els, deps) {
    deps = deps || {};
    var now = deps.now || function () { return Date.now(); };
    var st = {
      state: null, rows: [], loaded: false, failed: false, filter: plainFilter(hl.queryParam('filter')),
      busy: false, open: null, tableSig: null, chipSig: null, chips: {}, panel: null
    };
    var ctrl = {};

    function setError(text) { els.error.textContent = text; els.error.hidden = !text; }
    function setNotice(text) { els.notice.textContent = text; els.notice.hidden = !text; }

    function activeNamespaces() {
      return hl.arr(hl.obj(st.state).namespaces).filter(function (n) { return hl.obj(n).state === 'active'; }).map(function (n) { return label(n.name, 63); });
    }

    // ----- the report note and the summary line

    function renderReport() {
      var box = els.reportnote;
      hl.clear(box);
      var ing = st.state ? hl.obj(st.state).ingress : null;
      if (!ing) { box.hidden = true; return; }
      var rep = hl.obj(ing.report);
      var errs = hl.arr(rep.errors);
      var stale = !!ing.stale;
      if (!stale && errs.length === 0) { box.hidden = true; return; }
      if (stale) {
        box.appendChild(hl.status('warn', 'The ingress report is stale'));
        box.appendChild(hl.el('p', 't2 small', 'It reported ' + hl.relTime(String(ing.received_at || ''), now()) + '. It may be down or unable to reach this app, so the states below are the last known ones.'));
      }
      if (errs.length) {
        box.appendChild(hl.status('warn', errs.length === 1 ? 'The ingress reports 1 error' : 'The ingress reports ' + errs.length + ' errors'));
        var ul = hl.el('ul', 't2 small');
        errs.slice(0, 20).forEach(function (e) { ul.appendChild(hl.el('li', null, label(e, 300))); });
        box.appendChild(ul);
      }
      box.hidden = false;
    }

    function summary() {
      if (!st.loaded) { return st.failed ? 'Could not load the funnel. Retrying.' : ''; }
      var served = st.rows.filter(function (r) { return !r.blocked; }).length;
      var text = served + (served === 1 ? ' public route. ' : ' public routes. ');
      var ing = hl.obj(st.state).ingress;
      if (!ing) { text += 'The ingress has not reported yet.'; }
      else {
        var n = hl.arr(hl.obj(ing.report).errors).length;
        text += 'Ingress reported ' + hl.relTime(String(ing.received_at || ''), now()) + (n === 0 ? ' with no errors.' : ' with ' + n + (n === 1 ? ' error.' : ' errors.'));
      }
      if (st.failed) { text += ' The data may be out of date.'; }
      return text;
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
      return fn().then(function (text) {
        st.busy = false;
        if (text) { setNotice(text); }
        return ctrl.reload();
      }, function (err) {
        st.busy = false;
        setError(failWord + ': ' + label(err && err.message, 200));
        render(true);
      });
    }

    function doBlock(r, reason) {
      run(function () {
        return hl.api('POST', '/funnel/blocks', { ns: r.ns, service: r.service, reason: String(reason || '').trim() }).then(function () {
          return 'Blocked ' + r.name + '. The ingress stops serving it on its next pull.';
        });
      }, 'Block failed for ' + r.name);
    }
    function doUnblock(r) {
      run(function () {
        return hl.api('DELETE', '/funnel/blocks/' + encodeURIComponent(r.ns) + '/' + encodeURIComponent(r.service)).then(function () {
          return 'Unblocked ' + r.name + '.';
        });
      }, 'Unblock failed for ' + r.name);
    }
    function doRemove(r) {
      run(function () {
        return hl.api('DELETE', '/funnel/routes/' + encodeURIComponent(r.ns) + '/' + encodeURIComponent(r.service)).then(function () {
          return 'Removed ' + r.name + '.';
        });
      }, 'Remove failed for ' + r.name);
    }

    function openConfirm(r, kind, td) {
      var panel;
      if (kind === 'block') {
        var reason = hl.el('input', 'input');
        reason.setAttribute('type', 'text');
        reason.setAttribute('maxlength', '500');
        reason.setAttribute('autocomplete', 'off');
        reason.setAttribute('aria-label', 'Reason');
        panel = hl.confirmPanel({
          title: 'Block ' + r.name + '?', object: r.name,
          consequence: 'The ingress stops serving it on its next pull. The route stays, and Unblock turns it back on.',
          confirmLabel: 'Block ' + r.name, cancelLabel: 'Keep serving', icon: 'ban', into: td,
          onConfirm: function () { doBlock(r, reason.value); }, onCancel: closeOpen
        });
        var field = hl.el('label', 'field', 'Reason (optional, shown to operators)');
        field.appendChild(reason);
        panel.insertBefore(field, panel.children[panel.children.length - 1]);
        reason.focus();
      } else {
        panel = hl.confirmPanel({
          title: 'Remove ' + r.name + '?', object: r.name,
          consequence: 'The manual route is deleted and the ingress stops serving it on its next pull. Add it again to bring it back.',
          confirmLabel: 'Remove ' + r.name, cancelLabel: 'Keep', icon: 'trash', into: td,
          onConfirm: function () { doRemove(r); }, onCancel: closeOpen
        });
      }
      return panel;
    }

    function openPanel(r, kind) {
      if (st.busy) { return; }
      if (st.open && st.open.key === r.key && st.open.kind === kind) { return; }
      st.open = { key: r.key, kind: kind, el: null, cell: null };
      render(true);
      if (st.open && st.open.cell) { st.open.el = openConfirm(r, kind, st.open.cell); }
    }

    // ----- the table

    function actionsOf(r) {
      var out = [];
      if (r.blocked) { out.push({ key: 'unblock', label: 'Unblock', icon: null, fn: function () { doUnblock(r); } }); }
      else { out.push({ key: 'block', label: 'Block', icon: 'ban', fn: function () { openPanel(r, 'block'); } }); }
      if (r.manual) { out.push({ key: 'remove', label: 'Remove', icon: 'trash', fn: function () { openPanel(r, 'remove'); } }); }
      return out;
    }

    function renderTable(force) {
      var rows = st.rows.filter(function (r) { return matches(st.filter, r); });
      if (st.open && !st.rows.some(function (r) { return r.key === st.open.key; })) { st.open = null; }
      var sig = JSON.stringify([rows, st.filter, st.open && [st.open.key, st.open.kind], st.busy]);
      if (!force && st.open && st.open.el) { return; }
      if (!force && sig === st.tableSig) { return; }
      st.tableSig = sig;
      var fk = hl.focusKey(els.rows);
      hl.clear(els.rows);
      if (st.open) { st.open.cell = null; }
      if (rows.length === 0) {
        var tr0 = hl.el('tr');
        var td0 = hl.el('td', 't2', 'No routes match this filter.');
        td0.setAttribute('colspan', String(COLUMNS));
        tr0.appendChild(td0);
        els.rows.appendChild(tr0);
      }
      rows.forEach(function (r) {
        var tr = hl.el('tr', st.open && st.open.key === r.key ? 'sel' : '');
        tr.appendChild(hl.el('td', 'mono name', r.name));
        var state = hl.el('td');
        state.appendChild(hl.status(r.level, r.word));
        if (r.blocked && r.reason) { state.setAttribute('title', 'Reason: ' + r.reason); }
        tr.appendChild(state);
        tr.appendChild(hl.el('td', 'mono ' + (r.blocked ? 't3' : 't2'), r.target));
        tr.appendChild(hl.el('td', 't2', r.sourceWord));
        tr.appendChild(hl.el('td', 't2', r.owner));
        var act = hl.el('td', 'act');
        var box = hl.el('div', 'rowbtns');
        actionsOf(r).forEach(function (a) {
          var b = keyed(hl.button(a.label, a.icon, 'sm quiet', a.fn), 'act:' + r.key + ':' + a.key);
          b.disabled = st.busy;
          box.appendChild(b);
        });
        act.appendChild(box);
        tr.appendChild(act);
        els.rows.appendChild(tr);
        if (st.open && st.open.key === r.key) {
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
      var show = st.rows.length > 0;
      els.chips.hidden = !show;
      var sig = show ? 'on' : 'off';
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

    function renderEmpty() {
      var empty = st.loaded && st.rows.length === 0;
      els.empty.hidden = !empty;
      if (!empty) { return; }
      els.emptytext.textContent = activeNamespaces().length
        ? 'Add a route to publish a tailnet service, or let a node register one with a publisher key.'
        : 'Routes need an active namespace. Create one under Namespaces & keys, then add a route or mint a publisher key for a node to register its own.';
    }

    function renderAdd() {
      var spaces = activeNamespaces().length;
      var sig = spaces > 0 ? 'on' : 'off';
      if (st.addSig === sig) { return; }
      st.addSig = sig;
      hl.clear(els.addslot);
      var b = keyed(hl.button('Add route', 'plus', '', function () { openAddPanel(); }), 'add');
      b.disabled = spaces === 0;
      if (b.disabled) { b.setAttribute('title', 'Create and confirm a namespace first.'); }
      els.addslot.appendChild(b);
    }

    function render(force) {
      els.sub.textContent = summary();
      renderReport();
      renderAdd();
      renderEmpty();
      els.tablewrap.hidden = st.rows.length === 0;
      renderChips();
      renderTable(force);
    }

    // ----- the add route panel

    function closeAddPanel() {
      if (!st.panel) { return; }
      var back = st.panel.opener;
      st.panel.root.remove();
      st.panel = null;
      if (back && back.focus) { back.focus(); }
    }

    function openAddPanel() {
      if (st.panel) { return; }
      var spaces = activeNamespaces();
      if (!spaces.length) { return; }
      var p = { opener: typeof document !== 'undefined' ? document.activeElement : null, saving: false };
      p.root = hl.el('section', 'panel');
      p.root.setAttribute('aria-label', 'Add route');
      var head = hl.el('div', 'panelhead');
      head.appendChild(hl.el('h2', null, 'Add a manual route'));
      p.root.appendChild(head);
      p.root.appendChild(hl.el('p', 't2', 'Publishes a tailnet service that does not register itself. The target must be a node carrying the namespace tag, and the port must be inside the namespace\'s ports.'));
      var form = hl.el('form', 'stack');
      form.setAttribute('novalidate', '');
      function input(type) { var i = hl.el('input', 'input'); i.setAttribute('type', type); i.setAttribute('autocomplete', 'off'); return i; }
      function select(values) {
        var s = hl.el('select', 'input');
        values.forEach(function (v) { var o = hl.el('option', null, v); o.setAttribute('value', v); s.appendChild(o); });
        s.value = values[0];
        return s;
      }
      function field(text, ctl) { var l = hl.el('label', 'field grow', text); l.appendChild(ctl); return l; }
      var ns = select(spaces);
      var service = input('text');
      service.setAttribute('maxlength', '63');
      keyed(service, 'service');
      var target = input('text');
      target.setAttribute('maxlength', '200');
      var port = input('text');
      port.setAttribute('inputmode', 'numeric');
      var scheme = select(['http', 'https']);
      var tls = hl.el('input');
      tls.setAttribute('type', 'checkbox');
      var tlsl = hl.el('label', 'row');
      tlsl.appendChild(tls);
      tlsl.appendChild(hl.el('span', null, 'Skip TLS verification'));
      var r1 = hl.el('div', 'row');
      r1.appendChild(field('Namespace', ns));
      r1.appendChild(field('Service', service));
      var r2 = hl.el('div', 'row');
      r2.appendChild(field('Target (node name or tailnet address)', target));
      r2.appendChild(field('Port', port));
      r2.appendChild(field('Scheme', scheme));
      form.appendChild(r1);
      form.appendChild(r2);
      form.appendChild(tlsl);
      var err = hl.el('div', 'note crit');
      err.setAttribute('role', 'alert');
      err.hidden = true;
      form.appendChild(err);
      var btns = hl.el('div', 'rowbtns start');
      var submit = hl.el('button', 'btn primary', 'Add route');
      submit.setAttribute('type', 'submit');
      btns.appendChild(submit);
      btns.appendChild(hl.button('Cancel', null, 'quiet', closeAddPanel));
      form.appendChild(btns);
      p.root.appendChild(form);
      p.root.addEventListener('keydown', function (ev) { if (ev && ev.key === 'Escape') { closeAddPanel(); } });
      form.addEventListener('submit', function (ev) {
        if (ev && ev.preventDefault) { ev.preventDefault(); }
        if (p.saving) { return; }
        var svc = String(service.value || '').trim();
        var tgt = String(target.value || '').trim();
        var pn = parsePort(port.value);
        var problem = '';
        if (!svc) { problem = 'Service is required.'; }
        else if (!tgt) { problem = 'Target is required.'; }
        else if (isNaN(pn)) { problem = 'Port must be a number from 1 to 65535.'; }
        if (problem) { err.textContent = problem; err.hidden = false; return; }
        err.hidden = true;
        p.saving = true;
        submit.disabled = true;
        hl.api('POST', '/funnel/routes', { ns: ns.value, service: svc, target: tgt, port: pn, scheme: scheme.value, insecure_skip_verify: !!tls.checked }).then(function () {
          p.saving = false;
          closeAddPanel();
          setNotice('Added ' + svc + '.' + ns.value + '. The ingress picks it up on its next pull.');
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
      service.focus();
    }

    // ----- control surface

    ctrl.setFilter = function (f) {
      st.filter = plainFilter(f);
      hl.setQueryParam('filter', st.filter === 'all' ? null : st.filter);
      st.open = null;
      render(true);
    };
    ctrl.update = function (d) {
      st.state = hl.obj(d);
      st.rows = serviceRows(st.state);
      st.loaded = true;
      render();
    };
    ctrl.setFailed = function (f) { st.failed = !!f; render(); };
    ctrl.reload = function () {
      return hl.api('GET', '/funnel/state').then(function (j) {
        ctrl.update(j);
        ctrl.setFailed(false);
      }, function () { ctrl.setFailed(true); });
    };
    ctrl.openAddPanel = openAddPanel;
    ctrl.state = st;
    render();
    return ctrl;
  }

  function init() {
    var root = document.getElementById('funnel');
    if (!root) { return; }
    var $ = function (id) { return document.getElementById(id); };
    var ctrl = mount({
      sub: $('sub'), error: $('error'), notice: $('notice'), reportnote: $('reportnote'), panel: $('panel'), chips: $('chips'),
      empty: $('empty'), emptytext: $('emptytext'), tablewrap: $('tablewrap'), rows: $('rows'), addslot: $('addslot')
    });
    var poller = hl.poll(ctrl.reload, POLL_MS);
    document.addEventListener('visibilitychange', function () { if (!document.hidden) { poller.now(); } });
  }

  hl.funnel = { serviceRows: serviceRows, mount: mount, init: init };
  init();
})();
