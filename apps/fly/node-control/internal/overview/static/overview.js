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
  var ACK_URL = '/_attention/acknowledge';
  var UNACK_URL = '/_attention/unacknowledge';
  var ACK_CHOICES = [
    { label: 'Until it changes', seconds: 0 }, { label: '1 day', seconds: 86400 }, { label: '7 days', seconds: 604800 }
  ];
  var LINE_STATUS = {
    ok: ['ok', 'Normal'], waiting: ['warn', 'Needs you'], critical: ['crit', 'At risk'],
    unknown: ['off', 'Unknown'], off: ['off', 'Off']
  };

  // The same sentences as the Activity page (/_ui/activity-text.js, loaded first).
  var describe = hl.activityText ? hl.activityText.describe : function (e) { return hl.plain(e && e.action, 100); };

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

  // splitItems: what asks for someone now, and what someone has acknowledged.
  function splitItems(items) {
    var out = { active: [], acked: [] };
    arr(items).forEach(function (it) {
      if (it) { (it.acknowledged ? out.acked : out.active).push(it); }
    });
    return out;
  }

  // ackedText: the line under an acknowledged item: what it is, who said they saw it, and when that ends.
  function ackedText(it, nowMs) {
    var a = it.acknowledged || {};
    var level = COUNTED[it.level] || it.level === 'info' ? it.level : 'info';
    var ends = a.until ? hl.relTime(a.until, nowMs) : '';
    return WORD[level] + '. Acknowledged by ' + hl.plain(a.by, 100) +
      (a.at ? ' ' + hl.relTime(a.at, nowMs) : '') + (ends ? ', ends ' + ends : ', until it changes') + '.';
  }

  // model: what the heading and the sentence under it say. opts: { lastIssue: ms or null, now: ms }.
  // kind is "attention", "unknown" or "clear". Unknown is never clear: a system that cannot be read
  // blocks "All clear", whatever else is true.
  function model(data, opts) {
    opts = opts || {};
    var split = splitItems(data && data.items);
    var items = split.active;
    var systems = arr(data && data.systems);
    var hidden = !!(data && data.hidden_systems);
    var counted = items.filter(function (it) { return it && COUNTED[it.level]; }).length;
    var risk = items.filter(function (it) { return it && it.level === 'critical'; }).length;
    var unknown = systems.filter(function (s) { return s && s.state === 'unknown'; });
    var m = { kind: 'clear', heading: '', sub: '', counted: counted, risk: risk, unknown: unknown.length, hidden: hidden, acked: split.acked.length };

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
      age: ageText(it, nowMs), actions: actions,
      ackToken: level !== 'timed' && typeof it.ack_token === 'string' ? it.ack_token : ''
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
      activity: $('activity'), rows: $('activity-rows'),
      acked: $('acked'), ackedSummary: $('acked-summary'), ackedRows: $('acked-rows'), ackError: $('ack-error')
    };
    var st = { data: null, activity: [], failed: false, ages: [], sig: null, ackedSig: null, ackedTexts: [], choosing: null, busy: false, ackError: '', refocus: null, focusEl: null };

    function actionButton(label, iconName, name) {
      var b = hl.el('button', 'btn sm');
      b.setAttribute('type', 'button');
      var ic = iconName ? hl.icon(iconName) : null;
      if (ic) { b.appendChild(ic); }
      b.appendChild(document.createTextNode(label));
      if (name) { b.setAttribute('aria-label', name); }
      b.disabled = st.busy;
      return b;
    }

    // send posts one acknowledge or undo. The item's token says what the operator was looking at; the answer, good or
    // bad, is followed by a fresh read, as the item may have moved on.
    function send(url, body) {
      st.busy = true;
      st.ackError = '';
      render();
      return hl.api('POST', url, body).then(function () {
        st.choosing = null;
      }, function (err) {
        st.ackError = err && err.message ? err.message : 'That did not work. Try again.';
      }).then(function () {
        st.busy = false;
        return refresh();
      });
    }

    function ackControls(row, it, r) {
      if (!r.ackToken) { return; }
      if (st.choosing !== it.id) {
        var open = actionButton('Acknowledge', 'check', 'Acknowledge: ' + r.title);
        open.addEventListener('click', function () {
          st.choosing = it.id;
          st.refocus = 'choice';
          render();
        });
        if (st.refocus === 'open:' + it.id) { st.refocus = null; st.focusEl = open; }
        row.appendChild(open);
        return;
      }
      var box = hl.el('div', 'ackchoices');
      box.setAttribute('role', 'group');
      box.setAttribute('aria-label', 'Acknowledge ' + r.title);
      ACK_CHOICES.forEach(function (c, i) {
        var b = actionButton(c.label, null, null);
        b.addEventListener('click', function () {
          var body = { id: it.id, ack_token: r.ackToken };
          if (c.seconds) { body.expires_in_seconds = c.seconds; }
          send(ACK_URL, body);
        });
        if (i === 0 && st.refocus === 'choice') { st.refocus = null; st.focusEl = b; }
        box.appendChild(b);
      });
      var cancel = actionButton('Cancel', null, null);
      cancel.className = 'btn sm quiet';
      cancel.addEventListener('click', function () {
        st.choosing = null;
        st.refocus = 'open:' + it.id;
        render();
      });
      box.appendChild(cancel);
      row.appendChild(box);
    }

    function renderQueue(items, now) {
      var sig = JSON.stringify([items.map(function (it) {
        return [it.id, it.level, it.title, it.detail, it.action_label, it.action_path, it.ack_token];
      }), st.choosing, st.busy]);
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
          ackControls(row, it, r);
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
      if (st.focusEl) { st.focusEl.focus(); st.focusEl = null; }
    }

    // renderAcked keeps the section's open or closed state: only the rows are rebuilt, and only when they change.
    function renderAcked(items, now) {
      els.acked.hidden = items.length === 0;
      els.ackedSummary.textContent = 'Acknowledged (' + items.length + ')';
      var sig = JSON.stringify([items.map(function (it) {
        return [it.id, it.level, it.title, it.detail, it.action_label, it.action_path, it.ack_token, it.acknowledged];
      }), st.busy]);
      if (sig !== st.ackedSig) {
        st.ackedSig = sig;
        clear(els.ackedRows);
        st.ackedTexts = [];
        items.forEach(function (it) {
          var r = rowModel(it, now);
          var row = hl.el('div', 'qrow');
          var g = hl.el('span', 'g off');
          g.setAttribute('aria-hidden', 'true');
          row.appendChild(g);
          var body = hl.el('div', 'qbody');
          body.appendChild(hl.el('div', 'qtitle', r.title));
          var says = hl.el('div', 't2 small', ackedText(it, now));
          body.appendChild(says);
          st.ackedTexts.push({ el: says, item: it });
          row.appendChild(body);
          r.actions.forEach(function (a) {
            var link = hl.el('a', 'btn sm', a.label);
            link.setAttribute('href', a.path);
            row.appendChild(link);
          });
          var undo = actionButton('Undo', 'undo', 'Undo acknowledging: ' + r.title);
          undo.addEventListener('click', function () { send(UNACK_URL, { id: it.id, ack_token: r.ackToken }); });
          if (!r.ackToken) { undo.disabled = true; }
          row.appendChild(undo);
          els.ackedRows.appendChild(row);
        });
      }
      st.ackedTexts.forEach(function (a) { a.el.textContent = ackedText(a.item, now); });
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
        tr.appendChild(hl.el('td', 'wrap', describe(e)));
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
      var split = splitItems(data.items);
      var last = readLastIssue();
      var m = model(data, { lastIssue: last, now: now });
      if (m.counted > 0) { writeLastIssue(now); }
      els.heading.textContent = m.heading;
      els.sub.textContent = m.sub + (st.failed ? ' The data may be out of date.' : '');
      els.glyph.hidden = m.kind !== 'clear';
      if (st.choosing !== null && !split.active.some(function (it) { return it.id === st.choosing && it.ack_token; })) { st.choosing = null; }
      els.ackError.textContent = st.ackError;
      els.ackError.hidden = !st.ackError;
      renderQueue(split.active, now);
      renderAcked(split.acked, now);
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

  hl.overview = { describe: describe, model: model, rowModel: rowModel, ageText: ageText, fmtWhen: fmtWhen, countdown: countdown,
    splitItems: splitItems, ackedText: ackedText, ACK_CHOICES: ACK_CHOICES, init: init };
  init();
})();
