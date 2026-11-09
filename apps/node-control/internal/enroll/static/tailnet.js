// headlamp console: tailnet grants (#139). Needs /_ui/hl.js first. Everything from the network goes in through
// textContent only; markup is never built from strings.
//
// Bridge page (/enroll/approve?name=<bridge>): the tailnet_access selector (off, all, grants), the targets, the grant
// groups with their status, and the ACL preview with "Apply through policy console".
// Devices page (/enroll): one row per device with its targets on each bridge in grants mode, direct and through
// its owner, and check boxes to change the device grants and the owner grants.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var MODES = ['off', 'all', 'grants'];
  var STATUS = {
    ok: ['ok', 'ok'], needs_provisioning: ['warn', 'needs provisioning'],
    pending_acl: ['warn', 'pending ACL'], over_limit: ['crit', 'over the limit']
  };

  function names(list) { return hl.arr(list).map(function (x) { return hl.plain(String(x), 64); }).join(', '); }

  // targetsOf: the targets a device holds on a bridge, as {direct, owner} name lists.
  function targetsOf(view, bridge, dev) {
    var out = { direct: [], owner: [] };
    hl.arr(view && view.grants).forEach(function (g) {
      if (!g || g.bridge !== bridge) { return; }
      if (g.kind === 'device' && g.subject === String(dev.id)) { out.direct.push(g.target); }
      if (g.kind === 'owner' && dev.owner && g.subject === dev.owner) { out.owner.push(g.target); }
    });
    return out;
  }

  function grantBridges(view) {
    return hl.arr(view && view.bridges).filter(function (b) { return b && b.mode === 'grants'; });
  }

  function table(head) {
    var wrap = hl.el('div', 'tw');
    var t = wrap.appendChild(hl.el('table'));
    var tr = t.appendChild(hl.el('thead')).appendChild(hl.el('tr'));
    head.forEach(function (h) { tr.appendChild(hl.el('th', '', h)); });
    return { wrap: wrap, body: t.appendChild(hl.el('tbody')) };
  }

  function row(body, cells) {
    var tr = body.appendChild(hl.el('tr'));
    cells.forEach(function (c) {
      if (c && typeof c === 'object') { tr.appendChild(hl.el('td')).appendChild(c); } else { tr.appendChild(hl.el('td', 't2', c)); }
    });
    return tr;
  }

  // checks: one check box per target, and a Save button that sends the checked names.
  function checks(into, all, held, onSave) {
    var box = into.appendChild(hl.el('div', 'stack'));
    var inputs = [];
    all.forEach(function (t) {
      var lab = box.appendChild(hl.el('label', 'small'));
      var cb = lab.appendChild(hl.el('input'));
      cb.setAttribute('type', 'checkbox');
      cb.checked = held.indexOf(t.name) >= 0;
      cb.value = t.name;
      lab.appendChild(document.createTextNode(' ' + hl.plain(t.name, 32) + ' (' + names(t.dst) + ')'));
      inputs.push(cb);
    });
    box.appendChild(hl.button('Save', null, 'sm primary', function () {
      onSave(inputs.filter(function (i) { return i.checked; }).map(function (i) { return i.value; }));
    }));
    return box;
  }

  // --- bridge page ---

  function mountBridge(els, bridge) {
    var st = { view: null, acl: null, error: '' };
    var ctrl = {};
    function setError(t) { st.error = t; els.error.textContent = t; els.error.hidden = !t; }
    function act(method, url, body) {
      setError('');
      return hl.api(method, url, body).then(function () { return ctrl.load(); }, function (e) { setError(hl.plain(e && e.message, 200)); });
    }
    function bridgeView() { return hl.arr(st.view && st.view.bridges).filter(function (b) { return b && b.name === bridge; })[0]; }

    function renderMode(b) {
      var p = els.root.appendChild(hl.el('div', 'rowbtns start'));
      p.appendChild(hl.el('span', 't2', 'Tailnet access: '));
      MODES.forEach(function (m) {
        var btn = p.appendChild(hl.button(m, null, 'sm ' + (b.mode === m ? 'primary' : 'quiet'), function () {
          act('POST', '/enroll/tailnet-grants/mode', { bridge: bridge, mode: m });
        }));
        btn.setAttribute('aria-pressed', b.mode === m ? 'true' : 'false');
      });
      if (b.mode === 'all') {
        p.appendChild(hl.button('Convert to grants', null, 'sm', function () {
          act('POST', '/enroll/tailnet-grants/convert', { bridge: bridge });
        }));
      }
    }

    function renderTargets() {
      els.root.appendChild(hl.el('h3', '', 'Tailnet targets'));
      var tb = table(['Name', 'Destinations', 'Source', '']);
      hl.arr(st.view.targets).forEach(function (t) {
        var del = t.source === 'console' ? hl.button('Remove', null, 'sm quiet', function () {
          act('DELETE', '/enroll/tailnet-grants/targets/' + encodeURIComponent(t.name), {});
        }) : '';
        row(tb.body, [hl.plain(t.name, 32), names(t.dst), t.source === 'config' ? 'enroll.json' : 'console', del]);
      });
      els.root.appendChild(tb.wrap);
      var form = els.root.appendChild(hl.el('div', 'rowbtns start'));
      var n = form.appendChild(hl.el('input'));
      n.setAttribute('aria-label', 'Target name');
      n.setAttribute('placeholder', 'name');
      var d = form.appendChild(hl.el('input'));
      d.setAttribute('aria-label', 'Destinations');
      d.setAttribute('placeholder', 'tag:nas:445,22 100.64.0.20:631');
      form.appendChild(hl.button('Add or change target', 'plus', 'sm', function () {
        act('POST', '/enroll/tailnet-grants/targets', { name: n.value.trim(), dst: d.value.trim().split(/\s+/).filter(Boolean) });
      }));
    }

    function renderGroups(b) {
      els.root.appendChild(hl.el('h3', '', 'Grant groups'));
      if (b.error) { els.root.appendChild(hl.el('div', 'note crit', hl.plain(b.error, 200))); }
      var tb = table(['Tag', 'Targets', 'Devices', 'Status']);
      hl.arr(b.groups).forEach(function (g) {
        var s = STATUS[g.status] || ['warn', 'unknown'];
        row(tb.body, [hl.plain(g.tag, 40), names(g.targets), String(hl.arr(g.devices).length), hl.status(s[0], s[1])]);
      });
      els.root.appendChild(tb.wrap);
      if (hl.arr(b.unresolved).length) {
        els.root.appendChild(hl.el('p', 't2 small', 'Not resolved (no access until node-control knows the group of the owner): ' + names(b.unresolved)));
      }
    }

    function renderACL() {
      var a = st.acl;
      if (!a) { return; }
      els.root.appendChild(hl.el('h3', '', 'ACL preview'));
      els.root.appendChild(hl.el('p', 't2 small', 'Status: ' + hl.plain(a.status, 40)));
      var tb = table(['Change', 'Source tag', 'Now', 'After']);
      hl.arr(a.changes).forEach(function (c) { row(tb.body, [hl.plain(c.op, 10), hl.plain(c.src, 40), names(c.old), names(c.new)]); });
      els.root.appendChild(tb.wrap);
      if (a.status === 'out_of_sync') {
        els.root.appendChild(hl.el('div', 'rowbtns start')).appendChild(hl.button('Apply through policy console', null, 'primary', function () {
          act('POST', '/enroll/tailnet-acl', {}).then(function () { els.root.appendChild(hl.el('p', 't2', 'Confirm the change on the Policy page.')); });
        }));
      }
    }

    function render() {
      hl.clear(els.root);
      var b = bridgeView();
      els.section.hidden = !b;
      if (!b) { return; }
      renderMode(b);
      if (b.mode === 'grants') { renderTargets(); renderGroups(b); }
      renderACL();
    }

    ctrl.load = function () {
      return Promise.all([hl.api('GET', '/enroll/tailnet-grants'), hl.api('GET', '/enroll/tailnet-acl').catch(function () { return null; })])
        .then(function (r) { st.view = r[0]; st.acl = r[1]; render(); }, function () { els.section.hidden = true; });
    };
    ctrl.state = st;
    return ctrl;
  }

  // --- devices page ---

  function mountDevices(els) {
    var st = { view: null, devices: [], open: null };
    var ctrl = {};
    function setError(t) { els.error.textContent = t; els.error.hidden = !t; }
    function save(bridge, kind, subject, targets) {
      setError('');
      return hl.api('POST', '/enroll/tailnet-grants/subject', { bridge: bridge, kind: kind, subject: subject, targets: targets })
        .then(function () { st.open = null; return ctrl.load(); }, function (e) { setError(hl.plain(e && e.message, 200)); });
    }

    function editor(cell, d) {
      grantBridges(st.view).forEach(function (b) {
        var held = targetsOf(st.view, b.name, d);
        cell.appendChild(hl.el('h4', '', 'Bridge ' + hl.plain(b.name, 63)));
        cell.appendChild(hl.el('p', 't2 small', 'This device'));
        checks(cell, hl.arr(st.view.targets), held.direct, function (ts) { save(b.name, 'device', String(d.id), ts); });
        if (d.owner) {
          cell.appendChild(hl.el('p', 't2 small', 'All devices of ' + (hl.plain(d.owner_email, 100) || hl.plain(d.owner, 100))));
          checks(cell, hl.arr(st.view.targets), held.owner, function (ts) { save(b.name, 'owner', d.owner, ts); });
        }
      });
    }

    function render() {
      hl.clear(els.rows);
      var bridges = grantBridges(st.view);
      els.section.hidden = bridges.length === 0;
      st.devices.filter(function (d) { return !d.revoked; }).forEach(function (d) {
        var parts = bridges.map(function (b) {
          var t = targetsOf(st.view, b.name, d);
          var all = t.direct.concat(t.owner.filter(function (x) { return t.direct.indexOf(x) < 0; }));
          return hl.plain(b.name, 63) + ': ' + (all.length ? names(all) : 'none');
        });
        var btn = hl.button(st.open === d.id ? 'Close' : 'Edit', 'edit', 'sm quiet', function () {
          st.open = st.open === d.id ? null : d.id; render();
        });
        row(els.rows, [hl.plain(d.name, 100), hl.plain(d.owner_email, 100) || hl.plain(d.owner, 100), parts.join('; '), btn]);
        if (st.open === d.id) {
          var tr = els.rows.appendChild(hl.el('tr'));
          var cell = tr.appendChild(hl.el('td', 'detail'));
          cell.setAttribute('colspan', '4');
          editor(cell, d);
        }
      });
    }

    ctrl.load = function () {
      return Promise.all([hl.api('GET', '/enroll/tailnet-grants'), hl.api('GET', '/enroll/devices')]).then(function (r) {
        st.view = r[0]; st.devices = hl.arr(r[1] && r[1].devices); render();
      }, function () { els.section.hidden = true; });
    };
    ctrl.state = st;
    return ctrl;
  }

  function init() {
    var $ = function (id) { return document.getElementById(id); };
    var sec = $('tailnet');
    if (!sec) { return; }
    if (sec.getAttribute('data-page') === 'bridge') {
      var name = hl.queryParam('name') || '';
      if (name) { mountBridge({ section: sec, root: $('tailnetroot'), error: $('tailneterror') }, name).load(); }
    } else {
      mountDevices({ section: sec, rows: $('tailnetrows'), error: $('tailneterror') }).load();
    }
  }

  hl.tailnet = { mountBridge: mountBridge, mountDevices: mountDevices, targetsOf: targetsOf, init: init };
  init();
})();
