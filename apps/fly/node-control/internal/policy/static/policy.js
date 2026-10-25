(function () {
  'use strict';
  var $ = function (id) { return document.getElementById(id); };
  var baseUpdatedAt = '';
  var timer = null;
  var pendingId = 0;

  function show(el, text) {
    el.textContent = text || '';
    el.hidden = !text;
  }

  function request(method, path, body) {
    var opts = { method: method, headers: {} };
    if (body !== undefined) {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body);
    }
    return fetch(path, opts).then(function (r) {
      if (r.status === 204) { return { status: 204, data: {} }; }
      return r.json().catch(function () { return {}; }).then(function (d) {
        return { status: r.status, data: d };
      });
    });
  }

  // Shows a banner when headscale failed or the outcome of a write is unknown; returns true if shown.
  function checkBanner(res) {
    if (res.status === 502) {
      show($('banner'), 'Headscale is unreachable or failed. The policy shown may be out of date.');
      return true;
    }
    if (res.data && res.data.uncertain) {
      show($('banner'), 'The outcome is unknown. An automatic revert is armed; reload and check the policy.');
      return true;
    }
    show($('banner'), '');
    return false;
  }

  function say(res, okText) {
    if (checkBanner(res) && res.status === 502) { return; }
    if (res.status === 409) {
      show($('message'), res.data.detail === 'the policy changed since you loaded it'
        ? 'Reload: the policy changed. ' + res.data.detail : res.data.detail);
    } else if (res.status >= 400 && res.data.detail) {
      show($('message'), res.data.detail);
    } else {
      show($('message'), okText);
    }
  }

  function renderPending(p) {
    clearInterval(timer);
    pendingId = p ? p.id : 0;
    show($('stuck'), p && (p.attempts > 0 || p.overdue)
      ? 'The automatic revert is failing (' + p.attempts + ' attempts). Check headscale. Confirm keeps the change.'
      : '');
    if (!p) { $('pending').hidden = true; return; }
    var left = p.seconds_left;
    var draw = function () {
      show($('pending-text'), 'Change #' + p.id + ' by ' + p.actor + ' reverts in ' + Math.max(left, 0) + ' s unless confirmed.');
      left -= 1;
    };
    draw();
    $('pending').hidden = false;
    timer = setInterval(function () {
      draw();
      if (left < -5) { clearInterval(timer); load(); }
    }, 1000);
  }

  function renderHistory(rows) {
    var body = $('history');
    body.textContent = '';
    rows.forEach(function (e) {
      var tr = document.createElement('tr');
      [e.id, e.at, e.actor, e.reason, e.state].forEach(function (v) {
        var td = document.createElement('td');
        td.textContent = String(v);
        tr.appendChild(td);
      });
      var td = document.createElement('td');
      var b = document.createElement('button');
      b.type = 'button';
      b.textContent = 'Revert';
      b.addEventListener('click', function () {
        request('POST', '/policy/revert/' + e.id, {}).then(function (res) { say(res, 'Reverted; confirm below.'); load(); });
      });
      td.appendChild(b);
      tr.appendChild(td);
      body.appendChild(tr);
    });
  }

  function load() {
    return request('GET', '/policy/state').then(function (res) {
      if (checkBanner(res) && res.status === 502) { return; }
      if (res.status !== 200) { show($('message'), 'Cannot load the policy (' + res.status + ').'); return; }
      var s = res.data;
      $('editor').value = s.policy;
      baseUpdatedAt = s.updated_at;
      renderPending(s.pending);
      renderHistory(s.history);
      show($('last-edit'), s.last_edit
        ? 'Last console edit: #' + s.last_edit.id + ' by ' + s.last_edit.actor + ' at ' + s.last_edit.at + ' (' + s.last_edit.state + ')'
        : 'No console edits yet.');
    });
  }

  $('preview').addEventListener('click', function () {
    request('POST', '/policy/preview', { policy: $('editor').value }).then(function (res) {
      if (res.status !== 200) { say(res, ''); return; }
      var p = res.data;
      show($('message'), p.valid ? 'Valid.' : 'Invalid: ' + p.error);
      $('diff').textContent = p.diff || '(no differences)';
      var ul = $('warnings');
      ul.textContent = '';
      (p.warnings || []).forEach(function (w) {
        var li = document.createElement('li');
        li.textContent = w;
        ul.appendChild(li);
      });
    });
  });

  $('apply').addEventListener('click', function () {
    request('POST', '/policy/apply', { policy: $('editor').value, base_updated_at: baseUpdatedAt }).then(function (res) {
      say(res, 'Applied; confirm below.');
      if (res.status === 200 || res.status === 202) { load(); }
    });
  });

  $('confirm').addEventListener('click', function () {
    request('POST', '/policy/confirm', { change_id: pendingId }).then(function (res) { say(res, 'Confirmed.'); load(); });
  });

  $('reload').addEventListener('click', function () { show($('message'), ''); load(); });

  load();
})();
