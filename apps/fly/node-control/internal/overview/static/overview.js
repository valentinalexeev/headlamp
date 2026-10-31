// headlamp console: the Overview page. Needs /_ui/hl.js first (the shell loads it). Everything that came
// from the network goes in through textContent only; markup is never built from strings.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var ATTENTION_URL = '/_attention.json';
  var ACTIVITY_URL = '/_activity.json?limit=6';
  var POLL_MS = 15000;
  var LAST_ISSUE_KEY = 'hl-last-issue';
  var COUNTED = { timed: 1, critical: 1, waiting: 1 };
  var GLYPH = { timed: 'warn', critical: 'crit', waiting: 'warn', info: 'ok' };
  var WORD = { timed: 'Waiting', critical: 'At risk', waiting: 'Waiting', info: 'Notice' };
  var LINE_STATUS = {
    ok: ['ok', 'Normal'], waiting: ['warn', 'Needs you'], critical: ['crit', 'At risk'],
    unknown: ['off', 'Unknown'], off: ['off', 'Off']
  };

  function arr(v) { return Array.isArray(v) ? v : []; }

  function localPath(p) {
    return typeof p === 'string' && p.charAt(0) === '/' && p.charAt(1) !== '/' && p.indexOf('\\') < 0;
  }

  function two(n) { return (n < 10 ? '0' : '') + n; }

  function countdown(deadlineIso, nowMs) {
    var t = Date.parse(deadlineIso);
    if (isNaN(t)) { return ''; }
    var s = Math.max(0, Math.round((t - nowMs) / 1000));
    return two(Math.floor(s / 60)) + ':' + two(s % 60);
  }

  // model: what the heading and the sentence under it say. opts: { lastIssue: ms or null, now: ms }.
  // kind is "attention", "unknown" or "clear". Unknown is never clear: a system that cannot be read
  // blocks "All clear", whatever else is true.
  function model(data, opts) {
    opts = opts || {};
    var items = arr(data && data.items);
    var systems = arr(data && data.systems);
    var hidden = !!(data && data.hidden_systems);
    var counted = items.filter(function (it) { return it && COUNTED[it.level]; }).length;
    var risk = items.filter(function (it) { return it && it.level === 'critical'; }).length;
    var unknown = systems.filter(function (s) { return s && s.state === 'unknown'; });
    var m = { kind: 'clear', heading: '', sub: '', counted: counted, risk: risk, unknown: unknown.length, hidden: hidden };

    if (counted > 0) {
      m.kind = 'attention';
      m.heading = counted === 1 ? '1 item needs you' : counted + ' items need you';
      if (risk > 0) { m.sub = (risk === 1 ? '1 is' : risk + ' are') + ' putting access at risk.'; }
      if (unknown.length > 0) {
        m.sub += (m.sub ? ' ' : '') + (unknown.length === 1 ? '1 system has an unknown status.' : unknown.length + ' systems have an unknown status.');
      }
      return m;
    }
    if (unknown.length > 0) {
      m.kind = 'unknown';
      m.heading = 'Status unknown';
      m.sub = unknown.length === 1
        ? 'The status of ' + hl.plain(unknown[0].name, 60) + ' could not be confirmed.'
        : unknown.length + ' systems could not be confirmed.';
      return m;
    }
    m.heading = hidden ? 'All clear in the systems you can see' : 'All clear';
    m.sub = 'Nothing needs you.';
    if (typeof opts.lastIssue === 'number' && isFinite(opts.lastIssue) && typeof opts.now === 'number') {
      var rel = hl.relTime(new Date(opts.lastIssue).toISOString(), opts.now);
      if (rel) { m.sub += ' Last issue seen ' + rel + '.'; }
    }
    return m;
  }

  // ageText: the short text at the right of a queue row.
  function ageText(it, nowMs) {
    if (it.deadline) {
      if (it.level === 'timed') { return 'reverts in ' + countdown(it.deadline, nowMs); }
      var rel = hl.relTime(it.deadline, nowMs);
      if (!rel) { return ''; }
      if (it.level === 'info') { return 'ends ' + rel; }
      return 'next: ' + rel.replace(/^in /, '');
    }
    return it.since ? hl.relTime(it.since, nowMs) : '';
  }

  // rowModel: one queue row. A row has at most one action: the link the module named, if it is local.
  function rowModel(it, nowMs) {
    var level = COUNTED[it.level] || it.level === 'info' ? it.level : 'info';
    var actions = [];
    if (it.action_label && localPath(it.action_path)) {
      actions.push({ label: hl.plain(it.action_label, 60), path: it.action_path, primary: level === 'timed' });
    }
    return {
      id: hl.plain(it.id, 100), level: level, glyph: GLYPH[level], word: WORD[level],
      title: hl.plain(it.title, 200), detail: hl.plain(it.detail, 500),
      age: ageText(it, nowMs), actions: actions
    };
  }

  // fmtWhen: "14:31" for today, "Yesterday", else "Oct 29". Local time.
  function fmtWhen(iso, nowMs) {
    var t = new Date(iso);
    if (isNaN(t.getTime())) { return ''; }
    var now = new Date(nowMs);
    var day = function (d) { return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime(); };
    var diff = Math.round((day(now) - day(t)) / 86400000);
    if (diff === 0) { return two(t.getHours()) + ':' + two(t.getMinutes()); }
    if (diff === 1) { return 'Yesterday'; }
    return ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'][t.getMonth()] + ' ' + t.getDate();
  }

  function clear(node) {
    while (node.firstChild) { node.removeChild(node.firstChild); }
  }

  function readLastIssue() {
    try {
      var v = Number(window.localStorage.getItem(LAST_ISSUE_KEY));
      return v > 0 && isFinite(v) ? v : null;
    } catch (e) { return null; }
  }

  function writeLastIssue(ms) {
    try { window.localStorage.setItem(LAST_ISSUE_KEY, String(ms)); } catch (e) { /* the page still works */ }
  }

  function init() {
    var root = document.getElementById('overview');
    if (!root) { return; }
    var $ = function (id) { return document.getElementById(id); };
    var els = {
      heading: $('heading'), sub: $('sub'), glyph: $('glyph'), queue: $('queue'), systems: $('systems'),
      activity: $('activity'), rows: $('activity-rows')
    };
    var st = { data: null, activity: [], failed: false, ages: [], sig: null };

    function renderQueue(items, now) {
      var sig = JSON.stringify(items.map(function (it) {
        return [it.id, it.level, it.title, it.detail, it.action_label, it.action_path];
      }));
      st.ages = [];
      if (sig !== st.sig) {
        st.sig = sig;
        clear(els.queue);
        els.queue.hidden = items.length === 0;
        items.forEach(function (it) {
          var r = rowModel(it, now);
          var row = hl.el('div', 'qrow');
          var g = hl.el('span', 'g ' + r.glyph);
          g.setAttribute('aria-hidden', 'true');
          row.appendChild(g);
          var body = hl.el('div', 'qbody');
          var title = hl.el('div', 'qtitle');
          title.appendChild(hl.el('span', 'sr', r.word + ': '));
          title.appendChild(document.createTextNode(r.title));
          body.appendChild(title);
          if (r.detail) { body.appendChild(hl.el('div', 't2 small', r.detail)); }
          row.appendChild(body);
          var age = hl.el('span', 'qage', r.age);
          row.appendChild(age);
          r.actions.forEach(function (a) {
            var link = hl.el('a', 'btn sm' + (a.primary ? ' primary' : ''), a.label);
            link.setAttribute('href', a.path);
            row.appendChild(link);
          });
          els.queue.appendChild(row);
          st.ages.push({ el: age, item: it });
        });
      } else {
        var kids = els.queue.children || [];
        items.forEach(function (it, i) {
          var age = kids[i] && kids[i].querySelector ? kids[i].querySelector('.qage') : null;
          if (age) { st.ages.push({ el: age, item: it }); }
        });
      }
      st.ages.forEach(function (a) { a.el.textContent = ageText(a.item, now); });
    }

    function renderSystems(systems, showStatus) {
      clear(els.systems);
      systems.forEach(function (s) {
        var tr = hl.el('tr');
        var name = hl.el('td', 'name');
        if (localPath(s.path)) {
          var a = hl.el('a', null, hl.plain(s.name, 100));
          a.setAttribute('href', s.path);
          name.appendChild(a);
        } else {
          name.textContent = hl.plain(s.name, 100);
        }
        tr.appendChild(name);
        if (showStatus) {
          var ls = LINE_STATUS[s.state] || LINE_STATUS.unknown;
          var td = hl.el('td');
          td.appendChild(hl.status(ls[0], ls[1]));
          tr.appendChild(td);
        }
        tr.appendChild(hl.el('td', 't2', hl.plain(s.summary, 300)));
        els.systems.appendChild(tr);
      });
    }

    function renderActivity(now) {
      var entries = st.activity;
      els.activity.hidden = entries.length === 0;
      clear(els.rows);
      entries.forEach(function (e) {
        var tr = hl.el('tr');
        tr.appendChild(hl.el('td', 'mono t3', fmtWhen(e.at, now)));
        var text = hl.plain(e.action, 100);
        if (e.detail) { text += (text ? ': ' : '') + hl.plain(e.detail, 200); }
        tr.appendChild(hl.el('td', null, text));
        els.rows.appendChild(tr);
      });
    }

    function render() {
      var now = Date.now();
      var data = st.data;
      if (!data) {
        if (st.failed) { els.sub.textContent = 'Could not load the status. Retrying.'; }
        return;
      }
      var items = arr(data.items);
      var last = readLastIssue();
      var m = model(data, { lastIssue: last, now: now });
      if (m.counted > 0) { writeLastIssue(now); }
      els.heading.textContent = m.heading;
      els.sub.textContent = m.sub + (st.failed ? ' The data may be out of date.' : '');
      els.glyph.hidden = m.kind !== 'clear';
      renderQueue(items, now);
      renderSystems(arr(data.systems).filter(function (s) { return s; }), m.kind !== 'clear');
      renderActivity(now);
    }

    function refresh() {
      var attention = hl.api('GET', ATTENTION_URL).then(function (data) {
        st.data = data || {};
        st.failed = false;
      }, function (err) {
        if (!(err && err.status === 404)) { st.failed = true; }
      });
      var activity = hl.api('GET', ACTIVITY_URL).then(function (data) {
        st.activity = arr(data && data.entries);
      }, function () { /* the section stays as it was */ });
      return Promise.all([attention, activity]).then(render);
    }

    // A page opened in a background tab still loads once; the poll pauses while hidden and resumes at once
    // when the tab is shown.
    var poller = hl.poll(refresh, POLL_MS);
    if (document.hidden) { refresh(); }
    document.addEventListener('visibilitychange', function () { if (!document.hidden) { poller.now(); } });
    setInterval(function () {
      if (document.hidden) { return; }
      var now = Date.now();
      st.ages.forEach(function (a) { a.el.textContent = ageText(a.item, now); });
    }, 1000);
  }

  hl.overview = { model: model, rowModel: rowModel, ageText: ageText, fmtWhen: fmtWhen, countdown: countdown, init: init };
  init();
})();
