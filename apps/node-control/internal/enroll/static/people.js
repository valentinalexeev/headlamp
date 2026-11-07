// headlamp console: the People section of the Devices page (#102). It lists the people who enrol themselves through
// the identity provider and locks or unlocks them. It shows nothing when self-enrolment is off (the list is a 404).
// Everything from the network goes in through textContent only.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }
  var POLL_MS = 30000;

  function stateOf(p, nowSec) {
    if (p.locked) { return { level: 'crit', word: 'Locked' + (p.lock_reason ? ': ' + hl.plain(p.lock_reason, 80) : '') }; }
    if (p.check_failed) { return { level: 'warn', word: 'Identity provider unreachable at the last check' }; }
    return { level: 'ok', word: 'Active' };
  }

  function mount(els) {
    var st = { people: [], open: '' };

    function setError(t) { els.error.textContent = t || ''; els.error.hidden = !t; }

    function act(p, path, body) {
      return hl.api('POST', '/enroll/people/' + encodeURIComponent(p.subject) + '/' + path, body || {}).then(function (j) {
        j = hl.obj(j);
        setError(j.ok === false ? hl.plain(j.detail, 300) : '');
        st.open = '';
        return load();
      }, function (e) { setError(hl.plain(e && e.message, 200) || 'The request failed.'); });
    }

    function render() {
      hl.clear(els.rows);
      var now = Date.now() / 1000;
      st.people.forEach(function (p) {
        var tr = hl.el('tr');
        var who = hl.el('td');
        who.appendChild(hl.el('div', null, hl.plain(p.email, 120) || hl.plain(p.owner, 120)));
        who.appendChild(hl.el('div', 't3 small', hl.plain(p.owner, 120)));
        tr.appendChild(who);
        var s = stateOf(p, now);
        var stc = hl.el('td');
        stc.appendChild(hl.status(s.level, s.word));
        tr.appendChild(stc);
        tr.appendChild(hl.el('td', null, String(p.active_devices | 0)));
        tr.appendChild(hl.el('td', null, p.last_login ? hl.relTime(p.last_login * 1000) : 'never'));
        tr.appendChild(hl.el('td', null, p.last_check ? hl.relTime(p.last_check * 1000) : 'never'));
        var actions = hl.el('td');
        var btns = hl.el('div', 'rowbtns');
        if (p.locked) {
          btns.appendChild(hl.button('Unlock', 'extend', '', function () { act(p, 'unlock'); }));
        } else {
          btns.appendChild(hl.button('Lock', 'ban', 'danger', function () {
            st.open = p.subject;
            hl.confirmPanel({
              title: 'Lock ' + (hl.plain(p.email, 120) || hl.plain(p.owner, 120)) + '?', object: hl.plain(p.owner, 120),
              consequence: 'All relay devices of this person are revoked on every bridge at once, their tailnet devices must sign in again, and they cannot sign in to enrol. Unlock lets them sign in, but revoked devices stay revoked.',
              confirmLabel: 'Lock', cancelLabel: 'Keep', icon: 'ban', into: actions,
              onConfirm: function () { act(p, 'lock', { reason: 'locked in the console' }); },
              onCancel: function () { st.open = ''; }
            });
          }));
        }
        actions.appendChild(btns);
        tr.appendChild(actions);
        els.rows.appendChild(tr);
      });
    }

    function load() {
      if (st.open) { return Promise.resolve(); } // never redraw under an open confirmation
      return hl.api('GET', '/enroll/people').then(function (j) {
        st.people = hl.arr(hl.obj(j).people);
        els.root.hidden = false;
        render();
      }, function (e) {
        // 404: self-enrolment is off; the section stays hidden.
        if (e && e.status === 404) { els.root.hidden = true; return; }
        if (!els.root.hidden) { setError('The list of people could not be read.'); }
      });
    }
    return { load: load, state: st, render: render };
  }

  function init() {
    var root = document.getElementById('people');
    if (!root) { return; }
    var ctrl = mount({ root: root, rows: document.getElementById('peoplerows'), error: document.getElementById('peopleerror') });
    hl.poll(ctrl.load, POLL_MS);
  }

  hl.people = { mount: mount, stateOf: stateOf };
  init();
})();
