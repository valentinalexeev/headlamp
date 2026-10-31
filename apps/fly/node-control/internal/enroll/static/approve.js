// headlamp console: the bridge approval review. Needs /_ui/hl.js first (the shell loads it). Everything that
// came from the network goes in through textContent only; markup is never built from strings.
//
// The page reads GET /enroll/ingresses once and shows what a bridge published and which node published it, so
// the operator can compare it with the host. The values are not refreshed under the operator's eyes: the POST
// carries the fingerprint and node id that were shown, and the server refuses it if either has moved on.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  function arr(v) { return Array.isArray(v) ? v : []; }
  function clear(node) { while (node.firstChild) { node.removeChild(node.firstChild); } }

  function button(label, iconName, cls, onClick) {
    var b = hl.el('button', 'btn' + (cls ? ' ' + cls : ''));
    b.setAttribute('type', 'button');
    var ic = iconName ? hl.icon(iconName) : null;
    if (ic) { b.appendChild(ic); }
    b.appendChild(hl.el('span', null, label));
    if (onClick) { b.addEventListener('click', onClick); }
    return b;
  }

  // approvable: a discovered bridge whose published parameters are known and which is not approved as they are.
  function approvable(v) {
    return v.source === 'discovered' && !!v.fingerprint && (v.approval === 'pending' || v.approval === 'changed');
  }

  function more(prefix) { var p = hl.plain(prefix, 16); return p ? p + '…' : ''; }

  // fieldsOf: the label and value of every line of the comparison, in the order the operator reads them.
  function fieldsOf(v) {
    return [
      ['Node', v.node_id ? String(v.node_id) : ''],
      ['Name', hl.plain(v.given_name, 63)],
      ['Hostname', hl.plain(v.hostname, 63)],
      ['Address', hl.plain(v.address, 45)],
      ['Machine key', more(v.machine_key_prefix)],
      ['Public key', more(v.public_key_prefix)],
      ['Short ID', more(v.short_id_prefix)],
      ['Fingerprint', hl.plain(v.fingerprint, 128)],
      ['Host', hl.plain(v.host, 253)]
    ];
  }

  // mount wires the page's elements. deps.go(url) leaves the page after an approval.
  function mount(els, deps) {
    deps = deps || {};
    var go = deps.go || function (url) { window.location.assign(url); };
    var st = { name: '', view: null, busy: false, error: '' };
    var ctrl = {};

    function setError(text) {
      st.error = text;
      els.error.textContent = text;
      els.error.hidden = !text;
    }

    function syncNote(sync) {
      return sync && sync.pending ? ' It is not yet on every bridge; the sync is retried.' : '';
    }

    function post(path, body, done) {
      if (st.busy) { return; }
      st.busy = true;
      setError('');
      render();
      var v = st.view;
      hl.api('POST', '/enroll/ingresses/' + encodeURIComponent(v.name) + path, body).then(function (res) {
        st.busy = false;
        done(res);
      }, function (err) {
        st.busy = false;
        setError(hl.plain(err && err.message, 200));
        // The values may have moved on: read them again so the operator reviews what is there now.
        ctrl.load(st.name, true);
      });
    }

    function approve() {
      var v = st.view;
      post('/approve', { fingerprint: v.fingerprint, node_id: v.node_id }, function (res) {
        els.sub.textContent = hl.plain(v.name, 63) + ' is approved.' + syncNote(res && res.sync);
        go('/nodes');
      });
    }

    function unapprove() {
      post('/unapprove', {}, function () { go('/nodes'); });
    }

    function ask(opts) {
      clear(els.confirm);
      hl.confirmPanel({
        title: opts.title, object: opts.object, consequence: opts.consequence,
        confirmLabel: opts.confirmLabel, cancelLabel: 'Cancel', icon: opts.icon, into: els.confirm,
        onConfirm: opts.run, onCancel: function () { render(); }
      });
      els.actions.hidden = true;
    }

    function render() {
      var v = st.view;
      clear(els.fields);
      clear(els.confirm);
      clear(els.actions);
      els.changed.hidden = true;
      clear(els.changed);
      els.info.hidden = true;
      els.review.hidden = true;
      els.actions.hidden = true;
      if (!v) {
        els.title.textContent = st.name ? 'Bridge not found' : 'Bridge approval';
        els.sub.textContent = st.loaded ? '' : 'Loading.';
        if (st.loaded) {
          els.info.textContent = st.name
            ? 'No bridge named ' + hl.plain(st.name, 63) + ' is listed. It may have been removed, or discovery may be off.'
            : 'No bridge was named. Open this page from a bridge on the Nodes page.';
          els.info.hidden = false;
        }
        return;
      }
      var name = hl.plain(v.name, 63);
      if (approvable(v)) {
        var changed = v.approval === 'changed';
        els.title.textContent = 'Approve ' + name + '?';
        els.sub.textContent = 'Once approved it receives device credentials and appears in every profile.';
        if (changed) {
          var head = hl.status('crit', name + ' changed its host identity');
          els.changed.appendChild(head);
          els.changed.appendChild(hl.el('p', 't2 small', name + ' was approved with a different identity. If you did not rebuild this host, treat it as compromised and do not approve. The approved values are not kept in a form this page can show: compare the values below with your own records of the host.'));
          if (v.detail) { els.changed.appendChild(hl.el('p', 't2 small', hl.plain(v.detail, 200))); }
          els.changed.hidden = false;
        } else if (v.detail) {
          els.info.textContent = hl.plain(v.detail, 200);
          els.info.hidden = false;
        }
        fieldsOf(v).forEach(function (f) {
          els.fields.appendChild(hl.el('dt', null, f[0]));
          els.fields.appendChild(hl.el('dd', f[1] ? 'mono' : 'mono t3', f[1] || 'not reported'));
        });
        els.review.hidden = false;
        var go1 = button(changed ? 'Approve ' + name + ' anyway' : 'Approve ' + name, 'check', changed ? 'danger' : 'primary', function () {
          if (!changed) { approve(); return; }
          ask({
            title: 'Approve a changed identity?', object: name,
            consequence: 'It receives device credentials although its identity differs from what you approved.',
            confirmLabel: 'Approve ' + name, icon: 'check', run: approve
          });
        });
        go1.disabled = st.busy;
        go1.setAttribute('data-fk', 'approve');
        els.actions.appendChild(go1);
        var notNow = hl.el('a', 'btn quiet', 'Not now');
        notNow.setAttribute('href', '/nodes');
        els.actions.appendChild(notNow);
        els.actions.hidden = false;
        return;
      }
      els.title.textContent = name;
      if (v.source === 'discovered' && v.approved && v.approval === 'approved') {
        els.sub.textContent = name + ' is approved. It receives device credentials and appears in every profile.';
        var rm = button('Remove approval', 'ban', 'danger', function () {
          ask({
            title: 'Remove the approval of ' + name + '?', object: name,
            consequence: 'It leaves the profiles and gets no more updates; it keeps the set it last received.',
            confirmLabel: 'Remove approval', icon: 'ban', run: unapprove
          });
        });
        rm.disabled = st.busy;
        els.actions.appendChild(rm);
        els.actions.hidden = false;
        return;
      }
      els.sub.textContent = '';
      els.info.textContent = name + ' cannot be approved here: ' + hl.plain(v.approval, 40) + (v.detail ? ', ' + hl.plain(v.detail, 200) : '') + '.';
      els.info.hidden = false;
    }

    ctrl.load = function (name, keepError) {
      st.name = hl.plain(name, 63);
      if (!keepError) { setError(''); }
      if (!st.name) { st.loaded = true; st.view = null; render(); return Promise.resolve(); }
      return hl.api('GET', '/enroll/ingresses').then(function (res) {
        var list = arr(res && res.ingresses);
        st.view = null;
        list.forEach(function (v) { if (v && v.name === st.name) { st.view = v; } });
        st.loaded = true;
        render();
      }, function (err) {
        st.loaded = true;
        st.view = null;
        setError('Could not read the bridge: ' + hl.plain(err && err.message, 200));
        render();
      });
    };
    ctrl.state = st;
    render();
    return ctrl;
  }

  function init() {
    var root = document.getElementById('approve');
    if (!root) { return; }
    var $ = function (id) { return document.getElementById(id); };
    var ctrl = mount({
      title: $('title'), sub: $('sub'), error: $('error'), changed: $('changed'), info: $('info'),
      review: $('review'), fields: $('fields'), confirm: $('confirm'), actions: $('actions')
    });
    ctrl.load(hl.queryParam('name') || '');
  }

  hl.approve = { mount: mount, init: init, approvable: approvable, fieldsOf: fieldsOf };
  init();
})();
