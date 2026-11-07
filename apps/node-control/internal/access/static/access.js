// headlamp console: the Access page (#123), roles and grants. Needs /_ui/hl.js first (the shell loads it).
// Network data goes in through textContent only. The server checks every rule again (no escalation, no self-grant);
// the page only hides what the caller cannot do.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }
  var $ = function (id) { return document.getElementById(id); };
  var state = null;

  function show(id, text) { var n = $(id); n.textContent = text || ''; n.hidden = !text; }
  function done(msg) { show('error', ''); show('notice', msg); return load(); }
  function failed(e) { show('notice', ''); show('error', e && e.message ? e.message : 'The request failed.'); }

  function input(ph, label) {
    var i = hl.el('input');
    i.setAttribute('placeholder', ph);
    i.setAttribute('aria-label', label);
    return i;
  }
  function split(v) { return String(v || '').split(',').map(function (s) { return s.trim(); }).filter(Boolean); }

  function setGrant(subject, roles) {
    return hl.api('POST', '/admin/grants', { subject: subject, roles: roles, version: state.version })
      .then(function () { return done(roles.length ? 'Grant saved.' : 'Grant removed.'); }, failed);
  }

  function grantRow(g, source) {
    var tr = hl.el('tr');
    tr.appendChild(hl.el('td', null, hl.plain(g.subject, 200)));
    tr.appendChild(hl.el('td', null, hl.arr(g.roles).map(function (r) { return hl.plain(r, 64); }).join(', ')));
    tr.appendChild(hl.el('td', 't2', source));
    var td = hl.el('td');
    if (source === 'store' && state.can_admin && g.subject !== state.me) {
      td.appendChild(hl.button('Remove', 'trash', 'sm', function () { setGrant(g.subject, []); }));
    }
    tr.appendChild(td);
    return tr;
  }

  function roleRow(r) {
    var tr = hl.el('tr');
    tr.appendChild(hl.el('td', null, hl.plain(r.name, 64)));
    tr.appendChild(hl.el('td', null, hl.arr(r.perms).join(', ')));
    tr.appendChild(hl.el('td', 't2', r.source));
    var td = hl.el('td');
    if (r.source === 'store' && state.can_admin) {
      td.appendChild(hl.button('Delete', 'trash', 'sm', function () {
        hl.api('DELETE', '/admin/roles/' + encodeURIComponent(r.name)).then(function () { return done('Role deleted.'); }, failed);
      }));
    }
    tr.appendChild(td);
    return tr;
  }

  function forms() {
    var gf = $('grantform'), rf = $('roleform'), is = $('importslot');
    hl.clear(gf); hl.clear(rf); hl.clear(is);
    if (!state.can_admin) { return; }
    var sub = input('alice@ or group:staff', 'Subject');
    var roles = input('viewer, operator', 'Roles');
    gf.appendChild(sub); gf.appendChild(roles);
    gf.appendChild(hl.button('Set grant', 'check', 'primary sm', function () { setGrant(sub.value, split(roles.value)); }));

    var name = input('role name', 'Role name');
    var perms = input('nodes.operate, tasks.view', 'Permissions');
    rf.appendChild(name); rf.appendChild(perms);
    rf.appendChild(hl.button('Save role', 'check', 'primary sm', function () {
      hl.api('PUT', '/admin/roles/' + encodeURIComponent(name.value.trim()), { perms: split(perms.value), version: state.version })
        .then(function () { return done('Role saved.'); }, failed);
    }));

    var file = hl.el('input');
    file.type = 'file'; file.accept = 'application/json,.json'; file.hidden = true;
    file.addEventListener('change', function () {
      var f = file.files && file.files[0];
      if (!f) { return; }
      f.text().then(function (t) {
        var body;
        try { body = JSON.parse(t); } catch (e) { failed(new Error('The file is not JSON.')); return; }
        return hl.api('POST', '/admin/import', body).then(function () { return done('Imported. The stored roles and grants were replaced.'); }, failed);
      });
    });
    is.appendChild(file);
    is.appendChild(hl.button('Import', 'plus', null, function () { file.click(); }));
  }

  function render() {
    var g = $('grants'), r = $('roles');
    hl.clear(g); hl.clear(r);
    hl.arr(state.file_grants).forEach(function (x) { g.appendChild(grantRow(x, 'file')); });
    hl.arr(state.stored_grants).forEach(function (x) { g.appendChild(grantRow(x, 'store')); });
    hl.arr(state.roles).forEach(function (x) { r.appendChild(roleRow(x)); });
    forms();
  }

  function loadAudit() {
    return hl.api('GET', '/admin/audit').then(function (es) {
      var ul = $('audit');
      hl.clear(ul);
      hl.arr(es).slice(0, 50).forEach(function (e) {
        e = hl.obj(e);
        ul.appendChild(hl.el('li', null, [hl.relTime ? hl.relTime(e.at) : e.at, hl.plain(e.actor, 100), hl.plain(e.action, 30),
          hl.plain(e.target, 200), hl.plain(e.detail, 300)].filter(Boolean).join(' · ')));
      });
    }, function () { /* the audit needs audit.view; the page works without it */ });
  }

  function load() {
    return hl.api('GET', '/admin/state').then(function (s) {
      state = hl.obj(s);
      render();
      return loadAudit();
    }, failed);
  }

  load();
})();
