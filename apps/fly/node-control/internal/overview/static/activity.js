// headlamp console: the Activity page. Needs /_ui/hl.js first (the shell loads it). Everything that came from the
// network goes in through textContent only; markup is never built from strings.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var URL_BASE = '/_activity.json';
  var POLL_MS = 30000;
  var LIMIT = 200;
  var SOURCES = ['All', 'Nodes', 'Devices', 'Funnel', 'Tasks', 'Policy'];
  // The sentences are shared with the Overview page: /_ui/activity-text.js, loaded first.
  var describe = hl.activityText ? hl.activityText.describe : function (e) { return hl.plain(e && e.action, 100); };
  var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

  function arr(v) { return Array.isArray(v) ? v : []; }
  function two(n) { return (n < 10 ? '0' : '') + n; }
  function clear(node) { while (node.firstChild) { node.removeChild(node.firstChild); } }

  // sourceOf: the filter named by the URL, or "All" for anything that is not one of the chips.
  function sourceOf(param) {
    return SOURCES.indexOf(param) > 0 ? param : 'All';
  }

  // fmtWhen: "Today 14:31", "Yesterday 18:40", else "Oct 29 16:22" (with the year when it is not this one). Local time.
  function fmtWhen(iso, nowMs) {
    var t = new Date(iso);
    if (isNaN(t.getTime())) { return ''; }
    var now = new Date(nowMs);
    var day = function (d) { return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime(); };
    var diff = Math.round((day(now) - day(t)) / 86400000);
    var clock = two(t.getHours()) + ':' + two(t.getMinutes());
    if (diff === 0) { return 'Today ' + clock; }
    if (diff === 1) { return 'Yesterday ' + clock; }
    var date = MONTHS[t.getMonth()] + ' ' + t.getDate();
    if (t.getFullYear() !== now.getFullYear()) { date += ' ' + t.getFullYear(); }
    return date + ' ' + clock;
  }

  // notices: the sentences under the title. A source that did not answer or is hidden never makes the page look
  // complete.
  function notices(data) {
    var out = [];
    if (data && data.partial) { out.push('Some sources did not answer, so this list may be incomplete.'); }
    if (data && data.hidden_systems) { out.push('Some systems are left out because you do not have access to them.'); }
    return out;
  }

  // emptyText: the head and the sentence shown when there is nothing to list.
  function emptyText(source) {
    if (source === 'All') { return ['No activity yet', 'Changes made through the console show here as they happen.']; }
    return ['No activity from ' + source, 'Nothing from ' + source + ' has been recorded yet. Choose All to see the rest.'];
  }

  // create drives the page. els: { chips, error, notice, empty, emptyhead, emptytext, table, rows }.
  // opts: { api, now, getParam, setParam } (all optional; the defaults are the real ones).
  function create(els, opts) {
    opts = opts || {};
    var api = opts.api || hl.api;
    var now = opts.now || function () { return Date.now(); };
    var getParam = opts.getParam || function (k) { return hl.queryParam(k); };
    var setParam = opts.setParam || function (k, v) { hl.setQueryParam(k, v); };
    var st = { source: sourceOf(getParam('source')), data: null, failed: false, token: 0, chips: [] };

    function renderChips() {
      if (!st.chips.length) {
        SOURCES.forEach(function (name) {
          var b = hl.el('button', 'chip', name);
          b.setAttribute('type', 'button');
          b.addEventListener('click', function () { ctl.pick(name); });
          els.chips.appendChild(b);
          st.chips.push({ name: name, el: b });
        });
      }
      st.chips.forEach(function (c) {
        var on = c.name === st.source;
        c.el.className = on ? 'chip on' : 'chip';
        c.el.setAttribute('aria-pressed', on ? 'true' : 'false');
      });
    }

    function render() {
      renderChips();
      var data = st.data;
      var entries = arr(data && data.entries).filter(function (e) { return e; });
      var notes = notices(data);
      els.notice.hidden = notes.length === 0;
      els.notice.textContent = notes.join(' ');
      els.error.hidden = !st.failed;
      els.error.textContent = st.failed
        ? (data ? 'Could not refresh the activity. Showing what was loaded before. Retrying.' : 'Could not load the activity. Retrying.')
        : '';
      var empty = !!data && entries.length === 0;
      els.empty.hidden = !empty;
      if (empty) {
        var t = emptyText(st.source);
        els.emptyhead.textContent = t[0];
        els.emptytext.textContent = t[1];
      }
      els.table.hidden = entries.length === 0;
      clear(els.rows);
      var at = now();
      entries.forEach(function (e) {
        var tr = hl.el('tr');
        tr.appendChild(hl.el('td', 'mono t3', fmtWhen(e.at, at)));
        tr.appendChild(hl.el('td', 't2', hl.plain(e.actor, 100) || '-'));
        tr.appendChild(hl.el('td', 't2', hl.plain(e.source, 100)));
        tr.appendChild(hl.el('td', null, describe(e)));
        els.rows.appendChild(tr);
      });
    }

    var ctl = {
      state: st,
      render: render,
      url: function () {
        return URL_BASE + '?limit=' + LIMIT + (st.source === 'All' ? '' : '&source=' + encodeURIComponent(st.source));
      },
      // refresh reads the entries of the chosen source. An answer for a source that is no longer chosen is dropped.
      refresh: function () {
        var token = ++st.token;
        return api('GET', ctl.url()).then(function (data) {
          if (token !== st.token) { return; }
          st.data = data || {};
          st.failed = false;
          render();
        }, function () {
          if (token !== st.token) { return; }
          st.failed = true;
          render();
        });
      },
      pick: function (name) {
        var next = sourceOf(name);
        if (next === st.source) { return Promise.resolve(); }
        st.source = next;
        st.data = null;
        setParam('source', next === 'All' ? '' : next);
        render();
        return ctl.refresh();
      }
    };
    return ctl;
  }

  function init() {
    var root = document.getElementById('activity-page');
    if (!root) { return; }
    var $ = function (id) { return document.getElementById(id); };
    var ctl = create({
      chips: $('chips'), error: $('error'), notice: $('notice'), empty: $('empty'), emptyhead: $('emptyhead'),
      emptytext: $('emptytext'), table: $('table'), rows: $('rows')
    });
    ctl.render();
    var poller = hl.poll(function () { return ctl.refresh(); }, POLL_MS);
    document.addEventListener('visibilitychange', function () { if (!document.hidden) { poller.now(); } });
  }

  hl.activity = { describe: describe, fmtWhen: fmtWhen, sourceOf: sourceOf, notices: notices, emptyText: emptyText, create: create, init: init };
  init();
})();
