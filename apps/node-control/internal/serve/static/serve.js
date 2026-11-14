// headlamp console: the Serve page (#129). Needs /_ui/hl.js first (the shell loads it). Everything that came from
// the network goes in through textContent only; markup is never built from strings.
//
// Secrets: a service key exists only in the "New key" panel, right after it was minted (a new service or a rotation).
// It is never put in a table, the URL or storage. Its Done button, or leaving the page, removes it.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var POLL_MS = 15000;
  var COLUMNS = 6;
  var WARN_DAYS = 14;
  var DAY_MS = 24 * 3600 * 1000;

  function label(v, n) { return hl.plain(v, n || 100); }
  function keyed(el, key) { el.setAttribute('data-fk', key); return el; }

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

  // serviceRows turns GET /serve/state into one row per service: its state word and level, its keys newest first
  // with how long each has left, and its proxies. A key that expires within WARN_DAYS makes the row "warn".
  function serviceRows(state, nowMs) {
    state = hl.obj(state);
    return hl.arr(state.services).map(function (s) {
      s = hl.obj(s);
      var keys = hl.arr(s.keys).map(function (k) {
        k = hl.obj(k);
        var exp = Date.parse(String(k.expires || ''));
        var left = isNaN(exp) ? NaN : exp - nowMs;
        return { id: label(k.id, 20), expires: String(k.expires || ''), left: left, expired: !isNaN(left) && left <= 0,
          soon: !isNaN(left) && left > 0 && left <= WARN_DAYS * DAY_MS };
      });
      var nodes = hl.arr(s.nodes).map(function (n) {
        n = hl.obj(n);
        return { name: label(n.name, 63), online: !!n.online, ips: hl.arr(n.ips).map(function (a) { return label(a, 45); }), key: label(n.key_id, 20) };
      });
      var online = nodes.filter(function (n) { return n.online; }).length;
      var newest = keys[0];
      var level, word;
      if (newest && newest.expired) { level = 'crit'; word = 'Key expired'; }
      else if (online === 0) { level = 'warn'; word = nodes.length ? 'Offline' : 'No proxy yet'; }
      else if (newest && newest.soon) { level = 'warn'; word = 'Key expires soon'; }
      else { level = 'ok'; word = online === 1 ? 'Online' : online + ' online'; }
      if (keys.length > 1 && level === 'ok') { word += ', rotating'; }
      var name = label(s.name, 253);
      return { id: label(s.id, 20), name: name, wildcard: name.indexOf('*.') === 0, ns: label(s.namespace, 32), note: label(s.note, 200),
        created: String(s.created || ''), keys: keys, nodes: nodes, online: online, level: level, word: word };
    });
  }

  // sidecarEnv is the sample environment of a funnel-sidecar in serve mode for a new service.
  function sidecarEnv(state, authKey) {
    var server = label(hl.obj(state).dns_server, 45) || '<node-control address>';
    return ['FUNNEL_SERVE_URL=http://' + server, 'TS_AUTHKEY=' + authKey, 'FUNNEL_SERVICES=web=443@127.0.0.1:8080'].join('\n');
  }

  function leftText(k) {
    if (isNaN(k.left)) { return 'unknown expiry'; }
    if (k.expired) { return 'expired'; }
    var d = Math.floor(k.left / DAY_MS);
    return d >= 1 ? 'expires in ' + d + (d === 1 ? ' day' : ' days') : 'expires today';
  }

  // mount wires the page's elements to the data. deps (all optional): now() for the clock, copy(text) for the
  // clipboard, later(fn, ms) for timers.
  function mount(els, deps) {
    deps = deps || {};
    var now = deps.now || function () { return Date.now(); };
    var copy = deps.copy || copyText;
    var later = deps.later || function (fn, ms) { return setTimeout(fn, ms); };
    var st = { state: null, rows: [], loaded: false, failed: false, panel: null, confirm: null, secrets: [],
      focus: label(hl.queryParam('service'), 20) };
    var ctrl = {};

    function canAdmin() { return !!hl.obj(st.state).can_admin; }
    function setError(text) { els.error.textContent = text; els.error.hidden = !text; }
    function setNotice(text) { els.notice.textContent = text; els.notice.hidden = !text; }

    // ----- zone, DNS and summary

    function renderZone() {
      var s = hl.obj(st.state);
      hl.clear(els.zone);
      function row(k, v, mono) {
        els.zone.appendChild(hl.el('dt', null, k));
        var dd = hl.el('dd', mono ? 'mono' : null);
        if (typeof v === 'string') { dd.textContent = v; } else { dd.appendChild(v); }
        els.zone.appendChild(dd);
      }
      var zone = label(s.zone, 253);
      row('Zone', zone, true);
      if (s.cert_source === 'acme') {
        var names = Array.isArray(s.acme_names) ? s.acme_names.map(function (n) { return label(n, 253); }).join(', ') : '';
        row('Certificate', 'public ACME CA: ' + (names || 'not issued yet'), true);
        if (s.acme_until) { row('Valid until', label(s.acme_until, 40), true); }
        if (s.acme_error) { row('Last ACME error', label(s.acme_error, 300)); }
        var dl = document.querySelector('a[href="/serve/ca.crt"]');
        if (dl) { dl.hidden = true; }
      } else {
        row('CA fingerprint', label(s.ca_fingerprint, 128) || 'unknown', true);
      }
      var server = label(s.dns_server, 45);
      var split = hl.el('pre', 'code');
      split.textContent = 'dns:\n  nameservers:\n    split:\n      ' + zone + ':\n        - ' + (server || '<node-control tailnet IPv4>');
      row('headscale split', split);

      var note = els.dnsnote;
      hl.clear(note);
      var err = label(s.dns_error, 300);
      if (err) {
        note.appendChild(hl.status('warn', 'DNS answers are not refreshed'));
        note.appendChild(hl.el('p', 't2 small', 'The last refresh failed: ' + err + '. Names keep their last answers.'));
        note.hidden = false;
      } else { note.hidden = true; }

      var n = st.rows.length, on = st.rows.filter(function (r) { return r.online > 0; }).length;
      var text = n === 0 ? 'No services.' : n + (n === 1 ? ' service, ' : ' services, ') + on + ' online.';
      if (s.dns_refreshed) { text += ' DNS refreshed ' + hl.relTime(String(s.dns_refreshed), now()) + '.'; }
      if (st.failed) { text += ' The last update failed; this is the last known state.'; }
      els.sub.textContent = text;
    }

    // ----- the table

    function actionsFor(r) {
      if (!canAdmin()) { return []; }
      var out = [];
      if (r.keys.length < 2) { out.push({ key: 'addkey', label: 'Add key', icon: 'key', fn: function () { addKey(r); } }); }
      else { out.push({ key: 'retire', label: 'Retire old key', icon: 'ban', fn: function () { askRetire(r, r.keys[r.keys.length - 1]); } }); }
      out.push({ key: 'delete', label: 'Delete', icon: 'trash', fn: function () { askDelete(r); } });
      return out;
    }

    function keysCell(r) {
      var td = hl.el('td', 'small');
      r.keys.forEach(function (k, i) {
        var line = hl.el('div', 'nowrap');
        line.appendChild(hl.el('span', 'mono', '#' + k.id + ' '));
        var text = leftText(k) + (r.keys.length > 1 ? (i === 0 ? ', new' : ', old') : '');
        line.appendChild(k.expired || k.soon ? hl.status(k.expired ? 'crit' : 'warn', text) : hl.el('span', 't2', text));
        td.appendChild(line);
      });
      if (!r.keys.length) { td.appendChild(hl.el('span', 't3', 'No key')); }
      return td;
    }

    function nodesCell(r) {
      var td = hl.el('td', 'small');
      r.nodes.forEach(function (n) {
        var line = hl.el('div');
        line.appendChild(hl.status(n.online ? 'ok' : 'off', n.name || 'unnamed'));
        line.appendChild(hl.el('span', 'mono t3', ' ' + n.ips.filter(function (a) { return a.indexOf(':') < 0; }).join(', ')));
        td.appendChild(line);
      });
      if (!r.nodes.length) { td.appendChild(hl.el('span', 't3', 'None joined')); }
      return td;
    }

    function renderTable() {
      var fk = hl.focusKey(els.rows);
      hl.clear(els.rows);
      st.rows.forEach(function (r) {
        var tr = hl.el('tr', st.focus === r.id ? 'sel' : '');
        var name = hl.el('td', 'mono name', r.name);
        if (r.wildcard) { name.appendChild(hl.el('span', 't3 small', ' wildcard')); }
        tr.appendChild(name);
        var state = hl.el('td');
        state.appendChild(hl.status(r.level, r.word));
        tr.appendChild(state);
        tr.appendChild(hl.el('td', 'mono t2', r.ns));
        tr.appendChild(keysCell(r));
        tr.appendChild(nodesCell(r));
        var act = hl.el('td', 'act');
        var box = hl.el('div', 'rowbtns');
        actionsFor(r).forEach(function (a) { box.appendChild(keyed(hl.button(a.label, a.icon, 'sm quiet', a.fn), 'act:' + r.id + ':' + a.key)); });
        act.appendChild(box);
        tr.appendChild(act);
        els.rows.appendChild(tr);
        if (st.confirm && st.confirm.id === r.id) {
          var dr = hl.el('tr');
          var cell = hl.el('td', 'detail');
          cell.setAttribute('colspan', String(COLUMNS));
          cell.appendChild(st.confirm.node);
          dr.appendChild(cell);
          els.rows.appendChild(dr);
        }
      });
      hl.refocus(els.rows, fk);
    }

    function render() {
      renderZone();
      var any = st.rows.length > 0;
      els.tablewrap.hidden = !any;
      els.empty.hidden = any || !st.loaded;
      els.emptytext.textContent = canAdmin()
        ? 'Add a service: a name in the zone and the namespace of its proxies. You get a key for the proxies once.'
        : 'An administrator adds services here.';
      hl.clear(els.addslot);
      if (canAdmin()) { els.addslot.appendChild(keyed(hl.button('Add service', 'plus', '', openAddPanel), 'add')); }
      if (st.loaded) { renderTable(); }
    }

    // ----- actions

    function done(text) { setError(''); setNotice(text); return ctrl.reload(); }
    function failed(what) { return function (e) { setError(what + ' failed: ' + label(e && e.message, 200)); }; }

    function addKey(r) {
      hl.api('POST', '/serve/services/' + encodeURIComponent(r.id) + '/keys', {}).then(function (j) {
        showSecret(label(hl.obj(j).auth_key, 200), r.name, false);
        return done('Added a key to ' + r.name + '. Give it to the proxies, then retire the old key.');
      }, failed('Adding a key to ' + r.name));
    }

    function askRetire(r, k) {
      var node = hl.confirmPanel({
        title: 'Retire key #' + k.id + '?', object: r.name,
        consequence: 'headscale expires the key. Proxies that joined with it keep running until they restart, but the name stops answering them at the next DNS refresh.',
        confirmLabel: 'Retire key', icon: 'ban',
        onCancel: function () { st.confirm = null; render(); },
        onConfirm: function () {
          st.confirm = null;
          hl.api('DELETE', '/serve/services/' + encodeURIComponent(r.id) + '/keys/' + encodeURIComponent(k.id)).then(function () {
            return done('Retired key #' + k.id + ' of ' + r.name + '.');
          }, failed('Retiring the key'));
        }
      });
      st.confirm = { id: r.id, node: node };
      render();
    }

    // askDelete needs the service name typed, as for the other destructive actions in the console.
    function askDelete(r) {
      var box = hl.el('div', 'note crit confirm');
      box.setAttribute('role', 'group');
      box.setAttribute('aria-label', 'Delete ' + r.name);
      box.appendChild(hl.el('h2', null, 'Delete ' + r.name + '?'));
      box.appendChild(hl.el('p', 't2', 'Its keys are expired in headscale and the name stops answering. Proxies on the tailnet lose their name. This cannot be undone.'));
      var field = hl.el('label', 'field', 'Type the name to confirm');
      var input = hl.el('input', 'input mono');
      input.setAttribute('autocomplete', 'off');
      field.appendChild(input);
      box.appendChild(field);
      var btns = hl.el('div', 'rowbtns');
      var go = hl.button('Delete service', 'trash', 'danger', function () {
        if (input.value.trim() !== r.name) { return; }
        st.confirm = null;
        hl.api('DELETE', '/serve/services/' + encodeURIComponent(r.id)).then(function () {
          clearSecretsFor(r.name);
          return done('Deleted ' + r.name + '.');
        }, failed('Deleting ' + r.name));
      });
      go.disabled = true;
      input.addEventListener('input', function () { go.disabled = input.value.trim() !== r.name; });
      function cancel() { st.confirm = null; render(); }
      input.addEventListener('keydown', function (ev) { if (ev && ev.key === 'Escape') { cancel(); } });
      btns.appendChild(hl.button('Cancel', null, '', cancel));
      btns.appendChild(go);
      box.appendChild(btns);
      st.confirm = { id: r.id, node: box };
      render();
      input.focus();
    }

    // ----- adding a service

    function closeAddPanel() {
      if (!st.panel) { return; }
      st.panel.root.remove();
      st.panel = null;
    }

    function openAddPanel() {
      if (st.panel) { return; }
      var p = { saving: false };
      p.root = hl.el('form', 'panel');
      p.root.setAttribute('novalidate', '');
      p.root.setAttribute('aria-label', 'Add service');
      p.root.appendChild(hl.el('h2', null, 'Add a service'));
      var zone = label(hl.obj(st.state).zone, 253);
      p.root.appendChild(hl.el('p', 't2', 'A name in ' + zone + ', or *.<label> for an ingress controller behind one proxy. Each proxy of the service joins with the key you get next.'));
      var form = hl.el('div', 'stack');
      function input() { var i = hl.el('input', 'input mono'); i.setAttribute('autocomplete', 'off'); return i; }
      function field(text, ctl) { var l = hl.el('label', 'field grow', text); l.appendChild(ctl); return l; }
      var name = input(), ns = input();
      name.setAttribute('placeholder', 'grafana or *.apps');
      ns.setAttribute('placeholder', 'mon');
      var r1 = hl.el('div', 'row');
      r1.appendChild(field('Name', name));
      r1.appendChild(field('Namespace', ns));
      form.appendChild(r1);
      var ttl = Number(hl.obj(st.state).key_ttl_hours) || 0;
      if (ttl) { form.appendChild(hl.el('p', 't2 small', 'The key is reusable and ephemeral, and expires in ' + Math.round(ttl / 24) + ' days.')); }
      var err = hl.el('div', 'note crit');
      err.hidden = true;
      form.appendChild(err);
      var btns = hl.el('div', 'rowbtns start');
      var submit = hl.el('button', 'btn primary', 'Add service');
      submit.setAttribute('type', 'submit');
      btns.appendChild(submit);
      btns.appendChild(hl.button('Cancel', null, 'quiet', closeAddPanel));
      form.appendChild(btns);
      p.root.appendChild(form);
      p.root.addEventListener('keydown', function (ev) { if (ev && ev.key === 'Escape') { closeAddPanel(); } });
      p.root.addEventListener('submit', function (ev) {
        ev.preventDefault();
        if (p.saving) { return; }
        var n = name.value.trim().toLowerCase(), s = ns.value.trim().toLowerCase();
        var problem = !n ? 'Enter a name.' : (!/^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$/.test(s) ? 'The namespace is 1 to 32 lower-case letters, digits or dashes.' : '');
        if (problem) { err.textContent = problem; err.hidden = false; return; }
        err.hidden = true;
        p.saving = true;
        submit.disabled = true;
        hl.api('POST', '/serve/services', { name: n, namespace: s }).then(function (j) {
          j = hl.obj(j);
          var full = label(hl.obj(j.service).name, 253);
          closeAddPanel();
          showSecret(label(j.auth_key, 200), full, true);
          return done('Added ' + full + '. Copy its key now: it is not shown again.');
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

    // ----- the key, once

    // showSecret puts a new key in the side panel, the only place its text lives. withEnv adds a sample sidecar
    // environment for a new service. Done, deleting the service or leaving the page removes it.
    function showSecret(key, service, withEnv) {
      var root = hl.el('div', 'panel');
      root.setAttribute('role', 'group');
      root.setAttribute('aria-label', 'New key');
      root.appendChild(hl.el('h2', null, 'New key: copy it now'));
      root.appendChild(hl.el('p', 't2 small', 'This key for ' + service + ' is shown once and is never stored or logged here. Put it in the secret of the proxies. Done removes it from this page.'));
      var box = hl.el('div', 'secret', key);
      box.setAttribute('aria-label', 'Service key');
      root.appendChild(box);
      var note = hl.el('p', 't2 small');
      note.hidden = true;
      root.appendChild(note);
      var row = hl.el('div', 'rowbtns start');
      var cp = hl.button('Copy key', 'copy', 'primary', function () {
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
      var secret = { root: root, box: box, env: null, service: service };
      row.appendChild(hl.button('Done', null, 'quiet', function () { clearSecret(secret); }));
      root.appendChild(row);
      if (withEnv) {
        root.appendChild(hl.el('h3', null, 'funnel-sidecar environment'));
        var env = hl.el('pre', 'code', sidecarEnv(st.state, key));
        root.appendChild(env);
        secret.env = env;
        root.appendChild(hl.el('p', 't2 small', 'Set FUNNEL_SERVICES to the port of your backend.'));
      }
      els.side.appendChild(root);
      st.secrets.push(secret);
      cp.focus();
    }

    function clearSecret(secret) {
      secret.box.textContent = '';
      if (secret.env) { secret.env.textContent = ''; }
      secret.root.remove();
      st.secrets = st.secrets.filter(function (s) { return s !== secret; });
    }
    function clearSecretsFor(service) { st.secrets.filter(function (s) { return s.service === service; }).forEach(clearSecret); }
    function clearSecrets() { st.secrets.slice().forEach(clearSecret); }

    // ----- control surface

    ctrl.update = function (d) {
      st.state = hl.obj(d);
      st.rows = serviceRows(st.state, now());
      st.loaded = true;
      if (!canAdmin()) { closeAddPanel(); st.confirm = null; }
      render();
    };
    ctrl.setFailed = function (f) { st.failed = !!f; render(); };
    ctrl.reload = function () {
      return hl.api('GET', '/serve/state').then(function (j) {
        ctrl.update(j);
        ctrl.setFailed(false);
      }, function () { ctrl.setFailed(true); });
    };
    ctrl.openAddPanel = openAddPanel;
    ctrl.clearSecrets = clearSecrets;
    ctrl.state = st;
    render();
    return ctrl;
  }

  function init() {
    var root = document.getElementById('serve');
    if (!root) { return; }
    var $ = function (id) { return document.getElementById(id); };
    var ctrl = mount({
      sub: $('sub'), error: $('error'), notice: $('notice'), dnsnote: $('dnsnote'), zone: $('zone'), panel: $('panel'),
      empty: $('empty'), emptytext: $('emptytext'), tablewrap: $('tablewrap'), rows: $('rows'), addslot: $('addslot'), side: $('side')
    });
    var poller = hl.poll(ctrl.reload, POLL_MS);
    document.addEventListener('visibilitychange', function () { if (!document.hidden) { poller.now(); } });
    window.addEventListener('pagehide', function () { ctrl.clearSecrets(); });
  }

  hl.serve = { serviceRows: serviceRows, sidecarEnv: sidecarEnv, mount: mount, init: init };
  init();
})();
