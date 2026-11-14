// headlamp console: the DNS page (#285). Needs /_ui/hl.js first (the shell loads it). Everything that came from
// the network goes in through textContent only; markup is never built from strings.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var POLL_MS = 15000;
  var REC_COLUMNS = 7;
  var ZONE_COLUMNS = 4;
  var TYPES = ['A', 'CNAME', 'TXT', 'SRV'];

  function label(v, n) { return hl.plain(v, n || 100); }
  function keyed(el, key) { el.setAttribute('data-fk', key); return el; }
  function list(v, n) { return hl.arr(v).map(function (x) { return label(x, n || 253); }); }

  // dataText is the data of a record or rule in the one-line form the form takes back (parseData).
  function dataText(type, t) {
    t = hl.obj(t);
    switch (type) {
      case 'CNAME': return label(t.target, 253);
      case 'TXT': return list(t.text, 255).join('');
      case 'SRV': return t.target ? [Number(t.priority) || 0, Number(t.weight) || 0, Number(t.port) || 0, label(t.target, 253)].join(' ') : '';
    }
    if (t.node) { return 'node:' + label(t.node, 63); }
    if (t.tag) { return label(t.tag, 70); }
    return list(t.addresses, 45).join(', ');
  }

  // parseData turns the one-line form into the API fields of type. It returns {error} for a line it cannot read.
  // A: addresses separated by commas, node:<given name>, or tag:<name>. CNAME: a name. TXT: text, cut into
  // strings of 255 bytes. SRV: "priority weight port target".
  function parseData(type, text) {
    text = String(text || '').trim();
    if (!text) { return {}; }
    switch (type) {
      case 'CNAME': return { target: text.toLowerCase() };
      case 'TXT': {
        var parts = [];
        for (var i = 0; i < text.length; i += 255) { parts.push(text.slice(i, i + 255)); }
        return { text: parts };
      }
      case 'SRV': {
        var f = text.split(/\s+/);
        var nums = f.slice(0, 3).map(Number);
        if (f.length !== 4 || nums.some(function (n) { return !(n >= 0 && n <= 65535 && Math.floor(n) === n); })) {
          return { error: 'Write SRV data as: priority weight port target.' };
        }
        return { priority: nums[0], weight: nums[1], port: nums[2], target: f[3].toLowerCase() };
      }
    }
    if (/^node:/i.test(text)) { return { node: text.slice(5).trim().toLowerCase() }; }
    if (/^tag:/i.test(text)) { return { tag: text.toLowerCase() }; }
    return { addresses: text.split(/[\s,]+/).filter(Boolean) };
  }

  // parseRules reads one rule per line: "<who> = <data>". <who> is logins and tag:<name> items, separated by
  // commas. An empty <data> hides the name from those clients.
  function parseRules(type, text) {
    var out = [];
    var lines = String(text || '').split('\n');
    for (var i = 0; i < lines.length; i++) {
      var line = lines[i].trim();
      if (!line) { continue; }
      var eq = line.indexOf('=');
      if (eq < 0) { return { error: 'Rule ' + (out.length + 1) + ': write it as <users and tags> = <data>.' }; }
      var who = line.slice(0, eq).split(',').map(function (s) { return s.trim(); }).filter(Boolean);
      var rule = { users: who.filter(function (s) { return s.indexOf('tag:') !== 0; }), tags: who.filter(function (s) { return s.indexOf('tag:') === 0; }) };
      var d = parseData(type, line.slice(eq + 1));
      if (d.error) { return { error: 'Rule ' + (out.length + 1) + ': ' + d.error }; }
      Object.keys(d).forEach(function (k) { rule[k] = d[k]; });
      rule.type = type;
      out.push(rule);
    }
    return { rules: out };
  }

  function rulesText(type, rules) {
    return hl.arr(rules).map(function (r) {
      r = hl.obj(r);
      return list(r.users, 128).concat(list(r.tags, 70)).join(', ') + ' = ' + dataText(type, r);
    }).join('\n');
  }

  // recordRows turns GET /dns/state into one row per record.
  function recordRows(state) {
    state = hl.obj(state);
    return hl.arr(state.records).map(function (r) {
      r = hl.obj(r);
      var type = TYPES.indexOf(label(r.type, 8)) >= 0 ? label(r.type, 8) : 'A';
      var rules = hl.arr(r.rules).map(function (x) {
        x = hl.obj(x);
        return { who: list(x.users, 128).concat(list(x.tags, 70)), data: dataText(type, x), answer: list(x.answer, 300) };
      });
      return { name: label(r.name, 253), type: type, data: dataText(type, r), answer: list(r.answer, 300), rules: rules,
        rulesText: rulesText(type, r.rules), inFile: !!r.in_file, zone: label(r.zone, 253), note: label(r.note, 200) };
    });
  }

  // splitLines is the dns.nameservers.split block of the state, as YAML.
  function splitLines(state) {
    var split = hl.obj(hl.obj(state).split);
    var names = Object.keys(split).sort();
    if (!names.length) { return ''; }
    var out = ['dns:', '  nameservers:', '    split:'];
    names.forEach(function (n) {
      out.push('      ' + label(n, 253) + ':');
      hl.arr(split[n]).forEach(function (a) { out.push('        - ' + label(a, 45)); });
    });
    return out.join('\n');
  }

  function mount(els, deps) {
    deps = deps || {};
    var now = deps.now || function () { return Date.now(); };
    var st = { state: null, rows: [], loaded: false, failed: false, panel: null, confirm: null };
    var ctrl = {};

    function canAdmin() { return !!hl.obj(st.state).can_admin; }
    function setError(text) { els.error.textContent = text; els.error.hidden = !text; }
    function setNotice(text) { els.notice.textContent = text; els.notice.hidden = !text; }
    function kv(box, k, v, mono) {
      box.appendChild(hl.el('dt', null, k));
      var dd = hl.el('dd', mono ? 'mono' : null);
      if (typeof v === 'string') { dd.textContent = v; } else { dd.appendChild(v); }
      box.appendChild(dd);
    }

    // ----- server, health and configuration

    function renderServer() {
      var s = hl.obj(st.state);
      hl.clear(els.server);
      kv(els.server, 'Server', label(s.server, 60) || 'unknown', true);
      kv(els.server, 'Listening', hl.status(s.listening ? 'ok' : 'warn', s.listening ? 'Yes' : 'No'));
      kv(els.server, 'MagicDNS domain', label(s.base_domain, 253) || 'unknown', true);
      if (s.serve_zone) { kv(els.server, 'Serve zone', label(s.serve_zone, 253) + ' (on the Serve page)', true); }
      var extra = list(s.extra_records, 253);
      kv(els.server, 'Other extra records', extra.length ? extra.join(', ') : 'none', true);

      hl.clear(els.health);
      var err = label(s.error, 300), ferr = label(s.extra_records_file_error, 300);
      if (err || ferr) {
        els.health.appendChild(hl.status('warn', 'DNS answers are not refreshed'));
        els.health.appendChild(hl.el('p', 't2 small', 'The last refresh failed: ' + (err || ferr) + '. Names keep their last answers.'));
        els.health.hidden = false;
      } else { els.health.hidden = true; }

      hl.clear(els.config);
      var file = label(s.extra_records_file, 300);
      if (file) {
        var written = s.extra_records_file_written ? 'written ' + hl.relTime(String(s.extra_records_file_written), now()) : 'not written yet';
        kv(els.config, 'Extra-records file', file + ', ' + written, true);
        kv(els.config, 'In the file', 'A and AAAA records without rules. headscale sends them to all devices.');
      } else {
        kv(els.config, 'Extra-records file', 'Not set (TAILNET_DNS_EXTRA_RECORDS_FILE). Devices need the split lines below.');
      }
      var yaml = splitLines(s);
      var pre = hl.el('pre', 'code', yaml || 'No split lines are necessary.');
      kv(els.config, 'nameservers.split', pre);

      var n = st.rows.length, z = hl.arr(s.zones).length;
      var text = n + (n === 1 ? ' record, ' : ' records, ') + z + (z === 1 ? ' zone.' : ' zones.');
      if (s.refreshed) { text += ' Refreshed ' + hl.relTime(String(s.refreshed), now()) + '.'; }
      if (st.failed) { text += ' The last update failed; this is the last known state.'; }
      els.sub.textContent = text;
    }

    // ----- zones

    function renderZones() {
      var zones = hl.arr(hl.obj(st.state).zones).map(function (z) { z = hl.obj(z); return { name: label(z.name, 253), note: label(z.note, 200) }; });
      els.zoneswrap.hidden = !zones.length;
      els.zonesempty.hidden = zones.length > 0 || !st.loaded;
      var fk = hl.focusKey(els.zones);
      hl.clear(els.zones);
      zones.forEach(function (z) {
        var tr = hl.el('tr');
        tr.appendChild(hl.el('td', 'mono name', z.name));
        tr.appendChild(hl.el('td', 'small', String(st.rows.filter(function (r) { return r.zone === z.name; }).length)));
        tr.appendChild(hl.el('td', 't2 small', z.note));
        var act = hl.el('td', 'act');
        if (canAdmin()) {
          act.appendChild(keyed(hl.button('Delete', 'trash', 'sm quiet', function () { askDelete('zone', z.name); }), 'zone:' + z.name));
        }
        tr.appendChild(act);
        els.zones.appendChild(tr);
        confirmRow(els.zones, 'zone:' + z.name, ZONE_COLUMNS);
      });
      hl.refocus(els.zones, fk);
    }

    function confirmRow(body, id, cols) {
      if (!st.confirm || st.confirm.id !== id) { return; }
      var dr = hl.el('tr');
      var cell = hl.el('td', 'detail');
      cell.setAttribute('colspan', String(cols));
      cell.appendChild(st.confirm.node);
      dr.appendChild(cell);
      body.appendChild(dr);
    }

    // ----- records

    function lines(td, items, cls) {
      items.forEach(function (x) { td.appendChild(hl.el('div', cls, x)); });
      if (!items.length) { td.appendChild(hl.el('span', 't3', 'no answer')); }
    }

    function renderRecords() {
      var any = st.rows.length > 0;
      els.tablewrap.hidden = !any;
      els.empty.hidden = any || !st.loaded;
      els.emptytext.textContent = canAdmin() ? 'No records. Add a record: a name, a type and its data.' : 'No records. An administrator adds records here.';
      var fk = hl.focusKey(els.rows);
      hl.clear(els.rows);
      st.rows.forEach(function (r) {
        var tr = hl.el('tr');
        var name = hl.el('td', 'mono name', r.name);
        if (r.note) { name.appendChild(hl.el('div', 't3 small', r.note)); }
        tr.appendChild(name);
        tr.appendChild(hl.el('td', 'mono', r.type));
        tr.appendChild(hl.el('td', 'mono small', r.data || 'only rules'));
        var ans = hl.el('td', 'mono small');
        lines(ans, r.answer);
        tr.appendChild(ans);
        var rules = hl.el('td', 'small');
        r.rules.forEach(function (x, i) {
          var d = hl.el('div');
          d.appendChild(hl.el('span', 't2', (i + 1) + '. ' + x.who.join(', ') + ': '));
          d.appendChild(hl.el('span', 'mono', x.answer.length ? x.answer.join(', ') : 'no answer'));
          rules.appendChild(d);
        });
        if (!r.rules.length) { rules.appendChild(hl.el('span', 't3', 'everyone')); }
        tr.appendChild(rules);
        var by = hl.el('td', 'small');
        by.appendChild(r.inFile ? hl.status('ok', 'headscale file') : hl.status('off', 'node-control'));
        if (r.zone) { by.appendChild(hl.el('div', 't3 small', 'zone ' + r.zone)); }
        tr.appendChild(by);
        var act = hl.el('td', 'act');
        if (canAdmin()) {
          var box = hl.el('div', 'rowbtns');
          box.appendChild(keyed(hl.button('Edit', 'edit', 'sm quiet', function () { openPanel(r); }), 'edit:' + r.name));
          box.appendChild(keyed(hl.button('Delete', 'trash', 'sm quiet', function () { askDelete('record', r.name); }), 'del:' + r.name));
          act.appendChild(box);
        }
        tr.appendChild(act);
        els.rows.appendChild(tr);
        confirmRow(els.rows, 'record:' + r.name, REC_COLUMNS);
      });
      hl.refocus(els.rows, fk);
    }

    function render() {
      hl.clear(els.addslot);
      if (canAdmin()) {
        els.addslot.appendChild(keyed(hl.button('Add zone', 'plus', '', function () { openZonePanel(); }), 'addzone'));
        els.addslot.appendChild(keyed(hl.button('Add record', 'plus', 'primary', function () { openPanel(null); }), 'add'));
      }
      renderServer();
      renderZones();
      renderRecords();
    }

    // ----- actions

    function done(text) { setError(''); setNotice(text); return ctrl.reload(); }
    function failed(what) { return function (e) { setError(what + ' failed: ' + label(e && e.message, 200)); }; }

    function askDelete(kind, name) {
      var path = kind === 'zone' ? '/dns/zones/' : '/dns/records/';
      var node = hl.confirmPanel({
        title: 'Delete the ' + kind + ' ' + name + '?', object: name,
        consequence: kind === 'zone'
          ? 'node-control stops answering the names in the zone that have no record. Its records stay; remove the split line of the zone from headscale.'
          : 'node-control stops answering the name, and it leaves the extra-records file at the next refresh.',
        confirmLabel: 'Delete ' + kind, icon: 'trash',
        onCancel: function () { st.confirm = null; render(); },
        onConfirm: function () {
          st.confirm = null;
          hl.api('DELETE', path + encodeURIComponent(name)).then(function () { return done('Deleted ' + name + '.'); }, failed('Deleting ' + name));
        }
      });
      st.confirm = { id: kind + ':' + name, node: node };
      render();
    }

    function closePanel() {
      if (!st.panel) { return; }
      st.panel.root.remove();
      st.panel = null;
    }

    function input(ph) { var i = hl.el('input', 'input mono'); i.setAttribute('autocomplete', 'off'); if (ph) { i.setAttribute('placeholder', ph); } return i; }
    function field(text, ctl) { var l = hl.el('label', 'field grow', text); l.appendChild(ctl); return l; }

    function form(title, intro, submitLabel, build, send) {
      closePanel();
      var p = { saving: false };
      p.root = hl.el('form', 'panel');
      p.root.setAttribute('novalidate', '');
      p.root.setAttribute('aria-label', title);
      p.root.appendChild(hl.el('h2', null, title));
      p.root.appendChild(hl.el('p', 't2', intro));
      var box = hl.el('div', 'stack');
      var first = build(box);
      var err = hl.el('div', 'note crit');
      err.hidden = true;
      box.appendChild(err);
      var btns = hl.el('div', 'rowbtns start');
      var submit = hl.el('button', 'btn primary', submitLabel);
      submit.setAttribute('type', 'submit');
      btns.appendChild(submit);
      btns.appendChild(hl.button('Cancel', null, 'quiet', closePanel));
      box.appendChild(btns);
      p.root.appendChild(box);
      p.root.addEventListener('keydown', function (ev) { if (ev && ev.key === 'Escape') { closePanel(); } });
      p.root.addEventListener('submit', function (ev) {
        ev.preventDefault();
        if (p.saving) { return; }
        var req = send();
        if (req.error) { err.textContent = req.error; err.hidden = false; return; }
        err.hidden = true;
        p.saving = true;
        submit.disabled = true;
        hl.api(req.method, req.url, req.body).then(function () {
          closePanel();
          return done(req.ok);
        }, function (e) {
          p.saving = false;
          submit.disabled = false;
          err.textContent = label(e && e.message, 300);
          err.hidden = false;
        });
      });
      els.panel.appendChild(p.root);
      st.panel = p;
      if (first) { first.focus(); }
      return p;
    }

    function openZonePanel() {
      var name, note;
      form('Add a zone', 'node-control answers every name in the zone. Add the split line of the zone to headscale.', 'Add zone', function (box) {
        name = input('corp.example.com'); note = input();
        var row = hl.el('div', 'row');
        row.appendChild(field('Zone', name));
        row.appendChild(field('Note', note));
        box.appendChild(row);
        return name;
      }, function () {
        var n = name.value.trim().toLowerCase();
        if (!n) { return { error: 'Enter a zone name.' }; }
        return { method: 'POST', url: '/dns/zones', body: { name: n, note: note.value.trim() }, ok: 'Added the zone ' + n + '.' };
      });
    }

    // openPanel adds a record (r null) or replaces one.
    function openPanel(r) {
      var name, type, data, rules, note;
      var edit = !!r;
      form(edit ? 'Edit ' + r.name : 'Add a record',
        'A: addresses, node:<name> or tag:<name> (the online nodes with the tag). CNAME: a name. TXT: text. SRV: priority weight port target. ' +
        'Rules, one per line: <logins and tags> = <data>. The first rule that matches the client answers; an empty data hides the name from them.',
        edit ? 'Save' : 'Add record', function (box) {
          name = input('app.example.com'); data = input(); note = input();
          type = hl.el('select', 'input');
          TYPES.forEach(function (t) { var o = hl.el('option', null, t); o.value = t; type.appendChild(o); });
          rules = hl.el('textarea', 'input mono');
          rules.setAttribute('rows', '3');
          rules.setAttribute('placeholder', 'tag:dev, user@example.com = 100.64.0.20');
          if (edit) { name.value = r.name; name.disabled = true; type.value = r.type; data.value = r.data; rules.value = r.rulesText; note.value = r.note; }
          var r1 = hl.el('div', 'row');
          r1.appendChild(field('Name', name));
          r1.appendChild(field('Type', type));
          box.appendChild(r1);
          box.appendChild(field('Data', data));
          box.appendChild(field('Rules', rules));
          box.appendChild(field('Note', note));
          return edit ? data : name;
        }, function () {
          var n = name.value.trim().toLowerCase(), t = type.value || 'A';
          if (!n) { return { error: 'Enter a name.' }; }
          var d = parseData(t, data.value);
          if (d.error) { return { error: d.error }; }
          var parsed = parseRules(t, rules.value);
          if (parsed.error) { return { error: parsed.error }; }
          var body = { type: t, note: note.value.trim() };
          Object.keys(d).forEach(function (k) { body[k] = d[k]; });
          if (parsed.rules.length) { body.rules = parsed.rules; }
          if (!parsed.rules.length && !Object.keys(d).length) { return { error: 'Enter the data, or add a rule.' }; }
          if (edit) { return { method: 'PUT', url: '/dns/records/' + encodeURIComponent(n), body: body, ok: 'Saved ' + n + '.' }; }
          body.name = n;
          return { method: 'POST', url: '/dns/records', body: body, ok: 'Added ' + n + '.' };
        });
    }

    // ----- control surface

    ctrl.update = function (d) {
      st.state = hl.obj(d);
      st.rows = recordRows(st.state);
      st.loaded = true;
      if (!canAdmin()) { closePanel(); st.confirm = null; }
      render();
    };
    ctrl.setFailed = function (f) { st.failed = !!f; render(); };
    ctrl.reload = function () {
      return hl.api('GET', '/dns/state').then(function (j) {
        ctrl.update(j);
        ctrl.setFailed(false);
      }, function () { ctrl.setFailed(true); });
    };
    ctrl.openPanel = openPanel;
    ctrl.openZonePanel = openZonePanel;
    ctrl.state = st;
    render();
    return ctrl;
  }

  function init() {
    var root = document.getElementById('dns');
    if (!root) { return; }
    var $ = function (id) { return document.getElementById(id); };
    var ctrl = mount({
      sub: $('sub'), error: $('error'), notice: $('notice'), health: $('health'), server: $('server'), panel: $('panel'),
      zones: $('zones'), zoneswrap: $('zoneswrap'), zonesempty: $('zonesempty'), empty: $('empty'), emptytext: $('emptytext'),
      tablewrap: $('tablewrap'), rows: $('rows'), config: $('config'), addslot: $('addslot')
    });
    var poller = hl.poll(ctrl.reload, POLL_MS);
    document.addEventListener('visibilitychange', function () { if (!document.hidden) { poller.now(); } });
  }

  hl.dns = { parseData: parseData, parseRules: parseRules, dataText: dataText, recordRows: recordRows, splitLines: splitLines, mount: mount, init: init };
  init();
})();
