// headlamp console: the Activity page. Needs /_ui/hl.js first (the shell loads it). Everything that came from the
// network goes in through textContent only; markup is never built from strings.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var URL_BASE = '/_activity.json';
  var POLL_MS = 30000;
  var LIMIT = 200;
  // Pages in the nav that are not a source of activity entries. Namespace and key changes are recorded under Funnel;
  // Sessions adds none.
  var NOT_SOURCES = ['Overview', 'Activity', 'Namespaces & keys', 'Sessions'];
  // The sentences are shared with the Overview page: /_ui/activity-text.js, loaded first.
  var describe = hl.activityText ? hl.activityText.describe : function (e) { return hl.plain(e && e.action, 100); };
  var MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

  function arr(v) { return Array.isArray(v) ? v : []; }
  function two(n) { return (n < 10 ? '0' : '') + n; }
  function clear(node) { while (node.firstChild) { node.removeChild(node.firstChild); } }

  // sourceOf: the filter named by the URL, or "All" for anything that is not one of the sources. Without a list
  // (the nav has not answered yet) a plain name is taken provisionally and checked once the list is known.
  function sourceOf(param, list) {
    if (typeof param !== 'string' || param === '' || param === 'All') { return 'All'; }
    if (!Array.isArray(list)) { return param.length <= 40 ? param : 'All'; }
    return list.indexOf(param) >= 0 ? param : 'All';
  }

  // sourcesFromNav: the entries of /_nav.json that can have activity, in nav order.
  function sourcesFromNav(nav) {
    var out = [];
    arr(nav).forEach(function (it) {
      var label = it && typeof it.label === 'string' ? hl.plain(it.label, 40) : '';
      if (label && NOT_SOURCES.indexOf(label) < 0 && out.indexOf(label) < 0) { out.push(label); }
    });
    return out;
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
    var st = { source: sourceOf(getParam('source'), opts.sources), sources: arr(opts.sources).slice(), seen: [],
      data: null, failed: false, navFailed: false, token: 0, chips: [], shown: '' };

    // names: the chips that would match something: the sources the nav lists, plus any source an entry has
    // come from, plus the one chosen now, unless the nav failed and so nothing vouches for it.
    function names() {
      var out = ['All'];
      st.sources.concat(st.seen).forEach(function (n) { if (out.indexOf(n) < 0) { out.push(n); } });
      if (out.indexOf(st.source) < 0 && !st.navFailed) { out.push(st.source); }
      return out;
    }

    function renderChips() {
      var list = names();
      if (list.join('\n') !== st.shown) {
        st.shown = list.join('\n');
        // A rebuild would drop focus from the chip that has it: remember which, and focus its replacement.
        var ae = typeof document !== 'undefined' ? document.activeElement : null;
        var focused = null;
        st.chips.forEach(function (c) { if (ae && c.el === ae) { focused = c.name; } });
        clear(els.chips);
        st.chips = list.map(function (name) {
          var b = hl.el('button', 'chip', name);
          b.setAttribute('type', 'button');
          b.addEventListener('click', function () { ctl.pick(name); });
          els.chips.appendChild(b);
          return { name: name, el: b };
        });
        st.chips.forEach(function (c) { if (c.name === focused) { c.el.focus(); } });
      }
      st.chips.forEach(function (c) {
        var on = c.name === st.source;
        c.el.className = on ? 'chip on' : 'chip';
        c.el.setAttribute('aria-pressed', on ? 'true' : 'false');
      });
    }

    function render() {
      var data = st.data;
      var entries = arr(data && data.entries).filter(function (e) { return e; });
      entries.forEach(function (e) {
        var src = hl.plain(e.source, 100);
        if (src && st.seen.indexOf(src) < 0) { st.seen.push(src); }
      });
      renderChips();
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

    // dropUnconfirmed: with no nav to check against, a source chosen by the URL stands only if an answer shows
    // entries from it. Returns the new read, or undefined when the choice stands or no answer has come yet.
    function dropUnconfirmed() {
      if (!st.navFailed || !st.data || st.source === 'All' || st.seen.indexOf(st.source) >= 0) { return undefined; }
      st.source = 'All';
      setParam('source', '');
      st.data = null;
      render();
      return ctl.refresh();
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
          return dropUnconfirmed();
        }, function () {
          if (token !== st.token) { return; }
          st.failed = true;
          render();
        });
      },
      // setSources gives the sources the nav lists. A source chosen by the URL that is not one of them is dropped.
      setSources: function (list) {
        st.sources = arr(list).slice();
        if (st.source !== 'All' && names().indexOf(st.source) >= 0 && st.sources.indexOf(st.source) < 0 && st.seen.indexOf(st.source) < 0) {
          st.source = 'All';
          setParam('source', '');
          st.data = null;
          render();
          return ctl.refresh();
        }
        render();
        return Promise.resolve();
      },
      // navFailed says the nav could not be read, so the chosen source cannot be checked against it.
      navFailed: function () {
        st.navFailed = true;
        render();
        return dropUnconfirmed() || Promise.resolve();
      },
      pick: function (name) {
        var next = sourceOf(name, names());
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
    // The chips follow the nav: a source the console does not have gets none. If the nav does not answer, the
    // chips come from the entries that arrive.
    hl.api('GET', '/_nav.json').then(function (nav) { return ctl.setSources(sourcesFromNav(nav)); }, function () { return ctl.navFailed(); });
    var poller = hl.poll(function () { return ctl.refresh(); }, POLL_MS);
    document.addEventListener('visibilitychange', function () { if (!document.hidden) { poller.now(); } });
  }

  hl.activity = { describe: describe, fmtWhen: fmtWhen, sourceOf: sourceOf, sourcesFromNav: sourcesFromNav, notices: notices, emptyText: emptyText, create: create, init: init };
  init();
})();
