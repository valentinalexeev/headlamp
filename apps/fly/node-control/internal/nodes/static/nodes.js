// headlamp console: the Nodes page. Needs /_ui/hl.js first (the shell loads it). Everything that came
// from the network goes in through textContent only; markup is never built from strings.
//
// One table lists three kinds of thing: nodes (nodes.json, /status, /tailnet/status), bridges
// (/enroll/ingresses) and task runners (/sessions.json). mergeNodes joins them into one row per machine.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var POLL_MS = 8000;
  var BUSY_MS = 2000;
  var ROLES = ['Exit', 'Bridge', 'Runner'];
  var RANK = { crit: 0, warn: 1, ok: 2, off: 3 };
  var RUNNING = { RUNNING: 1, ACTIVE: 1, STARTED: 1 };
  var STOPPED = { STOPPED: 1 };
  var SYNC_REASON = {
    unreachable: 'Agent unreachable', unauthorized: 'Agent refused the credentials',
    rejected: 'Agent rejected the credentials', conflict: 'Agent reports a conflict', error: 'Sync ended in an error'
  };
  var CONSEQUENCE = {
    Exit: 'Clients using it as an exit node lose that route until you start it again.',
    Bridge: 'Profiles that connect through it stop working until you start it again.',
    Runner: 'Tasks and sessions on it stop until you start it again.'
  };

  function arr(v) { return Array.isArray(v) ? v : []; }
  function has(o, k) { return Object.prototype.hasOwnProperty.call(o, k); }
  function obj(v) { return v && typeof v === 'object' && !Array.isArray(v) ? v : {}; }
  function str(v) { return typeof v === 'string' ? v : ''; }
  function up(v) { return str(v).toUpperCase(); }
  function pick(map, k) {
    return map && typeof map === 'object' && typeof k === 'string' && has(map, k) && map[k] && typeof map[k] === 'object' ? map[k] : null;
  }
  function clear(node) { while (node.firstChild) { node.removeChild(node.firstChild); } }

  // sentence turns a provider word ("STOPPING", "not_ready") into "Stopping", "Not ready".
  function sentence(s) {
    var t = hl.plain(s, 40).replace(/_/g, ' ').toLowerCase();
    return t ? t.charAt(0).toUpperCase() + t.slice(1) : '';
  }

  // formatDetail: the detail line a node's config asks for. detailFields is an ordered list of field
  // names or {field, template}; the first field with a value wins.
  function formatDetail(cfg, st) {
    var fields = arr(cfg.detailFields);
    for (var i = 0; i < fields.length; i++) {
      var f = fields[i];
      var field = typeof f === 'string' ? f : str(obj(f).field);
      var v = field && has(st, field) ? st[field] : '';
      if (!v) { continue; }
      v = hl.plain(v, 120);
      return typeof f === 'string' ? v : str(f.template).replace('{value}', v);
    }
    return hl.plain(st.detail, 200);
  }

  // kindOf: what the provider says, in the page's words.
  function kindOf(cfg, st) {
    var raw = str(st.status);
    var u = up(raw);
    if (u === '' || u === 'UNKNOWN') { return 'unknown'; }
    if (u === 'UNMANAGED') { return 'unmanaged'; }
    if (u === 'NOT_CONFIGURED') { return 'notconfigured'; }
    if (u === 'ERROR') { return 'error'; }
    if (u === up(cfg.startStatus) || has(RUNNING, u)) { return 'running'; }
    if (u === up(cfg.stopStatus) || has(STOPPED, u)) { return 'stopped'; }
    return 'transition';
  }

  // tailnetUsable: /tailnet/status answered with its map of names. A failed read, or the not-configured
  // body, means the tailnet is unknown.
  function tailnetUsable(t) {
    return !!t && typeof t === 'object' && !Array.isArray(t) && t.status !== 'not_configured';
  }

  function unixIso(sec) {
    if (typeof sec !== 'number' || !isFinite(sec)) { return ''; }
    var d = new Date(sec * 1000);
    return isNaN(d.getTime()) ? '' : d.toISOString();
  }

  function routeWord(n) { return n + (n === 1 ? ' route' : ' routes'); }

  // buildRow: one machine. s: { id, cfg, st, tn, ing, run }; cfg is null for a machine that is not in
  // nodes.json. Precedence of the state: out of sync, then running but offline, then an approval that
  // waits, then what the provider and the tailnet say.
  function buildRow(s, now) {
    var managed = !!s.cfg;
    var cfg = s.cfg || {};
    var st = s.st || {};
    var tn = s.tn;
    var ing = s.ing;
    var run = s.run;
    var kind = managed ? kindOf(cfg, st) : 'none';
    var role = ing ? 'Bridge' : (run ? 'Runner' : 'Exit');
    var online = null;
    if (tn) { online = !!tn.online; }
    else if (ing && typeof ing.online === 'boolean') { online = ing.online; }
    else if (!managed && run) { online = !!run.available; }
    var lastSeen = tn && tn.last_seen ? hl.relTime(tn.last_seen, now) : '';
    var canPower = managed && kind !== 'unmanaged' && kind !== 'notconfigured';
    var sync = ing && ing.sync && typeof ing.sync === 'object' ? ing.sync : null;
    var appr = ing ? str(ing.approval) : '';
    var approve = appr === 'pending' || appr === 'changed';
    // an approved discovered bridge: its approval can be reviewed or removed on the approval page
    var manage = !!ing && str(ing.source) === 'discovered' && ing.approved === true && appr === 'approved';
    var routes = tn ? (Number(tn.available_route_count) || 0) : 0;

    function runningDetail() {
      var parts = [];
      if (sync && sync.state === 'ok') { parts.push('In sync'); }
      if (run && run.session && run.session.state === 'running') { parts.push('1 session'); }
      if (routes > 0) { parts.push(routeWord(routes)); }
      if (parts.length) { return parts.join(', '); }
      return managed ? formatDetail(cfg, st) : '';
    }

    var level, state, detail = '', explain = '';
    if (sync && sync.pending) {
      var since = unixIso(sync.since);
      var failed = since ? hl.relTime(since, now) : '';
      level = 'crit'; state = 'Out of sync';
      detail = (SYNC_REASON[sync.state] || 'Last sync did not succeed') + (failed ? ', first failed ' + failed : '');
      explain = 'Revocations and extensions made ' + (failed ? 'since ' + failed.replace(/ ago$/, '') + ' ago ' : 'recently ') +
        'are not in effect on this bridge.';
    } else if (kind === 'running' && online === false) {
      level = 'crit'; state = 'Offline on tailnet';
      detail = 'Provider reports running' + (lastSeen ? ', last seen ' + lastSeen : '');
      explain = 'The machine is up but has dropped off the tailnet, so clients cannot use it. Restarting it usually restores the connection.';
    } else if (appr === 'pending') {
      level = 'warn'; state = 'Waiting for approval';
      detail = 'Discovered on the tailnet';
      explain = 'It gets no credentials and appears in no profile until approved.';
    } else if (appr === 'changed') {
      level = 'crit'; state = 'Identity changed';
      detail = 'Differs from what you approved';
      explain = 'Its identity differs from what you approved, so it gets no credentials. Compare them before you decide.';
    } else if (appr === 'invalid') {
      level = 'warn'; state = 'Invalid parameters';
      detail = hl.plain(ing.detail, 200);
    } else if (kind === 'running') {
      level = 'ok'; state = 'Running'; detail = runningDetail();
    } else if (kind === 'stopped') {
      level = 'off'; state = 'Stopped'; detail = lastSeen ? 'Last seen ' + lastSeen : '';
    } else if (kind === 'transition') {
      level = 'ok'; state = sentence(st.status); detail = formatDetail(cfg, st);
    } else if (kind === 'error') {
      level = 'warn'; state = 'Provider error'; detail = hl.plain(st.detail, 200);
    } else if (kind === 'notconfigured') {
      level = 'off'; state = 'Not configured'; detail = hl.plain(st.detail, 200);
    } else if (online === true) {
      level = 'ok'; state = 'Online'; detail = runningDetail();
    } else if (online === false) {
      level = 'off'; state = 'Offline'; detail = lastSeen ? 'Last seen ' + lastSeen : '';
    } else {
      level = 'off'; state = 'Unknown'; detail = ing ? hl.plain(ing.detail, 200) : '';
    }

    var cloud = 'Not managed here';
    if (managed) {
      cloud = { running: 'Running', stopped: 'Stopped', error: 'Error', notconfigured: 'Not configured', unknown: 'Unknown' }[kind] ||
        (kind === 'transition' ? sentence(st.status) : 'Not managed here');
    }
    var tailnet = 'Unknown';
    if (tn) {
      if (tn.online) { tailnet = 'Online'; }
      else if (kind === 'stopped') { tailnet = 'Offline, as expected'; }
      else { tailnet = 'Offline, ' + (lastSeen ? 'last seen ' + lastSeen : 'never seen'); }
    } else if (online === true) { tailnet = 'Online'; }
    else if (online === false) { tailnet = 'Offline'; }
    var addr = (tn && arr(tn.ip_addresses)[0]) || (ing && ing.address) || '';
    var action = '';
    if (approve) { action = 'Compare'; }
    else if (level === 'crit' || level === 'warn') { action = 'Open'; }
    else if (kind === 'stopped' && canPower) { action = 'Start'; }

    return {
      id: s.id, label: hl.plain(s.id, 100), role: role, level: level, state: state, detail: detail,
      provider: managed ? hl.plain(cfg.provider, 60) : 'unmanaged',
      canPower: canPower, approve: approve, manage: manage, action: action, kind: kind,
      cloud: cloud, tailnet: tailnet, routes: tn && role === 'Exit' ? (Number(tn.approved_route_count) || 0) + ' of ' + routes + ' approved' : 'n/a',
      addr: hl.plain(addr, 60) || 'Not known', explain: explain,
      bridge: ing ? hl.plain(ing.name, 100) : '',
      reached: { start: kind === 'running' || kind === 'notconfigured' || kind === 'unmanaged', stop: kind === 'stopped' || kind === 'notconfigured' || kind === 'unmanaged' }
    };
  }

  function byName(a, b) {
    if (RANK[a.level] !== RANK[b.level]) { return RANK[a.level] - RANK[b.level]; }
    var x = a.label.toLowerCase(), y = b.label.toLowerCase();
    return x < y ? -1 : (x > y ? 1 : 0);
  }

  // mergeNodes: nodes is the "nodes" object of /nodes.json, status is /status, tailnet is /tailnet/status
  // (null when it could not be read), ingresses the list from /enroll/ingresses (null when enrolment is
  // off) and sessions the "nodes" list of /sessions.json (null when there are no runners). A bridge joins
  // the node whose tailnet name its given_name or name equals; one without such a node is a row of its
  // own that cannot be powered. opts.now is for tests.
  function mergeNodes(nodes, status, tailnet, ingresses, sessions, opts) {
    var now = opts && typeof opts.now === 'number' ? opts.now : Date.now();
    nodes = obj(nodes); status = obj(status);
    var usable = tailnetUsable(tailnet);
    var ings = arr(ingresses).filter(function (i) { return i && typeof i === 'object'; });
    var runs = arr(sessions).filter(function (r) { return r && typeof r === 'object'; });
    var usedIng = [], usedRun = [];
    var rows = [];
    var taken = {};
    Object.keys(nodes).forEach(function (id) { taken[id] = true; });
    // uniq: a bridge or runner that shares an id with a node gets its own, so every row id is unique.
    function uniq(id, suffix) {
      var u = id;
      while (has(taken, u)) { u = u + ' (' + suffix + ')'; }
      taken[u] = true;
      return u;
    }

    Object.keys(nodes).forEach(function (id) {
      var cfg = obj(nodes[id]);
      var tname = str(cfg.tailnetName) || id;
      var ing = null, run = null;
      for (var i = 0; i < ings.length && !ing; i++) {
        if (!usedIng[i] && (ings[i].given_name === tname || ings[i].name === tname)) { usedIng[i] = true; ing = ings[i]; }
      }
      for (var j = 0; j < runs.length && !run; j++) {
        if (!usedRun[j] && (runs[j].id === tname || runs[j].name === tname || runs[j].id === id)) { usedRun[j] = true; run = runs[j]; }
      }
      rows.push(buildRow({ id: id, cfg: cfg, st: pick(status, id) || {}, tn: usable ? pick(tailnet, tname) : null, ing: ing, run: run }, now));
    });
    ings.forEach(function (ing, i) {
      if (!usedIng[i] && str(ing.name)) { rows.push(buildRow({ id: uniq(ing.name, 'bridge'), cfg: null, st: null, tn: null, ing: ing, run: null }, now)); }
    });
    runs.forEach(function (run, j) {
      var id = str(run.id) || str(run.name);
      if (!usedRun[j] && id) { rows.push(buildRow({ id: uniq(id, 'runner'), cfg: null, st: null, tn: null, ing: null, run: run }, now)); }
    });
    return rows.sort(byName);
  }

  function plainRole(r) { return ROLES.indexOf(r) >= 0 ? r : 'All'; }

  // summary: the sentence under the heading.
  function summary(rows) {
    if (!rows.length) { return ''; }
    var crit = 0, waiting = 0, look = 0, stopped = 0;
    rows.forEach(function (r) {
      if (r.level === 'crit') { crit++; }
      else if (r.approve) { waiting++; }
      else if (r.level === 'warn') { look++; }
      if (r.kind === 'stopped') { stopped++; }
    });
    var parts = [];
    if (crit) { parts.push(crit + ' impaired'); }
    if (waiting) { parts.push(waiting + ' waiting for approval'); }
    if (look) { parts.push(look + (look === 1 ? ' needs a look' : ' need a look')); }
    if (stopped) { parts.push(stopped + ' stopped on purpose'); }
    if (parts.length) { return parts.join(', '); }
    return rows.length === 1 ? '1 node, nothing needs you' : rows.length + ' nodes, nothing needs you';
  }

  function button(label, iconName, cls, onClick) {
    var b = hl.el('button', 'btn' + (cls ? ' ' + cls : ''));
    b.setAttribute('type', 'button');
    var ic = iconName ? hl.icon(iconName) : null;
    if (ic) { b.appendChild(ic); }
    b.appendChild(hl.el('span', null, label));
    if (onClick) { b.addEventListener('click', onClick); }
    return b;
  }

  // mount wires the page's elements to the data. Nothing here fetches except the start and stop POSTs.
  // busy[id] is null when idle, or the action ('start'/'stop') whose request is still unresolved. While it is
  // set the node's buttons stay off: a start or stop returns at once but the machine takes a while to reach
  // the target state, so the POST response is no signal to click again. busy only clears when a fresh status
  // poll shows that state; a failed action or a failed poll leaves it set, so an error blocks repeat
  // clicks exactly like an unresolved status does.
  function mount(els) {
    var st = {
      rows: [], loaded: false, failed: false, error: '', busy: {}, confirming: null,
      role: plainRole(hl.queryParam('role')), sel: hl.queryParam('sel') || null,
      tableSig: null, panelSig: null, chipSig: null, chips: {}
    };
    var ctrl = { onAction: null };
    st.errorId = null;
    st.confirmEl = null;

    // focusKey names the focused control inside box by its data-fk, so a rebuild can give focus back.
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
    function keyed(el, key) { el.setAttribute('data-fk', key); return el; }
    // view: the row as shown. While a start or stop is unresolved the state reads Starting or Stopping.
    function view(r) {
      var b = st.busy[r.id];
      return b ? Object.assign({}, r, { level: 'ok', state: b === 'start' ? 'Starting…' : 'Stopping…' }) : r;
    }

    function effRole() {
      return st.role !== 'All' && st.rows.some(function (r) { return r.role === st.role; }) ? st.role : 'All';
    }
    function find(id) {
      for (var i = 0; i < st.rows.length; i++) { if (st.rows[i].id === id) { return st.rows[i]; } }
      return null;
    }

    function select(id) {
      st.sel = id;
      st.confirming = null;
      st.confirmEl = null;
      hl.setQueryParam('sel', id);
      render();
    }

    function setRole(r) {
      st.role = plainRole(r);
      hl.setQueryParam('role', st.role === 'All' ? null : st.role);
      render();
    }

    function setError(text) {
      st.error = text;
      if (!text) { st.errorId = null; }
      els.error.textContent = text;
      els.error.hidden = !text;
    }

    function act(id, action) {
      if (st.busy[id]) { return; }
      st.busy[id] = action;
      setError('');
      render();
      hl.api('POST', '/nodes/' + encodeURIComponent(id) + '/' + action).then(function () {
        setError('');
      }, function (err) {
        // busy stays set on purpose: see the comment on mount.
        setError(hl.plain(id, 60) + ' ' + action + ' failed: ' + hl.plain(err && err.message, 200));
        st.errorId = id;
      }).then(function () {
        if (typeof ctrl.onAction === 'function') { ctrl.onAction(); }
      });
    }

    function renderChips() {
      var roles = ROLES.filter(function (r) { return st.rows.some(function (x) { return x.role === r; }); });
      els.chips.hidden = st.rows.length === 0;
      var sig = roles.join(',');
      if (sig !== st.chipSig) {
        st.chipSig = sig;
        st.chips = {};
        clear(els.chips);
        ['All'].concat(roles).forEach(function (r) {
          var b = hl.el('button', 'chip', r);
          b.setAttribute('type', 'button');
          b.addEventListener('click', function () { setRole(r); });
          st.chips[r] = b;
          els.chips.appendChild(b);
        });
      }
      var cur = effRole();
      Object.keys(st.chips).forEach(function (r) {
        st.chips[r].className = 'chip' + (r === cur ? ' on' : '');
        st.chips[r].setAttribute('aria-pressed', r === cur ? 'true' : 'false');
      });
    }

    function renderTable() {
      var cur = effRole();
      var shown = st.rows.filter(function (r) { return cur === 'All' || r.role === cur; });
      var sig = JSON.stringify([shown.map(view), st.sel, st.busy]);
      if (sig === st.tableSig) { return; }
      st.tableSig = sig;
      var fk = focusKey(els.rows);
      clear(els.rows);
      shown.forEach(function (raw) {
        var r = view(raw);
        var tr = hl.el('tr', r.id === st.sel ? 'sel' : '');
        var name = hl.el('td', 'name');
        var a = keyed(hl.el('a', null, r.label), 'name:' + r.id);
        a.setAttribute('href', '/nodes?sel=' + encodeURIComponent(r.id));
        a.addEventListener('click', function (ev) { if (ev && ev.preventDefault) { ev.preventDefault(); } select(r.id); });
        name.appendChild(a);
        tr.appendChild(name);
        tr.appendChild(hl.el('td', 't2', r.role));
        var state = hl.el('td');
        state.appendChild(hl.status(r.level, r.state));
        tr.appendChild(state);
        tr.appendChild(hl.el('td', 't2', r.detail));
        tr.appendChild(hl.el('td', 'mono t3', r.provider));
        var actCell = hl.el('td', 'act');
        if (raw.action) {
          // Start starts the node (it is not destructive) under the same busy rule; Open and Compare select.
          var starts = raw.action === 'Start';
          var ab = keyed(button(raw.action, null, 'sm', function () { if (starts) { act(raw.id, 'start'); } else { select(raw.id); } }), 'act:' + r.id);
          ab.disabled = starts && !!st.busy[raw.id];
          actCell.appendChild(ab);
        }
        tr.appendChild(actCell);
        els.rows.appendChild(tr);
      });
      refocus(els.rows, fk);
    }

    function renderPanel(force) {
      var raw = st.sel ? find(st.sel) : null;
      // A confirmation that is open stays as it is until it is answered.
      if (!force && st.confirmEl && st.confirming === st.sel && raw) { return; }
      var row = raw ? view(raw) : null;
      var sig = JSON.stringify([row, st.busy[st.sel], st.confirming === st.sel]);
      if (!force && sig === st.panelSig) { return; }
      st.panelSig = sig;
      var panel = els.panel;
      var fk = focusKey(panel);
      st.confirmEl = null;
      clear(panel);
      if (!row) {
        panel.appendChild(hl.el('p', 't2', 'Select a node to see its detail.'));
        return;
      }
      var head = hl.el('div', 'panelhead');
      var title = hl.el('div');
      title.appendChild(hl.el('h2', null, row.label));
      title.appendChild(hl.status(row.level, row.state));
      head.appendChild(title);
      panel.appendChild(head);
      if (row.explain) {
        panel.appendChild(hl.el('div', row.level === 'crit' ? 'note crit' : (row.level === 'warn' ? 'note' : 'note plain'), row.explain));
      }
      var kv = hl.el('dl', 'kv');
      [['Provider', row.cloud, ''], ['Tailnet', row.tailnet, ''], ['Routes', row.routes, 'mono'], ['Address', row.addr, 'mono']].forEach(function (p) {
        kv.appendChild(hl.el('dt', null, p[0]));
        kv.appendChild(hl.el('dd', p[2], p[1]));
      });
      panel.appendChild(kv);

      var busy = st.busy[row.id] || null;
      if (row.canPower) {
        panel.appendChild(hl.el('hr', 'divider'));
        var box = hl.el('div', 'stack');
        panel.appendChild(box);
        if (st.confirming === row.id && !busy) {
          st.confirmEl = hl.confirmPanel({
            title: 'Stop ' + row.label + '?',
            consequence: CONSEQUENCE[row.role] || CONSEQUENCE.Exit,
            confirmLabel: 'Stop ' + row.label, cancelLabel: 'Keep running', icon: 'stop', into: box,
            onConfirm: function () { st.confirming = null; st.confirmEl = null; act(row.id, 'stop'); },
            onCancel: function () { st.confirming = null; st.confirmEl = null; renderPanel(true); refocus(els.panel, 'panel:stop'); }
          });
        } else {
          var btns = hl.el('div', 'rowbtns');
          // Only what makes sense: Start for a stopped node, Stop for a running one, neither while the
          // provider's state is unknown, in error or changing.
          if (row.kind === 'stopped') {
            var start = keyed(button('Start', 'play', 'primary', function () { act(row.id, 'start'); }), 'panel:start');
            start.disabled = !!busy;
            btns.appendChild(start);
          }
          if (row.kind === 'running') {
            var stop = keyed(button('Stop', 'stop', '', function () { st.confirming = row.id; renderPanel(true); }), 'panel:stop');
            stop.disabled = !!busy;
            btns.appendChild(stop);
          }
          box.appendChild(btns);
        }
      }
      if (row.approve) {
        panel.appendChild(hl.el('hr', 'divider'));
        var link = hl.el('a', 'btn primary', 'Compare and approve');
        link.setAttribute('href', '/enroll/approve?name=' + encodeURIComponent(row.bridge || row.id));
        var wrap = hl.el('div', 'rowbtns');
        wrap.appendChild(link);
        panel.appendChild(wrap);
      }
      if (row.manage) {
        panel.appendChild(hl.el('hr', 'divider'));
        var manageLink = hl.el('a', 'btn', 'Manage approval');
        manageLink.setAttribute('href', '/enroll/approve?name=' + encodeURIComponent(row.bridge || row.id));
        var mwrap = hl.el('div', 'rowbtns');
        mwrap.appendChild(manageLink);
        panel.appendChild(mwrap);
      }
      refocus(panel, fk);
    }

    function render() {
      var rows = st.rows;
      var text = summary(rows);
      if (st.failed) { text = st.loaded ? (text ? text + '. ' : '') + 'The data may be out of date.' : 'Could not load the nodes. Retrying.'; }
      els.sub.textContent = text;
      els.empty.hidden = !(st.loaded && rows.length === 0);
      els.cols.hidden = rows.length === 0;
      renderChips();
      renderTable();
      renderPanel(false);
    }

    // update takes one round of reads. A status that failed (statusFresh false) never settles busy.
    ctrl.update = function (d) {
      st.loaded = true;
      st.rows = mergeNodes(d.nodes, d.status, d.tailnet, d.ingresses, d.sessions, typeof d.now === 'number' ? { now: d.now } : undefined);
      if (d.statusFresh) {
        Object.keys(st.busy).forEach(function (id) {
          var row = find(id);
          if (!row || row.reached[st.busy[id]]) {
            delete st.busy[id];
            if (st.errorId === id) { setError(''); }
          }
        });
      }
      if (st.confirming && !find(st.confirming)) { st.confirming = null; st.confirmEl = null; }
      render();
    };
    ctrl.setFailed = function (f) { st.failed = !!f; render(); };
    ctrl.busyOf = function (id) { return st.busy[id] || null; };
    ctrl.isBusy = function () { return Object.keys(st.busy).length > 0; };
    // refresh is the way out of a busy state that never resolves: it forgets the waits and reads again.
    ctrl.refresh = function () {
      st.busy = {};
      setError('');
      render();
      if (typeof ctrl.onAction === 'function') { ctrl.onAction(); }
    };
    ctrl.select = select;
    ctrl.setRole = setRole;
    render();
    return ctrl;
  }

  // makeReader reads everything the page shows and hands it to ctrl. Enrolment off (404) or not allowed (403)
  // means there are no rows of that kind, and the page does not ask again for as long as it stays open; any
  // other failure keeps what was last read.
  function makeReader(api, ctrl) {
    var last = { nodes: null, status: {}, tailnet: null, ingresses: null, sessions: null };
    var skip = { ingresses: false, sessions: false };

    function gone(err) { return err && (err.status === 404 || err.status === 403); }

    function optional(key, url, field, pick) {
      if (skip[key]) { return Promise.resolve(); }
      return api('GET', url).then(function (v) { last[field] = arr(pick(obj(v))); }, function (e) {
        if (gone(e)) { last[field] = null; skip[key] = true; }
      });
    }

    function refresh() {
      var failed = false, fresh = false;
      var reads = [
        api('GET', '/nodes.json').then(function (v) { last.nodes = obj(obj(v).nodes); }, function () { failed = true; }),
        api('GET', '/status').then(function (v) { last.status = obj(v); fresh = true; }, function () { failed = true; }),
        api('GET', '/tailnet/status').then(function (v) { last.tailnet = v; }, function () { last.tailnet = null; }),
        optional('ingresses', '/enroll/ingresses', 'ingresses', function (v) { return v.ingresses; }),
        optional('sessions', '/sessions.json', 'sessions', function (v) { return v.nodes; })
      ];
      return Promise.all(reads).then(function () {
        if (last.nodes) {
          ctrl.update({ nodes: last.nodes, status: last.status, tailnet: last.tailnet, ingresses: last.ingresses, sessions: last.sessions, statusFresh: fresh });
        }
        ctrl.setFailed(failed);
      });
    }
    return { refresh: refresh };
  }

  function init() {
    var root = document.getElementById('nodes');
    if (!root) { return; }
    var $ = function (id) { return document.getElementById(id); };
    var ctrl = mount({
      sub: $('sub'), chips: $('chips'), error: $('error'), empty: $('empty'), cols: $('cols'), rows: $('rows'), panel: $('panel')
    });
    var refreshBtn = $('refresh');
    if (refreshBtn) { refreshBtn.addEventListener('click', function () { ctrl.refresh(); }); }
    var reader = makeReader(hl.api, ctrl);
    var refresh = reader.refresh;

    // A page opened in a background tab still loads once; the poll pauses while hidden and resumes at once
    // when the tab is shown.
    var poller = hl.poll(refresh, POLL_MS);
    if (document.hidden) { refresh(); }
    document.addEventListener('visibilitychange', function () { if (!document.hidden) { poller.now(); } });
    ctrl.onAction = function () { poller.now(); };
    // While a start or stop is unresolved, look again sooner.
    setInterval(function () { if (!document.hidden && ctrl.isBusy()) { poller.now(); } }, BUSY_MS);
  }

  hl.nodes = { mergeNodes: mergeNodes, summary: summary, plainRole: plainRole, mount: mount, makeReader: makeReader, init: init };
  init();
})();
