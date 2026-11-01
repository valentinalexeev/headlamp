// headlamp console: the Tasks page. Needs /_ui/hl.js first (the shell loads it). Everything that came from the
// network, runner output included, goes in through textContent only; markup is never built from strings.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var POLL_MS = 10000;
  var COLUMNS = 6;
  var MAX_RUNS = 8;
  var MAX_RESULT_KEYS = 20;
  var RUN_WORDS = {
    succeeded: ['ok', 'Passed'], failed: ['warn', 'Failed'], lost: ['warn', 'Lost'], cancelled: ['off', 'Cancelled'],
    running: ['ok', 'Running'], queued: ['off', 'Queued']
  };

  function arr(v) { return Array.isArray(v) ? v : []; }
  function obj(v) { return v && typeof v === 'object' && !Array.isArray(v) ? v : {}; }
  function clear(node) { while (node.firstChild) { node.removeChild(node.firstChild); } }
  function label(v, n) { return hl.plain(v, n || 100); }
  function keyed(el, key) { el.setAttribute('data-fk', key); return el; }
  function cap(s) { return s ? s.charAt(0).toUpperCase() + s.slice(1) : s; }

  // alertWord: what a run's notify outcome ("alert=<outcome> ping=<outcome>", either part optional) says about its
  // alert, in words. Empty when the run had no alert to send: "alert=none", or only a healthcheck ping.
  function alertWord(outcome) {
    var m = /^alert=([\s\S]*?)(?: ping=[\s\S]*)?$/.exec(typeof outcome === 'string' ? outcome : '');
    var a = m ? m[1] : '';
    if (!a || a === 'none') { return ''; }
    if (a === 'sent') { return 'Sent'; }
    if (a === 'suppressed') { return 'Held back by the cooldown'; }
    if (a.indexOf('failed: ') === 0) { return 'Failed: ' + hl.plain(a.slice(8), 100); }
    return hl.plain(a, 100);
  }

  function fmtDuration(ms) {
    if (typeof ms !== 'number' || !isFinite(ms) || ms < 0) { return ''; }
    if (ms < 1000) { return Math.round(ms) + ' ms'; }
    var s = ms / 1000;
    if (s < 60) { return s.toFixed(1) + ' s'; }
    var m = Math.floor(s / 60);
    return m + ' min ' + Math.round(s % 60) + ' s';
  }

  // fmtValue turns one value of a task result into short text. Results come from the runner, so strings are
  // cleaned of control and direction characters and every value is cut to a fixed length.
  function fmtValue(v) {
    var j;
    if (v === null || v === undefined) { return 'null'; }
    if (typeof v === 'string') { j = label(v, -1); }
    else if (typeof v === 'number' || typeof v === 'boolean') { return String(v); }
    else {
      try { j = JSON.stringify(v); } catch (e) { j = '[unprintable]'; }
      j = label(j, -1);
    }
    return j.length > 200 ? j.slice(0, 200) + '...' : j;
  }

  function targetText(target) {
    if (!target) { return ''; }
    if (target.node) { return 'node ' + label(target.node, 100); }
    if (target.tags && target.tags.length) {
      return 'tags ' + target.tags.map(function (t) { return label(t, 100); }).join(', ') + (target.online ? ' (online only)' : '');
    }
    return '';
  }

  // taskRows turns the task list into display rows: failing first, then overdue, then the rest by name. Every
  // state has a shape (the level) and a word.
  function taskRows(list) {
    var rows = [];
    arr(list).forEach(function (t) {
      t = obj(t);
      if (typeof t.id !== 'string' || !t.id) { return; }
      var streak = typeof t.streak === 'number' && t.streak > 0 ? Math.floor(t.streak) : 0;
      var rank = 2, level, word;
      if (streak > 0) { rank = 0; level = 'warn'; word = 'Failing, ' + streak + ' in a row'; }
      else if (t.stale) { rank = 1; level = 'warn'; word = 'Overdue'; }
      else if (t.running) { level = 'ok'; word = 'Running'; }
      else if (t.last_success) { level = 'ok'; word = 'Passing'; }
      else { level = 'off'; word = 'No success yet'; }
      if (t.running && rank < 2) { word += ' (running)'; }
      rows.push({
        id: t.id, rank: rank, level: level, word: word, streak: streak, running: !!t.running, stale: !!t.stale,
        passing: streak === 0 && !t.stale && !!t.last_success, script: label(t.script, 100), target: targetText(t.target),
        every: label(t.every, 40), lastSuccess: t.last_success ? String(t.last_success) : '',
        nextRun: t.next_run ? String(t.next_run) : '', result: t.last_result
      });
    });
    rows.sort(function (a, b) { return (a.rank - b.rank) || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0); });
    return rows;
  }

  function button(text, iconName, cls, onClick) {
    var b = hl.el('button', 'btn' + (cls ? ' ' + cls : ''));
    b.setAttribute('type', 'button');
    var ic = iconName ? hl.icon(iconName) : null;
    if (ic) { b.appendChild(ic); }
    b.appendChild(hl.el('span', null, text));
    if (onClick) { b.addEventListener('click', onClick); }
    return b;
  }

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

  // mount wires the page's elements to the data. deps (all optional): now() for the clock.
  function mount(els, deps) {
    deps = deps || {};
    var now = deps.now || function () { return Date.now(); };
    var st = {
      loaded: false, failed: '', rows: [], sel: hl.queryParam('sel') || '', picked: '', busy: {}, runs: {}, runsFor: '',
      tableSig: null, panelSig: null
    };
    var ctrl = {};

    function setError(text) { els.error.textContent = text; els.error.hidden = !text; }
    function setNotice(text) { els.notice.textContent = text; els.notice.hidden = !text; }

    function when(iso) { return iso ? hl.relTime(iso, now()) : ''; }

    // The task shown in the panel: the one in the URL or last picked, else the first row.
    function selected() {
      var want = st.picked || st.sel;
      for (var i = 0; i < st.rows.length; i++) { if (st.rows[i].id === want) { return st.rows[i]; } }
      return st.rows[0] || null;
    }

    function summary() {
      if (!st.loaded) { return st.failed ? 'Could not load the tasks. Retrying.' : ''; }
      var passing = st.rows.filter(function (r) { return r.passing; }).length;
      var text = passing + ' of ' + st.rows.length + ' passing';
      if (st.failed) { text += '. The data may be out of date.'; }
      return text;
    }

    // ----- actions

    function runNow(id) {
      if (st.busy[id]) { return; }
      st.busy[id] = true;
      setError('');
      setNotice('');
      render();
      return hl.api('POST', '/tasks/' + encodeURIComponent(id) + '/run', {}).then(function (d) {
        st.busy[id] = false;
        var rid = d && typeof d.run_id === 'string' ? label(d.run_id, 64).slice(0, 8) : '';
        setNotice('Started ' + id + (rid ? ' (run ' + rid + ')' : '') + '.');
        return ctrl.reload();
      }, function (err) {
        st.busy[id] = false;
        setError('Run now failed for ' + id + ': ' + label(err && err.message, 200));
        render();
      });
    }

    function cancelRun(taskId, runId) {
      setError('');
      setNotice('');
      return hl.api('DELETE', '/runs/' + encodeURIComponent(runId)).then(function () {
        setNotice('Cancel requested for ' + taskId + '.');
        return ctrl.reload();
      }, function (err) {
        setError('Cancel failed for ' + taskId + ': ' + label(err && err.message, 200));
      });
    }

    function runButton(r, cls, fk) {
      var b = button(st.busy[r.id] ? 'Starting' : 'Run now', 'play', cls, function () { runNow(r.id); });
      b.setAttribute('aria-label', (st.busy[r.id] ? 'Starting ' : 'Run ') + r.id + (st.busy[r.id] ? '' : ' now'));
      keyed(b, fk + r.id);
      b.disabled = !!(st.busy[r.id] || r.running);
      if (r.running && !st.busy[r.id]) { b.setAttribute('title', r.id + ' is running now.'); }
      return b;
    }

    // ----- the table

    function renderTable(sel) {
      var shown = st.rows.map(function (r) {
        return { id: r.id, level: r.level, word: r.word, last: r.lastSuccess ? cap(when(r.lastSuccess)) : 'Never',
          next: r.nextRun ? cap(when(r.nextRun)) : (r.running ? 'Running now' : 'Not scheduled'), target: r.target || '-',
          running: r.running, busy: !!st.busy[r.id] };
      });
      var sig = JSON.stringify([shown, sel && sel.id]);
      if (sig === st.tableSig) { return; }
      st.tableSig = sig;
      var fk = focusKey(els.rows);
      clear(els.rows);
      st.rows.forEach(function (r, i) {
        var s = shown[i];
        var isSel = !!sel && sel.id === r.id;
        var tr = hl.el('tr', isSel ? 'sel' : '');
        var name = hl.el('td');
        var pick = keyed(hl.el('button', 'btn sm quiet mono name', r.id), 'pick:' + r.id);
        pick.setAttribute('type', 'button');
        pick.setAttribute('aria-pressed', isSel ? 'true' : 'false');
        pick.addEventListener('click', function () { ctrl.select(r.id); });
        name.appendChild(pick);
        tr.appendChild(name);
        var state = hl.el('td');
        state.appendChild(hl.status(r.level, r.word));
        tr.appendChild(state);
        tr.appendChild(hl.el('td', 't2', s.last));
        tr.appendChild(hl.el('td', 't2', s.next));
        tr.appendChild(hl.el('td', 'mono t2', s.target));
        var act = hl.el('td', 'act');
        var box = hl.el('div', 'rowbtns');
        box.appendChild(runButton(r, 'sm' + (isSel ? '' : ' quiet'), 'run:'));
        act.appendChild(box);
        tr.appendChild(act);
        els.rows.appendChild(tr);
      });
      refocus(els.rows, fk);
    }

    // ----- the panel of the selected task

    function lastFailure(runs) {
      for (var i = 0; i < runs.length; i++) {
        var r = obj(runs[i]);
        if ((r.state === 'failed' || r.state === 'lost') && r.reason) { return r; }
      }
      return null;
    }

    function resultNode(res) {
      var keys = Object.keys(obj(res)).sort().slice(0, MAX_RESULT_KEYS);
      if (!res) { return hl.el('span', 't3', 'None yet'); }
      if (!keys.length) { return hl.el('span', 't3', 'Empty result'); }
      var box = hl.el('div', 'stack');
      keys.forEach(function (k) { box.appendChild(hl.el('p', 'mono small', label(k, 60) + ': ' + fmtValue(res[k]))); });
      return box;
    }

    function renderPanel(sel) {
      var runs = sel && st.runsFor === sel.id ? st.runs[sel.id] : null;
      var shown = sel && [sel, !!st.busy[sel.id], runs, sel.lastSuccess ? when(sel.lastSuccess) : '', sel.nextRun ? when(sel.nextRun) : ''];
      var sig = JSON.stringify(shown || null);
      if (sig === st.panelSig) { return; }
      st.panelSig = sig;
      var fk = focusKey(els.panel);
      clear(els.panel);
      if (!sel) { return; }
      var panel = hl.el('div', 'panel');
      var head = hl.el('div', 'panelhead');
      head.appendChild(hl.el('h2', 'mono', sel.id));
      head.appendChild(runButton(sel, 'sm', 'panelrun:'));
      panel.appendChild(head);

      var fail = runs ? lastFailure(runs) : null;
      if (sel.streak > 0 && fail) {
        var note = hl.el('div', 'note');
        note.appendChild(hl.el('p', null, label(fail.reason, 400)));
        var ago = fail.ended || fail.started;
        note.appendChild(hl.el('p', 't2 small', 'Last failure ' + when(String(ago || '')) + (fail.node ? ' on ' + label(fail.node, 100) : '') + '.'));
        panel.appendChild(note);
      }

      var kv = hl.el('dl', 'kv');
      function fact(k, node) {
        kv.appendChild(hl.el('dt', null, k));
        var dd = hl.el('dd');
        dd.appendChild(typeof node === 'string' ? hl.el('span', null, node) : node);
        kv.appendChild(dd);
      }
      fact('Last success', sel.lastSuccess ? cap(when(sel.lastSuccess)) : 'Never');
      fact('Script', hl.el('span', 'mono', sel.script || '-'));
      fact('Target', sel.target || '-');
      fact('Schedule', sel.every ? 'Every ' + sel.every : '-');
      fact('Failure streak', String(sel.streak));
      fact('Next run', sel.nextRun ? cap(when(sel.nextRun)) : (sel.running ? 'Running now' : 'Not scheduled'));
      var lastAlert = '';
      (runs || []).some(function (r) {
        r = obj(r);
        var word = alertWord(r.notify_outcome);
        if (!word) { return false; }
        var at = when(String(r.ended || r.started || ''));
        lastAlert = word + (at ? ', ' + at : '');
        return true;
      });
      if (lastAlert) { fact('Last alert', lastAlert); }
      fact('Last result', resultNode(sel.result));
      panel.appendChild(kv);

      var sec = hl.el('div');
      var lab = hl.el('div', 'seclabel');
      lab.appendChild(hl.el('h2', null, 'Recent runs'));
      sec.appendChild(lab);
      if (!runs) {
        sec.appendChild(hl.el('p', 't2', st.failed ? 'The runs are not available right now.' : 'Loading the runs.'));
      } else if (!runs.length) {
        sec.appendChild(hl.el('p', 't2', 'No runs recorded yet.'));
      } else {
        var tw = hl.el('div', 'tw');
        var table = hl.el('table');
        var tb = hl.el('tbody');
        runs.slice(0, MAX_RUNS).forEach(function (r) {
          r = obj(r);
          var w = RUN_WORDS[r.state] || ['off', label(r.state, 30) || 'Unknown'];
          var tr = hl.el('tr');
          var t = hl.el('td', 'mono t3', r.started ? when(String(r.started)) : '-');
          if (r.started) { t.setAttribute('title', label(r.started, 40)); }
          tr.appendChild(t);
          var s = hl.el('td');
          s.appendChild(hl.status(w[0], w[1]));
          tr.appendChild(s);
          tr.appendChild(hl.el('td', 'mono t2', fmtDuration(r.duration_ms) || '-'));
          var act = hl.el('td', 'act');
          if (r.state === 'running' && typeof r.id === 'string') {
            var c = keyed(button('Cancel', null, 'sm quiet', function () { cancelRun(sel.id, r.id); }), 'cancel:' + r.id);
            c.setAttribute('aria-label', 'Cancel the run on ' + label(r.node || 'the node', 100));
            act.appendChild(c);
          }
          tr.appendChild(act);
          tb.appendChild(tr);
        });
        table.appendChild(tb);
        tw.appendChild(table);
        sec.appendChild(tw);
      }
      panel.appendChild(sec);
      els.panel.appendChild(panel);
      refocus(els.panel, fk);
    }

    function renderStale() {
      els.stale.textContent = st.failed ? 'Could not refresh (' + st.failed + '). Showing the last data.' : '';
      els.stale.hidden = !st.failed || !st.loaded;
    }

    function render() {
      els.sub.textContent = summary();
      renderStale();
      var empty = st.loaded && st.rows.length === 0;
      els.empty.hidden = !empty;
      els.cols.hidden = st.rows.length === 0;
      if (empty) { els.emptytext.textContent = 'Tasks come from the task configuration. Add one there and it shows here once the service has read it.'; }
      var sel = selected();
      renderTable(sel);
      renderPanel(sel);
    }

    function loadRuns() {
      var sel = selected();
      if (!sel) { return Promise.resolve(); }
      var id = sel.id;
      return hl.api('GET', '/tasks/' + encodeURIComponent(id) + '/runs').then(function (j) {
        st.runs[id] = arr(j);
        st.runsFor = id;
        render();
      }, function () { /* the stale note of the list reload covers the cause; the old runs stay */ });
    }

    // ----- control surface

    ctrl.select = function (id) {
      st.picked = id;
      hl.setQueryParam('sel', id);
      st.runsFor = st.runs[id] ? id : '';
      render();
      return loadRuns();
    };
    ctrl.update = function (list) {
      st.rows = taskRows(list);
      st.loaded = true;
      render();
      return loadRuns();
    };
    ctrl.reload = function () {
      return hl.api('GET', '/tasks.json').then(function (j) {
        st.failed = '';
        return ctrl.update(j);
      }, function (err) {
        st.failed = label(err && err.message, 120) || 'request failed';
        render();
      });
    };
    ctrl.state = st;
    render();
    return ctrl;
  }

  function init() {
    var root = document.getElementById('tasks');
    if (!root) { return; }
    var $ = function (id) { return document.getElementById(id); };
    var ctrl = mount({
      sub: $('sub'), error: $('error'), notice: $('notice'), stale: $('stale'), empty: $('empty'), emptytext: $('emptytext'),
      cols: $('cols'), rows: $('rows'), panel: $('panel')
    });
    var poller = hl.poll(ctrl.reload, POLL_MS);
    document.addEventListener('visibilitychange', function () { if (!document.hidden) { poller.now(); } });
  }

  hl.tasks = { taskRows: taskRows, fmtDuration: fmtDuration, fmtValue: fmtValue, targetText: targetText, mount: mount, init: init };
  init();
})();
