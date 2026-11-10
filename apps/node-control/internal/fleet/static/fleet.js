// headlamp console: the Fleet page (#195). Needs /_ui/hl.js first (the shell loads it). Everything that came
// from the network goes in through textContent only; markup is never built from strings.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var POLL_MS = 30000;

  function text(v, n) { return typeof v === 'string' ? hl.plain(v, n || 80) : ''; }

  function row(cells) {
    var tr = hl.el('tr');
    cells.forEach(function (c) {
      var td = hl.el('td');
      if (c && c.nodeType) { td.appendChild(c); } else { td.textContent = c || ''; }
      tr.appendChild(td);
    });
    return tr;
  }

  // nodeState: the word and level of a tailscale client against the pin.
  function nodeState(n) {
    var online = n.online ? '' : ', offline';
    switch (n.status) {
      case 'current': return ['ok', 'current' + online];
      case 'older': return [hl.arr(n.tags).length ? 'crit' : 'warn', 'older than the pin' + online];
      case 'newer': return ['ok', 'newer than the pin' + online];
      default: return ['off', 'unknown' + online];
    }
  }

  // bridgeSync: the word and level of a bridge's sync and reachability.
  function bridgeSync(b) {
    if (!b.active) { return ['off', 'not active']; }
    if (b.online === false) { return ['crit', 'offline']; }
    if (b.sync === 'pending') { return ['crit', 'not in sync']; }
    if (b.sync === 'ok' || b.sync === 'empty') { return ['ok', 'in sync']; }
    return ['off', text(b.sync, 20) || 'unknown'];
  }

  function render(els, d, now) {
    d = hl.obj(d);
    var problems = hl.arr(d.problems), notes = hl.arr(d.notes);
    [[els.problems, problems], [els.notes, notes]].forEach(function (p) {
      hl.clear(p[0]);
      p[1].forEach(function (s) { p[0].appendChild(hl.el('p', null, text(s, 200))); });
      p[0].hidden = p[1].length === 0;
    });

    hl.clear(els.control);
    els.control.appendChild(row(['node-control', text(d.node_control), hl.status('ok', 'running')]));
    els.control.appendChild(row(['headscale', text(d.headscale) || '-',
      d.headscale_error ? hl.status('crit', text(d.headscale_error, 120)) : hl.status('ok', 'answers')]));
    if (d.funnel) {
      var f = hl.obj(d.funnel), fs;
      if (f.stale) { fs = hl.status('crit', 'no report since ' + hl.relTime(f.reported_at, now)); }
      else if (f.errors > 0) { fs = hl.status('warn', f.errors + ' errors in its report'); }
      else { fs = hl.status('ok', 'reported ' + hl.relTime(f.reported_at, now)); }
      els.control.appendChild(row(['funnel ingress', text(f.version) || 'unknown (before 0.16.0)', fs]));
    }

    var bridges = hl.arr(d.bridges);
    hl.clear(els.bridges);
    bridges.forEach(function (b) {
      b = hl.obj(b);
      var s = bridgeSync(b);
      els.bridges.appendChild(row([text(b.name, 63), text(b.agent) || 'unknown', text(b.xray) || 'unknown',
        hl.status(s[0], s[1]), b.info_at ? hl.relTime(b.info_at, now) : '-']));
    });
    els.bridgesEmpty.hidden = bridges.length > 0;
    els.bridgesTable.hidden = bridges.length === 0;

    els.pin.textContent = d.tailscale_pin ? 'The pin is ' + text(d.tailscale_pin) +
      '. Servers (tagged nodes) older than the pin are a problem; personal devices update themselves.' : '';
    hl.clear(els.nodes);
    hl.arr(d.nodes).forEach(function (n) {
      n = hl.obj(n);
      var s = nodeState(n);
      els.nodes.appendChild(row([text(n.name, 63), text(n.tailscale) || '-', hl.status(s[0], s[1]),
        hl.arr(n.tags).map(function (t) { return text(t, 40); }).join(' ')]));
    });
  }

  function create(els, opts) {
    opts = opts || {};
    var api = opts.api || hl.api;
    var now = opts.now || function () { return Date.now(); };
    function load() {
      return api('GET', '/fleet.json').then(function (d) {
        els.error.hidden = true;
        render(els, d, now());
      }, function (e) {
        els.error.textContent = 'The fleet report could not be loaded: ' + (e && e.message ? e.message : 'error');
        els.error.hidden = false;
      });
    }
    return { load: load };
  }

  window.hlFleet = { create: create, nodeState: nodeState, bridgeSync: bridgeSync };

  if (typeof document === 'undefined' || !document.getElementById('fleet-page')) { return; }
  var g = function (id) { return document.getElementById(id); };
  var page = create({ error: g('error'), problems: g('problems'), notes: g('notes'), control: g('control'),
    bridges: g('bridges'), bridgesEmpty: g('bridges-empty'), bridgesTable: g('bridges-table'), pin: g('pin'),
    nodes: g('nodes') });
  hl.poll(page.load, POLL_MS);
})();
