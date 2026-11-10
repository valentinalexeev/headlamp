// headlamp console: the invitation form of the Devices page (#241). An administrator invites one email address; the
// link takes the person through the identity provider and, when the verified email is the invitation's, makes one
// relay device. The invitations themselves show in the tree of the Devices page (#287), under their person or
// under "Invited, not signed in yet". The form shows nothing when self-enrolment is off (the list is a 404).
// Everything from the network goes in through textContent only.
//
// Secret: the link of a new invitation exists only in this section, right after it is made. It is never put in
// storage or a table row, and the next reload drops it.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  // mount: els { root, error, made, form, email, days }. onMade is called after an invitation is made, so the tree
  // reads the list again.
  function mount(els, onMade) {
    var st = { busy: false };

    function setError(t) { els.error.textContent = t || ''; els.error.hidden = !t; }

    function showLink(url) {
      hl.clear(els.made);
      if (!url) { els.made.hidden = true; return; }
      els.made.appendChild(hl.el('p', null, 'Send this link to the person. It is shown one time only.'));
      els.made.appendChild(hl.el('code', null, url));
      els.made.hidden = false;
    }

    // load shows the form when self-enrolment is on: GET /enroll/invites answers 404 when it is off.
    function load() {
      return hl.api('GET', '/enroll/invites').then(function () { els.root.hidden = false; }, function (e) {
        if (e && e.status === 404) { els.root.hidden = true; }
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
        if (onMade) { onMade(); }
      }, function (e) {
        st.busy = false;
        setError(hl.plain(e && e.message, 200) || 'The invitation could not be made.');
      });
    });

    return { load: load, state: st };
  }

  function init() {
    var root = document.getElementById('invites');
    if (!root) { return; }
    var ctrl = mount({
      root: root, error: document.getElementById('inviteerror'), made: document.getElementById('invitemade'),
      form: document.getElementById('inviteform'), email: document.getElementById('inviteemail'),
      days: document.getElementById('invitedays')
    }, function () { if (hl.devicesCtrl && hl.devicesCtrl.loadInvites) { hl.devicesCtrl.loadInvites(); } });
    ctrl.load();
  }

  hl.invites = { mount: mount };
  init();
})();
