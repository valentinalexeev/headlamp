// headlamp console: the Policy page. Needs /_ui/hl.js and /_ui/shell.js first (the shell loads both). Policy text and
// everything else that came from the network goes in through textContent only; markup is never built from strings.
// The page is built around the confirm-or-revert window: a change that is live but not kept gets a countdown, a diff
// and the two ways out. Confirm is offered only on a page loaded after the change was applied (the same rule as the
// shell's strip, taken from hl.shell), because only then does reaching the page prove the change did not lock you out.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var POLL_MS = 15000;
  var RELOAD_RETRY_MS = 3000;
  var COLUMNS = 3;

  function label(v, n) { return hl.plain(v, n || 100); }
  function keyed(el, key) { el.setAttribute('data-fk', key); return el; }



  function clock(sec) {
    var s = Math.max(0, Math.floor(sec));
    var m = Math.floor(s / 60);
    var r = s % 60;
    return (m < 10 ? '0' : '') + m + ':' + (r < 10 ? '0' : '') + r;
  }

  // drawDiff writes unified diff text into a pre, one span per line, as text. Added and removed lines get the
  // add and rem classes (those are block-level); other lines are quiet and carry their own newline.
  function drawDiff(pre, text) {
    hl.clear(pre);
    var lines = String(text || '').split('\n');
    if (lines.length && lines[lines.length - 1] === '') { lines.pop(); }
    lines.forEach(function (line) {
      var head = line.indexOf('+++') === 0 || line.indexOf('---') === 0 || line.indexOf('@@') === 0;
      var cls = head ? 't3' : line.charAt(0) === '+' ? 'add' : line.charAt(0) === '-' ? 'rem' : 't3';
      pre.appendChild(hl.el('span', cls, cls === 't3' ? line + '\n' : line));
    });
  }

  var STATE_STATUS = { pending: ['warn', 'Pending'], confirmed: ['ok', 'Confirmed'], reverted: ['off', 'Reverted'] };

  // mount wires the page's elements to the data. deps (all optional): now() for the clock, reload() to reload the
  // page, loaded() for the server time the shell recorded, needsReload(item, loadedSec) for the fresh-page rule.
  function mount(els, deps) {
    deps = deps || {};
    var now = deps.now || function () { return Date.now(); };
    var reloadPage = deps.reload || function () { window.location.reload(); };
    var loadedSec = deps.loaded || function () { return hl.shell && hl.shell.loadedSeconds ? hl.shell.loadedSeconds() : NaN; };
    // Fail closed: without the shell's rule the page cannot prove it is newer than the change.
    var needsReload = deps.needsReload || function (item, sec) {
      return hl.shell && hl.shell.needsReload ? hl.shell.needsReload(item, sec) : true;
    };
    var st = {
      state: null, pending: null, history: [], loaded: false, failed: false, busy: false, receivedAt: 0,
      loadedText: null, base: '', baseSeen: '', lastPoll: 0, loading: false, loadErr: false,
      pendSig: null, editSig: null, histSig: null, countEl: null, diffText: null, gen: 0, loadSeq: 0, errKind: ''
    };
    var ctrl = {};

    // kind says where the message came from: a failed read ('load') goes when a read works again, but the
    // refusal of an action stays until the next action.
    function setError(text, kind) { els.error.textContent = text; els.error.hidden = !text; st.errKind = text ? (kind || 'action') : ''; }
    function setNotice(text) { els.notice.textContent = text; els.notice.hidden = !text; }
    function setMessage(text) { els.message.textContent = text || ''; }

    function dirty() { return st.loadedText !== null && els.editor.value !== st.loadedText; }

    function secondsLeft() {
      if (!st.pending) { return 0; }
      var left = Number(st.pending.seconds_left);
      if (!isFinite(left)) { left = 0; }
      return Math.max(0, Math.ceil(left - (now() - st.receivedAt) / 1000));
    }

    function stuck() {
      var p = st.pending;
      return !!p && Number(p.attempts) > 0; // as attention.go: past the deadline with no failed attempt is just about to revert
    }

    function fresh() {
      return !needsReload({ applied_at: st.pending && st.pending.applied_at }, loadedSec());
    }

    // ----- the pending panel

    function drawPending() {
      var p = st.pending;
      els.pending.hidden = !p;
      if (!p) { st.pendSig = null; st.countEl = null; return; }
      var isFresh = fresh();
      var isStuck = stuck();
      var reverting = !isStuck && (secondsLeft() <= 0 || !!p.overdue);
      var sig = JSON.stringify([p.id, isFresh, isStuck, Number(p.attempts), reverting, st.busy]);
      if (sig === st.pendSig) { if (st.countEl) { st.countEl.textContent = clock(secondsLeft()); } return; }
      var fk = hl.focusKey(els.pending);
      st.pendSig = sig;
      els.pending.className = isStuck ? 'note crit split' : 'note split';
      hl.clear(els.pendtitle);
      var g = hl.el('span', 'g ' + (isStuck ? 'crit' : 'warn'));
      g.setAttribute('aria-hidden', 'true');
      els.pendtitle.appendChild(g);
      var id = Number(p.id);
      // A timer only where it means something: not once the deadline has passed, and not when the revert is failing.
      var counting = !isStuck && !reverting;
      var head = isStuck ? 'Change #' + id + ' is neither confirmed nor reverted'
        : reverting ? 'Change #' + id + ' is being reverted'
        : 'Change #' + id + ' reverts in ';
      var strong = hl.el('b', 'name', head);
      st.countEl = null;
      if (counting) {
        st.countEl = hl.el('span', 'tick', clock(secondsLeft()));
        strong.appendChild(st.countEl);
      }
      els.pendtitle.appendChild(strong);

      var text;
      if (isStuck) {
        var n = Number(p.attempts);
        text = 'The automatic revert is failing (' + n + (n === 1 ? ' attempt' : ' attempts') +
          '). Check headscale. Confirm keeps the change; Revert now tries again.';
        if (!isFresh) { text += ' Reload this page first to confirm.'; }
      } else if (isFresh) {
        text = 'This page was loaded through the new rules, so reaching it shows the change did not lock you out. Confirm to keep it.';
      } else {
        text = 'This page was loaded before the change. Reload it through the new rules: if it opens, the change did not lock you out, and you can confirm it.';
      }
      els.pendtext.textContent = text;

      hl.clear(els.pendbtns);
      var primary;
      if (isFresh) {
        primary = keyed(hl.button('Confirm change', 'check', 'primary', function () { ctrl.confirmChange(); }), 'confirm');
        primary.disabled = st.busy;
      } else {
        primary = keyed(hl.button('Reload to confirm', null, 'primary', function () { reloadPage(); }), 'reload-to-confirm');
      }
      var revert = keyed(hl.button('Revert now', 'undo', '', function () { ctrl.revertNow(); }), 'revert-now');
      revert.disabled = st.busy;
      els.pendbtns.appendChild(primary);
      els.pendbtns.appendChild(revert);
      hl.refocus(els.pending, fk);
    }

    // ----- the diff of the pending change, or the editor

    function drawEditButtons() {
      var sig = JSON.stringify([!!st.pending, st.busy]);
      if (sig === st.editSig) { return; }
      var fk = hl.focusKey(els.editbtns);
      st.editSig = sig;
      hl.clear(els.editbtns);
      var pv = keyed(hl.button('Preview', null, '', function () { ctrl.preview(); }), 'preview');
      var ap = keyed(hl.button('Apply', null, 'primary', function () { ctrl.apply(); }), 'apply');
      var rl = keyed(hl.button('Reload', null, 'quiet', function () { ctrl.reloadNow(); }), 'reload');
      pv.disabled = st.busy || !!st.pending;
      ap.disabled = st.busy || !!st.pending;
      els.editbtns.appendChild(pv);
      els.editbtns.appendChild(ap);
      els.editbtns.appendChild(rl);
      hl.refocus(els.editbtns, fk);
    }

    function drawEditArea() {
      var p = st.pending;
      els.diffbox.hidden = !p;
      els.lockrow.hidden = !p;
      els.editbox.hidden = !!p || !st.loaded;
      els.editor.disabled = !!p;
      if (p) {
        var id = Number(p.id);
        els.difftitle.textContent = 'What changed in #' + id;
        var why = label(p.reason, 200);
        els.diffmeta.textContent = 'by ' + label(p.actor) + (why ? ', ' + why : '');
        // Redrawn only when the text changes, so a selection in the diff survives a poll.
        var dtext = p.diff || '(no differences)';
        if (st.diffText !== dtext) { st.diffText = dtext; drawDiff(els.diff, dtext); }
        if (!els.editlock.firstChild) {
          var ic = hl.icon('edit');
          if (ic) { els.editlock.appendChild(ic); }
          els.editlock.appendChild(hl.el('span', null, 'Edit policy'));
        }
        els.editlock.disabled = true;
        els.lockhint.textContent = 'Available after #' + id + ' is confirmed or reverted.';
      } else {
        st.diffText = null;
      }
      drawEditButtons();
    }

    // ----- history

    function drawHistory() {
      // Rebuilt only when what it shows changes (the relative times are part of that), so a poll that finds the
      // same history leaves the rows, and the focus in them, alone.
      var sig = JSON.stringify([st.loaded, !!st.pending, st.busy, st.history.map(function (e) {
        e = hl.obj(e);
        return [e.id, e.at, e.actor, e.reason, e.state, hl.relTime(String(e.at || ''), now())];
      })]);
      if (sig === st.histSig) { return; }
      st.histSig = sig;
      var fk = hl.focusKey(els.rows);
      hl.clear(els.rows);
      if (st.history.length === 0) {
        var tr0 = hl.el('tr');
        var td0 = hl.el('td', 't2', st.loaded ? 'No console changes yet.' : 'Loading.');
        td0.setAttribute('colspan', String(COLUMNS));
        tr0.appendChild(td0);
        els.rows.appendChild(tr0);
        return;
      }
      st.history.forEach(function (e) {
        e = hl.obj(e);
        var id = Number(e.id);
        var tr = hl.el('tr');
        // Who and why sit on a second line, so the table stays three columns wide.
        var first = hl.el('td', 'detail');
        first.appendChild(hl.el('div', 'mono', '#' + id));
        var why = label(e.reason, 120);
        first.appendChild(hl.el('div', 't3 small', label(e.actor) + (why ? ', ' + why : '')));
        tr.appendChild(first);
        var when = hl.el('td', 't2', hl.relTime(String(e.at || ''), now()));
        when.setAttribute('title', label(e.at, 40));
        tr.appendChild(when);
        var state = hl.el('td', 'detail');
        var known = Object.prototype.hasOwnProperty.call(STATE_STATUS, e.state) ? STATE_STATUS[e.state] : ['warn', 'Outcome unknown'];
        state.appendChild(hl.status(known[0], known[1]));
        var box = hl.el('div', 'rowbtns start');
        var b = keyed(hl.button('Revert change', 'undo', 'sm quiet', function () { ctrl.restore(id); }), 'restore:' + id);
        if (st.pending || st.busy) {
          // Undoing an older change is a new change, which cannot start while one is pending.
          b.disabled = true;
          if (st.pending) { b.setAttribute('title', 'Confirm or revert the pending change first'); }
        }
        box.appendChild(b);
        state.appendChild(box);
        tr.appendChild(state);
        els.rows.appendChild(tr);
      });
      hl.refocus(els.rows, fk);
    }

    function summary() {
      if (!st.loaded) { return st.failed ? 'Could not load the policy. Retrying.' : ''; }
      var text;
      if (st.pending) {
        var kept = null;
        for (var k = 0; k < st.history.length; k++) { if (hl.obj(st.history[k]).state === 'confirmed') { kept = hl.obj(st.history[k]); break; } }
        text = (kept ? 'Version ' + Number(kept.id) + ' confirmed. ' : '') + 'Change #' + Number(st.pending.id) + ' is live but not yet kept.';
      }
      else {
        var last = st.history.length ? hl.obj(st.history[0]) : null;
        text = last ? 'Last change #' + Number(last.id) + ' ' + label(last.state, 20) + ' ' + hl.relTime(String(last.at || ''), now()) + '.'
          : 'No console changes yet.';
      }
      if (st.failed) { text += ' The data may be out of date.'; }
      return text;
    }

    function render() {
      els.sub.textContent = summary();
      drawPending();
      drawEditArea();
      drawHistory();
    }

    // The strip shows the same change: after an action, ask it to read again instead of waiting for its poll.
    function nudgeShell() { if (hl.shell && typeof hl.shell.refresh === 'function') { hl.shell.refresh(); } }

    // ----- loading

    // update takes a /policy/state answer. The editor text is replaced only when the person has not edited it, or
    // when force says so, so a poll never eats an edit.
    ctrl.update = function (d, force) {
      d = hl.obj(d);
      var updated = String(d.updated_at || '');
      st.state = d;
      st.pending = d.pending ? hl.obj(d.pending) : null;
      st.history = hl.arr(d.history);
      st.receivedAt = now();
      st.loaded = true;
      if (force || !dirty()) {
        st.loadedText = String(d.policy || '');
        if (els.editor.value !== st.loadedText) { els.editor.value = st.loadedText; } // same text: keep caret and selection
        st.base = updated;
      } else if (updated !== st.baseSeen && updated !== st.base) {
        setMessage('The policy changed after you started editing. Apply will be refused; press Reload to discard your edit and load it.');
      }
      st.baseSeen = updated;
      render();
    };

    ctrl.setFailed = function (f) { st.failed = !!f; render(); };

    // load reads the state. An answer is dropped when an action began after the read started (that action reads
    // again itself) or a newer read exists, so a slow poll cannot bring back a panel the person just closed.
    function load(force) {
      // An action in flight reads again itself when it ends; a read begun now would only show its half-done state.
      if (st.busy) { return Promise.resolve(); }
      var gen = st.gen;
      var seq = ++st.loadSeq;
      st.loading = true;
      st.lastPoll = now();
      var current = function () { return gen === st.gen && seq === st.loadSeq; };
      return hl.api('GET', '/policy/state').then(function (j) {
        if (seq === st.loadSeq) { st.loading = false; }
        if (!current()) { return; }
        if (st.errKind === 'load') { setError(''); }
        ctrl.update(j, force);
        ctrl.setFailed(false);
      }, function (err) {
        if (seq === st.loadSeq) { st.loading = false; }
        if (!current()) { return; }
        if (err && err.status === 502) {
          setError('Headscale is unreachable or failed. The policy shown may be out of date.', 'load');
        } else {
          setError('Cannot load the policy: ' + label(err && err.message, 200), 'load');
        }
        ctrl.setFailed(true);
      });
    }

    ctrl.reload = function () { return load(false); };
    ctrl.reloadNow = function () {
      setMessage('');
      setNotice('');
      els.prevdiff.hidden = true;
      hl.clear(els.warnings);
      return load(true);
    };

    // tick runs every second: it moves the countdown, and once it reaches zero asks for the outcome until the
    // pending change is gone.
    ctrl.tick = function () {
      if (!st.pending) { return; }
      drawPending();
      if (secondsLeft() <= 0 && !st.loading && now() - st.lastPoll >= RELOAD_RETRY_MS) { load(false); }
    };

    // ----- actions

    function fail(err, what) {
      var msg = label(err && err.message, 200);
      if (err && err.status === 502) { setError('Headscale is unreachable or failed. The policy shown may be out of date.'); return; }
      if (err && err.status === 409 && /changed since/.test(msg)) {
        setError(what + ' refused: the policy changed since you loaded it. Press Reload to load the current policy; your edit stays here until you do.');
        return;
      }
      setError(what + ' failed: ' + msg);
    }

    // run does one action that changes the server: the message of a refusal stays on the page, and the state is
    // read again either way.
    function run(fn, what, force) {
      if (st.busy) { return Promise.resolve(); }
      st.busy = true;
      st.gen++;
      setError('');
      setNotice('');
      render();
      return fn().then(function (text) {
        st.busy = false;
        if (text) { setNotice(text); }
        nudgeShell();
        return load(force);
      }, function (err) {
        st.busy = false;
        fail(err, what);
        nudgeShell();
        return load(false);
      });
    }

    ctrl.confirmChange = function () {
      if (!st.pending || !fresh()) { return Promise.resolve(); }
      var id = Number(st.pending.id);
      return run(function () {
        return hl.api('POST', '/policy/confirm', { change_id: id }).then(function () { return 'Change #' + id + ' confirmed.'; });
      }, 'Confirm');
    };

    ctrl.revertNow = function () {
      if (!st.pending) { return Promise.resolve(); }
      var id = Number(st.pending.id);
      return run(function () {
        return hl.api('POST', '/policy/revert-now', { change_id: id }).then(function () { return 'Change #' + id + ' reverted.'; });
      }, 'Revert');
    };

    ctrl.restore = function (hid) {
      if (st.pending) { return Promise.resolve(); }
      return run(function () {
        return hl.api('POST', '/policy/revert/' + encodeURIComponent(String(hid)), {}).then(function (d) {
          if (hl.obj(d).uncertain) { return 'The outcome is unknown. An automatic revert is armed; reload and check the policy.'; }
          return 'Started change #' + Number(hl.obj(d).change_id) + ' to undo #' + hid + '. Confirm it to keep it.';
        });
      }, 'Revert of #' + hid, true);
    };

    ctrl.preview = function () {
      if (st.busy || st.pending) { return Promise.resolve(); }
      setError('');
      setMessage('');
      return hl.api('POST', '/policy/preview', { policy: els.editor.value }).then(function (p) {
        p = hl.obj(p);
        setMessage(p.valid ? 'Valid.' : 'Invalid: ' + label(p.error, 300));
        els.prevdiff.hidden = false;
        drawDiff(els.prevdiff, p.diff || '(no differences)');
        hl.clear(els.warnings);
        hl.arr(p.warnings).slice(0, 50).forEach(function (w) { els.warnings.appendChild(hl.el('li', null, label(w, 300))); });
      }, function (err) { fail(err, 'Preview'); });
    };

    ctrl.apply = function () {
      if (st.pending || st.busy) { return Promise.resolve(); }
      var text = els.editor.value;
      st.busy = true;
      st.gen++;
      setError('');
      setNotice('');
      render();
      return hl.api('POST', '/policy/apply', { policy: text, base_updated_at: st.base }).then(function (d) {
        st.busy = false;
        els.prevdiff.hidden = true;
        hl.clear(els.warnings);
        setMessage('');
        if (hl.obj(d).uncertain) { setError('The outcome is unknown. An automatic revert is armed; reload and check the policy.'); }
        else { setNotice('Applied as change #' + Number(hl.obj(d).change_id) + '. Confirm it to keep it.'); }
        nudgeShell();
        return load(true);
      }, function (err) {
        st.busy = false;
        fail(err, 'Apply');
        render();
        // A busy answer means a change is pending after all: show it, but keep the edit.
        if (err && err.status === 409 && !/changed since/.test(label(err.message, 200))) { return load(false); }
      });
    };

    ctrl.state = st;
    render();
    return ctrl;
  }

  function init() {
    var root = document.getElementById('policy');
    if (!root) { return; }
    var $ = function (id) { return document.getElementById(id); };
    var ctrl = mount({
      sub: $('sub'), error: $('error'), notice: $('notice'), pending: $('pending'), pendtitle: $('pendtitle'),
      pendtext: $('pendtext'), pendbtns: $('pendbtns'), diffbox: $('diffbox'), difftitle: $('difftitle'),
      diffmeta: $('diffmeta'), diff: $('diff'), lockrow: $('lockrow'), editlock: $('editlock'), lockhint: $('lockhint'),
      editbox: $('editbox'), editor: $('editor'), editbtns: $('editbtns'), message: $('message'),
      prevdiff: $('prevdiff'), warnings: $('warnings'), rows: $('rows')
    });
    var poller = hl.poll(ctrl.reload, POLL_MS);
    setInterval(function () { if (!document.hidden) { ctrl.tick(); } }, 1000);
    document.addEventListener('visibilitychange', function () { if (!document.hidden) { poller.now(); } });
  }

  hl.policy = { mount: mount, drawDiff: drawDiff, clock: clock, init: init };
  init();
})();
