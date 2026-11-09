// headlamp console: the Invitations section of the Devices page (#241). An administrator invites one
// email address; the link takes the person through the identity provider and, when the verified email
// is the invitation's, makes one relay device. The section shows nothing when self-enrolment is off
// (the list is a 404). Everything from the network goes in through textContent only.
//
// Secret: the link of a new invitation exists only in this section, right after it is made. It is
// never put in storage or a table row, and the next reload drops it.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }
  var POLL_MS = 30000;
  var WORD = { pending: ['ok', 'Waiting'], used: ['off', 'Used'], expired: ['warn', 'Expired'], revoked: ['off', 'Withdrawn'] };

  function mount(els) {
    var st = { invites: [], busy: false };

    function setError(t) { els.error.textContent = t || ''; els.error.hidden = !t; }

    function showLink(url) {
      hl.clear(els.made);
      if (!url) { els.made.hidden = true; return; }
      els.made.appendChild(hl.el('p', null, 'Send this link to the person. It is shown one time only.'));
      var code = hl.el('code', null, url);
      els.made.appendChild(code);
      els.made.hidden = false;
    }

    function render() {
      hl.clear(els.rows);
      st.invites.forEach(function (inv) {
        var tr = hl.el('tr');
        var who = hl.el('td');
        who.appendChild(hl.el('div', null, hl.plain(inv.email, 120)));
        who.appendChild(hl.el('div', 't3 small', hl.plain(inv.device_name, 100)));
        tr.appendChild(who);
        var w = WORD[inv.state] || ['warn', hl.plain(inv.state, 20)];
        var stc = hl.el('td');
        stc.appendChild(hl.status(w[0], w[1]));
        tr.appendChild(stc);
        tr.appendChild(hl.el('td', null, inv.expires_at ? new Date(inv.expires_at * 1000).toLocaleDateString() : ''));
        tr.appendChild(hl.el('td', null, hl.plain(inv.created_by, 80)));
        var actions = hl.el('td');
        if (inv.state === 'pending') {
          actions.appendChild(hl.button('Withdraw', 'ban', 'danger', function () {
            hl.api('POST', '/enroll/invites/' + encodeURIComponent(inv.id) + '/revoke', {}).then(function () {
              setError('');
              return load();
            }, function (e) { setError(hl.plain(e && e.message, 200) || 'The request failed.'); });
          }));
        }
        tr.appendChild(actions);
        els.rows.appendChild(tr);
      });
    }

    function load() {
      return hl.api('GET', '/enroll/invites').then(function (j) {
        st.invites = hl.arr(hl.obj(j).invites);
        els.root.hidden = false;
        render();
      }, function (e) {
        // 404: self-enrolment is off; the section stays hidden.
        if (e && e.status === 404) { els.root.hidden = true; return; }
        if (!els.root.hidden) { setError('The list of invitations could not be read.'); }
      });
    }

    els.form.addEventListener('submit', function (ev) {
      ev.preventDefault();
      if (st.busy) { return; }
      var body = { email: els.email.value.trim() };
      var days = parseInt(els.days.value, 10);
      if (days > 0) { body.days = days; }
      st.busy = true;
      hl.api('POST', '/enroll/invites', body).then(function (j) {
        st.busy = false;
        setError('');
        els.email.value = '';
        showLink(hl.plain(hl.obj(j).url, 500));
        return load();
      }, function (e) {
        st.busy = false;
        setError(hl.plain(e && e.message, 200) || 'The invitation could not be made.');
      });
    });

    return { load: load, state: st, render: render };
  }

  function init() {
    var root = document.getElementById('invites');
    if (!root) { return; }
    var ctrl = mount({
      root: root, rows: document.getElementById('inviterows'), error: document.getElementById('inviteerror'),
      made: document.getElementById('invitemade'), form: document.getElementById('inviteform'),
      email: document.getElementById('inviteemail'),
      days: document.getElementById('invitedays')
    });
    hl.poll(ctrl.load, POLL_MS);
  }

  hl.invites = { mount: mount };
  init();
})();
