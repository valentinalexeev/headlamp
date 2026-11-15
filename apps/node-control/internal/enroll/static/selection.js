// headlamp console: bridge selection (#314). Needs /_ui/hl.js first. Everything from the network goes in through
// textContent only; markup is never built from strings.
//
// The page edits the console override of the device profile selection: the urltest tuning, the order and group of
// each bridge, and a drain switch per bridge. "Preview" validates the draft on the server and shows the changes and the
// selection groups of the profile; "Apply" is possible only after a preview of the same draft. "Revert" goes back one
// step. Each value shows its source: console (ui), enroll.json (file) or the built-in default.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var SOURCE = { ui: 'console', file: 'enroll.json', 'default': 'default' };
  var STRATEGIES = ['', 'url-test', 'round-robin', 'consistent-hashing'];
  var TUNING = [
    { key: 'tolerance', label: 'Tolerance (ms, 0-1000)', type: 'number' },
    { key: 'interval', label: 'Interval (30s to 24h, for example 3m)', type: 'text' },
    { key: 'url', label: 'Test URL (http or https)', type: 'text' },
    { key: 'strategy', label: 'Strategy (Clash and mihomo only)', type: 'select' }
  ];

  function sourceText(s) { return SOURCE[s] || 'default'; }

  // draftOf: the editable draft from a state answer.
  function draftOf(state) {
    var o = hl.obj(state && state.override);
    var eff = hl.obj(state && state.effective);
    var d = {
      tolerance: typeof o.tolerance === 'number' ? String(o.tolerance) : '',
      interval: o.interval ? String(o.interval) : '', url: o.url ? String(o.url) : '',
      strategy: o.strategy ? String(o.strategy) : '',
      groups: {}, order: [], drain: hl.arr(o.drain).slice(), orderSet: hl.arr(o.order).length > 0
    };
    var g = hl.obj(o.groups);
    Object.keys(g).forEach(function (k) { d.groups[k] = String(g[k]); });
    hl.arr(eff.bridges).forEach(function (b) { if (b && b.name) { d.order.push(String(b.name)); } });
    return d;
  }

  // overrideOf: the request body from a draft. An empty field is not sent: enroll.json applies.
  function overrideOf(d) {
    var o = {};
    if (d.tolerance !== '') { o.tolerance = Number(d.tolerance); }
    if (d.interval !== '') { o.interval = d.interval; }
    if (d.url !== '') { o.url = d.url; }
    if (d.strategy !== '') { o.strategy = d.strategy; }
    if (Object.keys(d.groups).length) { o.groups = d.groups; }
    if (d.orderSet) { o.order = d.order.slice(); }
    if (d.drain.length) { o.drain = d.drain.slice(); }
    return o;
  }

  function mount(els) {
    var st = { state: null, draft: null, previewed: null, preview: null, error: '', busy: false };
    var ctrl = {};
    function setError(t) { st.error = t; els.error.textContent = t; els.error.hidden = !t; }
    function setNotice(t) { els.notice.textContent = t; els.notice.hidden = !t; }
    function key() { return JSON.stringify(overrideOf(st.draft)); }
    function changed() { st.previewed = null; st.preview = null; renderPreview(); renderActions(); }

    function renderTuning() {
      hl.clear(els.tuning);
      var eff = hl.obj(st.state && st.state.effective);
      TUNING.forEach(function (f) {
        var v = hl.obj(eff[f.key]);
        var lab = els.tuning.appendChild(hl.el('label', 'field', f.label));
        var input;
        if (f.type === 'select') {
          input = hl.el('select', 'input');
          STRATEGIES.forEach(function (s) {
            var opt = input.appendChild(hl.el('option', '', s === '' ? '(enroll.json)' : s));
            opt.value = s;
          });
        } else {
          input = hl.el('input', 'input');
          input.setAttribute('type', f.type);
          input.setAttribute('maxlength', '200');
        }
        input.setAttribute('data-key', f.key);
        input.value = st.draft[f.key];
        input.addEventListener(f.type === 'select' ? 'change' : 'input', function () { st.draft[f.key] = input.value; changed(); });
        lab.appendChild(input);
        els.tuning.appendChild(hl.el('p', 't2 small', 'In use: ' + hl.plain(String(v.value === undefined ? '' : v.value), 200) +
          ' (source: ' + sourceText(v.source) + ')'));
      });
    }

    function renderBridges() {
      hl.clear(els.bridges);
      var eff = hl.obj(st.state && st.state.effective);
      var byName = {};
      hl.arr(eff.bridges).forEach(function (b) { if (b) { byName[b.name] = b; } });
      st.draft.order.forEach(function (name, i) {
        var b = hl.obj(byName[name]);
        var row = els.bridges.appendChild(hl.el('div', 'row'));
        row.setAttribute('data-bridge', name);
        row.appendChild(hl.el('strong', '', hl.plain(name, 63)));
        var up = row.appendChild(hl.button('Up', null, 'sm', function () { move(i, -1); }));
        up.disabled = i === 0;
        var down = row.appendChild(hl.button('Down', null, 'sm', function () { move(i, 1); }));
        down.disabled = i === st.draft.order.length - 1;
        var gl = row.appendChild(hl.el('label', 'field', 'Group'));
        var g = gl.appendChild(hl.el('input', 'input'));
        g.setAttribute('maxlength', '32');
        g.setAttribute('data-group', name);
        g.value = Object.prototype.hasOwnProperty.call(st.draft.groups, name) ? st.draft.groups[name] : (b.file_group || '');
        g.addEventListener('input', function () {
          if (g.value === (b.file_group || '')) { delete st.draft.groups[name]; } else { st.draft.groups[name] = g.value; }
          changed();
        });
        row.appendChild(hl.el('span', 't2 small', 'source: ' + sourceText(b.group_source)));
        var dl = row.appendChild(hl.el('label', 'small'));
        var cb = dl.appendChild(hl.el('input'));
        cb.setAttribute('type', 'checkbox');
        cb.setAttribute('data-drain', name);
        cb.checked = st.draft.drain.indexOf(name) >= 0;
        cb.addEventListener('change', function () {
          st.draft.drain = st.draft.drain.filter(function (n) { return n !== name; });
          if (cb.checked) { st.draft.drain.push(name); }
          changed();
        });
        dl.appendChild(document.createTextNode(' Drain'));
      });
      els.bridges.appendChild(hl.el('p', 't2 small', 'Order source: ' + sourceText(st.draft.orderSet ? 'ui' : eff.order_source)));
    }

    function move(i, by) {
      var o = st.draft.order;
      var j = i + by;
      if (j < 0 || j >= o.length) { return; }
      var t = o[i]; o[i] = o[j]; o[j] = t;
      st.draft.orderSet = true;
      renderBridges();
      changed();
    }

    function renderPreview() {
      hl.clear(els.previewbody);
      els.preview.hidden = !st.preview;
      if (!st.preview) { return; }
      var p = st.preview;
      var ch = hl.arr(p.changes);
      els.previewbody.appendChild(hl.el('p', 't2', ch.length ? 'Changes:' : 'No change.'));
      var ul = els.previewbody.appendChild(hl.el('ul'));
      ch.forEach(function (c) { ul.appendChild(hl.el('li', 'small', hl.plain(String(c), 300))); });
      els.previewbody.appendChild(hl.el('p', 't2 small', 'sing-box check: ' + hl.plain(String(p.check || ''), 40)));
      els.previewbody.appendChild(hl.el('h3', '', 'sing-box groups'));
      els.previewbody.appendChild(hl.el('pre', 'code', JSON.stringify(hl.arr(p.singbox), null, 2)));
      els.previewbody.appendChild(hl.el('h3', '', 'Clash groups'));
      els.previewbody.appendChild(hl.el('pre', 'code', JSON.stringify(hl.arr(p.clash), null, 2)));
    }

    function renderActions() {
      hl.clear(els.actions);
      if (!st.state) { return; }
      var pv = els.actions.appendChild(hl.button('Preview', null, '', function () { ctrl.preview(); }));
      pv.disabled = st.busy;
      var ap = els.actions.appendChild(hl.button('Apply', 'check', 'primary', function () { ctrl.apply(); }));
      ap.disabled = st.busy || st.previewed === null || st.previewed !== key();
      var rv = els.actions.appendChild(hl.button('Revert last change', 'undo', '', function () { ctrl.revert(); }));
      rv.disabled = st.busy || !st.state.can_revert;
    }

    function render() {
      if (!st.state) { return; }
      var by = st.state.by ? 'Last change by ' + hl.plain(String(st.state.by), 100) : 'No console change yet.';
      els.sub.textContent = by + ' New profiles use these values at the next subscription refresh of each app.';
      renderTuning();
      renderBridges();
      renderPreview();
      renderActions();
    }

    function call(method, url, body) {
      setError(''); setNotice('');
      st.busy = true; renderActions();
      return hl.api(method, url, body).then(function (res) { st.busy = false; return res; }, function (e) {
        st.busy = false;
        var d = e && e.data && e.data.detail;
        var msg = Array.isArray(d) && d[0] && d[0].msg ? d[0].msg : (e && e.message);
        setError(hl.plain(String(msg || 'request failed'), 300));
        renderActions();
        throw e;
      });
    }

    ctrl.load = function () {
      return hl.api('GET', '/enroll/selection/state').then(function (res) {
        st.state = res; st.draft = draftOf(res); st.preview = null; st.previewed = null;
        render();
      }, function (e) { setError('Could not read the bridge selection: ' + hl.plain(e && e.message, 200)); });
    };
    ctrl.preview = function () {
      var k = key();
      return call('POST', '/enroll/selection/preview', { override: overrideOf(st.draft) }).then(function (res) {
        st.preview = res; st.previewed = k;
        renderPreview(); renderActions();
      }, function () {});
    };
    ctrl.apply = function () {
      if (st.previewed === null || st.previewed !== key()) { return Promise.resolve(); }
      return call('POST', '/enroll/selection/apply', { override: overrideOf(st.draft) }).then(function (res) {
        st.state = res; st.draft = draftOf(res); st.preview = null; st.previewed = null;
        render();
        setNotice('Applied. Each app gets the new profile at its next subscription refresh.');
      }, function () {});
    };
    ctrl.revert = function () {
      return call('POST', '/enroll/selection/revert', {}).then(function (res) {
        st.state = res; st.draft = draftOf(res); st.preview = null; st.previewed = null;
        render();
        setNotice('Reverted to the earlier values.');
      }, function () {});
    };
    ctrl.state = st;
    return ctrl;
  }

  function init() {
    var root = document.getElementById('selection');
    if (!root) { return; }
    var $ = function (id) { return document.getElementById(id); };
    mount({ sub: $('sub'), error: $('error'), notice: $('notice'), tuning: $('tuning'), bridges: $('bridges'),
      actions: $('actions'), preview: $('preview'), previewbody: $('previewbody') }).load();
  }

  hl.selection = { mount: mount, init: init, draftOf: draftOf, overrideOf: overrideOf };
  init();
})();
