(function () {
  'use strict';

  var POLL_MS = 10000;

  // ---- pure helpers (also exported for the node self-test) ----------------------------------

  function relTime(iso, nowMs) {
    if (!iso) { return ''; }
    var t = Date.parse(iso);
    if (isNaN(t)) { return ''; }
    var diff = Math.round((t - nowMs) / 1000);
    var future = diff > 0;
    var s = Math.abs(diff);
    var text;
    if (s < 5) { return 'just now'; }
    if (s < 60) { text = s + ' s'; }
    else if (s < 3600) { text = Math.floor(s / 60) + ' min'; }
    else if (s < 86400) { text = Math.floor(s / 3600) + ' h ' + Math.floor((s % 3600) / 60) + ' min'; }
    else { text = Math.floor(s / 86400) + ' d ' + Math.floor((s % 86400) / 3600) + ' h'; }
    return future ? 'in ' + text : text + ' ago';
  }

  function fmtDuration(ms) {
    if (typeof ms !== 'number' || !isFinite(ms) || ms < 0) { return ''; }
    if (ms < 1000) { return Math.round(ms) + ' ms'; }
    var s = ms / 1000;
    if (s < 60) { return s.toFixed(1) + ' s'; }
    var m = Math.floor(s / 60);
    return m + ' min ' + Math.round(s % 60) + ' s';
  }

  function fmtValue(v) {
    if (v === null || v === undefined) { return 'null'; }
    if (typeof v === 'string') { return v; }
    if (typeof v === 'number' || typeof v === 'boolean') { return String(v); }
    var j;
    try { j = JSON.stringify(v); } catch (e) { j = '[unprintable]'; }
    return j.length > 200 ? j.slice(0, 200) + '...' : j;
  }

  // The API answers {"error": ...}; helpers shared with other modules answer {"detail": ...}.
  function errorMessage(status, data) {
    if (data && typeof data === 'object') {
      if (typeof data.error === 'string' && data.error) { return data.error; }
      if (typeof data.detail === 'string' && data.detail) { return data.detail; }
    }
    return 'request failed (HTTP ' + status + ')';
  }

  // Returns [{cls, text}]: the state badges of one task, state always spelled out.
  function badgesFor(t) {
    var out = [];
    if (t.running) { out.push({ cls: 'running', text: 'running' }); }
    if (t.streak > 0) { out.push({ cls: 'failing', text: 'failing (' + t.streak + ' in a row)' }); }
    if (t.stale) { out.push({ cls: 'stale', text: 'stale' }); }
    if (!out.length) {
      out.push(t.last_success ? { cls: 'ok', text: 'ok' } : { cls: 'waiting', text: 'no success yet' });
    }
    return out;
  }

  function targetText(target) {
    if (!target) { return ''; }
    if (target.node) { return 'node ' + target.node; }
    if (target.tags && target.tags.length) {
      return 'tags ' + target.tags.join(', ') + (target.online ? ' (online only)' : '');
    }
    return '';
  }

  if (typeof document === 'undefined') {
    if (typeof module !== 'undefined' && module.exports) {
      module.exports = { relTime: relTime, fmtDuration: fmtDuration, fmtValue: fmtValue,
        errorMessage: errorMessage, badgesFor: badgesFor, targetText: targetText };
    }
    return;
  }

  // ---- DOM helpers (textContent only: nothing from the network goes through innerHTML) ------

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) { e.className = cls; }
    if (text !== undefined && text !== null) { e.textContent = text; }
    return e;
  }

  function clear(node) { while (node.firstChild) { node.removeChild(node.firstChild); } }

  function when(iso) {
    // Relative text with the absolute time as the tooltip.
    if (!iso) { return el('span', 'muted', 'never'); }
    var s = el('span', null, relTime(iso, Date.now()) || iso);
    s.title = iso;
    return s;
  }

  // ---- state ---------------------------------------------------------------------------------

  var $ = function (id) { return document.getElementById(id); };
  var sections = Object.create(null); // task id -> section parts
  var counter = 0;
  var placeholder = null;  // the loading / empty / no-data paragraph inside #tasks
  var haveData = false;
  var inflight = false;
  var again = false;
  var timer = null;

  function setStatus(text, isError) {
    var s = $('status');
    s.textContent = text || '';
    s.className = 'status' + (isError ? ' error' : '');
  }

  function setBanner(text) {
    var b = $('banner');
    b.textContent = text || '';
    b.hidden = !text;
  }

  // Resolves to {status, data}; a network failure resolves to {status: 0, data: null}.
  function call(method, path) {
    var opts = { method: method, headers: {}, credentials: 'same-origin' };
    if (method === 'POST') {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = '{}';
    }
    return fetch(path, opts).then(function (r) {
      if (r.status === 204) { return { status: 204, data: null }; }
      return r.json().catch(function () { return null; }).then(function (d) {
        return { status: r.status, data: d };
      });
    }, function () { return { status: 0, data: null }; });
  }

  function bannerFor(res) {
    if (res.status === 0) { return 'Cannot reach the server: showing the last data.'; }
    if (res.status === 503) {
      var m = errorMessage(res.status, res.data);
      return m === 'degraded' ? 'Redis is unreachable: showing the last data.' : 'Service unavailable (' + m + '): showing the last data.';
    }
    return 'Could not refresh (' + errorMessage(res.status, res.data) + '): showing the last data.';
  }

  // ---- task sections -------------------------------------------------------------------------

  function makeSection(id) {
    var root = el('section', 'task');
    var hid = 'h-' + (counter++);
    root.setAttribute('aria-labelledby', hid);
    var head = el('div', 'head');
    var title = el('h2', null, id);
    title.id = hid;
    var badges = el('span', 'badges');
    var spacer = el('span', 'spacer');
    var runBtn = el('button', 'primary', 'Run now');
    runBtn.type = 'button';
    head.appendChild(title); head.appendChild(badges); head.appendChild(spacer); head.appendChild(runBtn);
    var facts = el('dl', 'facts');
    var details = el('details', 'runs');
    var summary = el('summary', null, 'Runs');
    var runsBody = el('div', 'tablewrap');
    details.appendChild(summary); details.appendChild(runsBody);
    root.appendChild(head); root.appendChild(facts); root.appendChild(details);
    var sec = { id: id, root: root, badges: badges, runBtn: runBtn, facts: facts, details: details,
      runsBody: runsBody, busy: false, running: false, runsLoaded: false };
    runBtn.addEventListener('click', function () { runNow(sec); });
    details.addEventListener('toggle', function () { if (details.open) { loadRuns(sec); } });
    return sec;
  }

  function fact(dl, label, node) {
    dl.appendChild(el('dt', null, label));
    var dd = el('dd');
    dd.appendChild(node);
    dl.appendChild(dd);
  }

  function resultNode(res) {
    var box = el('div', 'result');
    var keys = res ? Object.keys(res).sort() : [];
    if (!keys.length) { return el('span', 'muted', res ? 'empty result' : 'none yet'); }
    keys.forEach(function (k) {
      var v = res[k];
      var s = el('span', null, k + ': ' + fmtValue(v));
      if (typeof v === 'string' && v.length > 60) { s.title = v; }
      box.appendChild(s);
    });
    return box;
  }

  function updateSection(sec, t) {
    sec.running = !!t.running;
    clear(sec.badges);
    badgesFor(t).forEach(function (b) { sec.badges.appendChild(el('span', 'badge ' + b.cls, b.text)); sec.badges.appendChild(document.createTextNode(' ')); });
    sec.runBtn.disabled = sec.busy || sec.running;
    clear(sec.facts);
    fact(sec.facts, 'Script', el('span', null, t.script || ''));
    fact(sec.facts, 'Target', el('span', null, targetText(t.target)));
    fact(sec.facts, 'Schedule', el('span', null, t.every ? 'every ' + t.every : ''));
    fact(sec.facts, 'Last success', when(t.last_success));
    fact(sec.facts, 'Failure streak', el('span', null, String(t.streak || 0)));
    fact(sec.facts, 'Next run', t.next_run ? when(t.next_run) : el('span', 'muted', 'not scheduled'));
    fact(sec.facts, 'Last result', resultNode(t.last_result));
  }

  function setPlaceholder(text) {
    var host = $('tasks');
    if (!text) {
      if (placeholder && placeholder.parentNode === host) { host.removeChild(placeholder); }
      placeholder = null;
      return;
    }
    if (!placeholder || placeholder.parentNode !== host) {
      clear(host);
      placeholder = el('p', 'muted');
      host.appendChild(placeholder);
    }
    placeholder.textContent = text;
  }

  function renderTasks(list) {
    var host = $('tasks');
    host.setAttribute('aria-busy', 'false');
    if (!Array.isArray(list)) { return; }
    var seen = {};
    var prev = null;
    list.forEach(function (t) {
      if (!t || typeof t.id !== 'string') { return; }
      seen[t.id] = true;
      var sec = sections[t.id];
      if (!sec) { sec = sections[t.id] = makeSection(t.id); }
      updateSection(sec, t);
      // Keep the server's order without recreating nodes (preserves focus and open state).
      if (placeholder) { setPlaceholder(''); }
      var want = prev ? prev.root.nextSibling : host.firstChild;
      if (want !== sec.root) { host.insertBefore(sec.root, want); }
      prev = sec;
    });
    Object.keys(sections).forEach(function (id) {
      if (!seen[id]) {
        if (sections[id].root.parentNode) { sections[id].root.parentNode.removeChild(sections[id].root); }
        delete sections[id];
      }
    });
    setPlaceholder(list.length ? '' : 'No tasks are configured.');
    $('updated').textContent = 'Updated ' + new Date().toLocaleTimeString();
  }

  // ---- run lists -----------------------------------------------------------------------------

  var RUN_COLS = ['State', 'Node', 'Attempt', 'Started', 'Duration', 'Reason', 'Script', 'Notify', ''];

  function renderRuns(sec, runs) {
    clear(sec.runsBody);
    if (!Array.isArray(runs) || !runs.length) {
      sec.runsBody.appendChild(el('p', 'muted', 'No runs recorded.'));
      return;
    }
    var table = el('table');
    var thead = el('thead');
    var hr = el('tr');
    RUN_COLS.forEach(function (c) { var th = el('th', null, c); th.scope = 'col'; hr.appendChild(th); });
    thead.appendChild(hr);
    table.appendChild(thead);
    var tb = el('tbody');
    runs.forEach(function (r) {
      var tr = el('tr');
      tr.appendChild(el('td', null, String(r.state || '')));
      tr.appendChild(el('td', null, String(r.node || '')));
      tr.appendChild(el('td', null, String(r.attempt === undefined ? '' : r.attempt)));
      var st = el('td');
      if (r.started) {
        st.appendChild(el('div', null, r.started));
        var rel = el('div', 'muted', relTime(r.started, Date.now()));
        st.appendChild(rel);
      }
      tr.appendChild(st);
      tr.appendChild(el('td', null, fmtDuration(r.duration_ms)));
      tr.appendChild(el('td', null, String(r.reason || '')));
      var sha = el('td', 'sha', String(r.script_sha || '').slice(0, 8));
      if (r.script_sha) { sha.title = r.script_sha; }
      tr.appendChild(sha);
      tr.appendChild(el('td', null, String(r.notify_outcome || '')));
      var act = el('td');
      if (r.state === 'running' && typeof r.id === 'string') {
        var b = el('button', null, 'Cancel');
        b.type = 'button';
        b.setAttribute('aria-label', 'Cancel run on ' + (r.node || 'node'));
        b.addEventListener('click', function () { cancelRun(sec, r.id, b); });
        act.appendChild(b);
      }
      tr.appendChild(act);
      tb.appendChild(tr);
    });
    table.appendChild(tb);
    sec.runsBody.appendChild(table);
  }

  function loadRuns(sec) {
    return call('GET', '/tasks/' + encodeURIComponent(sec.id) + '/runs').then(function (res) {
      if (sections[sec.id] !== sec) { return; }
      if (res.status === 200 && Array.isArray(res.data)) {
        sec.runsLoaded = true;
        renderRuns(sec, res.data);
      } else if (!sec.runsLoaded) {
        clear(sec.runsBody);
        sec.runsBody.appendChild(el('p', 'muted', 'Runs unavailable: ' + (res.status === 0 ? 'cannot reach the server' : errorMessage(res.status, res.data))));
      }
      // A failed reload keeps the previous list; the banner of the task refresh covers the cause.
    });
  }

  // ---- actions -------------------------------------------------------------------------------

  function runNow(sec) {
    sec.busy = true;
    sec.runBtn.disabled = true;
    call('POST', '/tasks/' + encodeURIComponent(sec.id) + '/run').then(function (res) {
      sec.busy = false;
      if (res.status === 202) {
        var rid = res.data && typeof res.data.run_id === 'string' ? res.data.run_id : '';
        setStatus('Started ' + sec.id + (rid ? ' (run ' + rid.slice(0, 8) + ')' : '') + '.', false);
      } else if (res.status === 0) {
        setStatus('Run now failed: cannot reach the server.', true);
      } else {
        setStatus('Run now failed for ' + sec.id + ': ' + errorMessage(res.status, res.data), true);
      }
      sec.runBtn.disabled = sec.running;
      refresh();
    });
  }

  function cancelRun(sec, runId, btn) {
    btn.disabled = true;
    call('DELETE', '/runs/' + encodeURIComponent(runId)).then(function (res) {
      if (res.status === 204) {
        setStatus('Cancel requested for ' + sec.id + '.', false);
      } else if (res.status === 0) {
        setStatus('Cancel failed: cannot reach the server.', true);
      } else {
        setStatus('Cancel failed for ' + sec.id + ': ' + errorMessage(res.status, res.data), true);
      }
      refresh();
    });
  }

  // ---- refresh loop --------------------------------------------------------------------------

  function refresh() {
    if (inflight) { again = true; return Promise.resolve(); }
    inflight = true;
    return call('GET', '/tasks.json').then(function (res) {
      if (res.status === 200 && Array.isArray(res.data)) {
        haveData = true;
        setBanner('');
        renderTasks(res.data);
        var loads = [];
        Object.keys(sections).forEach(function (id) {
          if (sections[id].details.open) { loads.push(loadRuns(sections[id])); }
        });
        return Promise.all(loads);
      }
      setBanner(bannerFor(res));
      if (!haveData) {
        $('tasks').setAttribute('aria-busy', 'false');
        setPlaceholder('No data yet.');
      }
    }).catch(function () {
      setBanner('Could not refresh: showing the last data.');
    }).then(function () {
      inflight = false;
      if (again) { again = false; return refresh(); }
    });
  }

  function visible() { return document.visibilityState !== 'hidden'; }

  function schedule() {
    clearTimeout(timer);
    timer = null;
    if (!visible()) { return; }
    timer = setTimeout(function () { refresh().then(schedule); }, POLL_MS);
  }

  placeholder = $('tasks').querySelector('p');
  document.addEventListener('visibilitychange', function () {
    if (visible()) { refresh().then(schedule); } else { clearTimeout(timer); timer = null; }
  });
  if (visible()) { refresh().then(schedule); }
}());
