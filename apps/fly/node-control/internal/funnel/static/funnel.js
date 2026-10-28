(function () {
  'use strict';
  var $ = function (id) { return document.getElementById(id); };
  var keysNS = '';

  // Every server-supplied string goes in through textContent; nothing is parsed as HTML.
  function el(tag, text, cls) {
    var e = document.createElement(tag);
    if (text !== undefined && text !== null) { e.textContent = String(text); }
    if (cls) { e.className = cls; }
    return e;
  }
  function cell(row, text, cls) { var td = el('td', text, cls); row.appendChild(td); return td; }
  function clear(node) { while (node.firstChild) { node.removeChild(node.firstChild); } }
  function show(node, text) { node.textContent = text || ''; node.hidden = !text; }

  function request(method, path, body) {
    var opts = { method: method, headers: {} };
    // Mutating requests must be JSON, even when there is nothing to send.
    if (method !== 'GET') {
      opts.headers['Content-Type'] = 'application/json';
      opts.body = JSON.stringify(body === undefined ? {} : body);
    }
    return fetch(path, opts).then(function (r) {
      if (r.status === 204) { return { status: 204, data: {} }; }
      return r.json().catch(function () { return {}; }).then(function (d) { return { status: r.status, data: d }; });
    });
  }

  function msg(res) {
    if (res.status >= 200 && res.status < 300) { return ''; }
    return (res.data && res.data.detail) || ('Request failed (' + res.status + ').');
  }

  function fmtPorts(ps) {
    return (ps || []).map(function (p) { return p.lo === p.hi ? String(p.lo) : p.lo + '-' + p.hi; }).join(',');
  }

  function parsePorts(text) {
    var out = [];
    text.split(',').forEach(function (part) {
      part = part.trim();
      if (!part) { return; }
      var m = /^(\d+)(?:-(\d+))?$/.exec(part);
      if (!m) { throw new Error('Ports must look like 8080,9000-9010'); }
      out.push({ lo: parseInt(m[1], 10), hi: parseInt(m[2] || m[1], 10) });
    });
    if (!out.length) { throw new Error('Give at least one port'); }
    return out;
  }

  function action(label, cls, fn) {
    var b = el('button', label, 'small ' + (cls || 'ghost'));
    b.type = 'button';
    b.addEventListener('click', fn);
    return b;
  }

  function renderIngress(ing) {
    var body = $('ingress-routes');
    clear(body);
    clear($('ingress-errors'));
    show($('ingress-banner'), '');
    if (!ing) { $('ingress-summary').textContent = 'The ingress has not reported yet.'; return; }
    var rep = ing.report || {};
    var bits = ['Report ' + ing.age_seconds + ' s old'];
    if (rep.applied_version) { bits.push('config ' + String(rep.applied_version).slice(0, 12)); }
    bits.push(rep.snapshot_loaded ? 'peer list loaded (' + rep.peer_count + ' peers)' : 'peer list not loaded');
    $('ingress-summary').textContent = bits.join(' · ');
    if (ing.stale) { show($('ingress-banner'), 'The ingress report is stale: it may be down or unable to reach this app.'); }
    (Array.isArray(rep.routes) ? rep.routes : []).forEach(function (r) {
      var tr = el('tr');
      cell(tr, r.service + '.' + r.ns);
      cell(tr, r.target);
      cell(tr, r.port);
      cell(tr, r.owner_node_id ? (r.owner_name || r.owner_node_id) : 'manual');
      cell(tr, r.target_online ? 'yes' : 'no', r.target_online ? '' : 'bad');
      var td = cell(tr, '', 'actions');
      if (r.owner_node_id) {
        td.appendChild(action('Block', 'danger', function () { blockRoute(r.ns, r.service); }));
      }
      body.appendChild(tr);
    });
    (Array.isArray(rep.errors) ? rep.errors : []).forEach(function (e) { $('ingress-errors').appendChild(el('li', e)); });
  }

  function blockRoute(ns, service) {
    var reason = window.prompt('Reason for blocking ' + service + '.' + ns + ' (optional)');
    if (reason === null) { return; }
    request('POST', '/funnel/blocks', { ns: ns, service: service, reason: reason }).then(function (res) {
      show($('banner'), msg(res));
      load();
    });
  }

  function renderNamespaces(list) {
    var body = $('namespaces');
    clear(body);
    list.forEach(function (n) {
      var tr = el('tr');
      cell(tr, n.name);
      var st = cell(tr, '');
      var pill = el('span', n.state, 'pill ' + n.state);
      st.appendChild(pill);
      if (n.state === 'pending' || (n.pending_ports && n.pending_ports.length)) {
        st.appendChild(el('span', ' ' + (n.change_outcome || 'pending'), 'muted'));
      }
      var ports = fmtPorts(n.ports);
      if (n.pending_ports && n.pending_ports.length) { ports += ' → ' + fmtPorts(n.pending_ports); }
      cell(tr, ports);
      cell(tr, n.note, 'wrap');
      var td = cell(tr, '', 'actions');
      td.appendChild(action('DNS', 'ghost', function () { showChecklist(n); }));
      if (n.state === 'active') {
        td.appendChild(action('Keys', 'ghost', function () { openKeys(n.name); }));
        td.appendChild(action('Ports', 'ghost', function () { changePorts(n); }));
      }
      td.appendChild(action('Delete', 'danger', function () { deleteNS(n.name); }));
      body.appendChild(tr);
    });
  }

  function showChecklist(n) {
    var box = $('checklist');
    clear(box);
    box.hidden = false;
    box.appendChild(el('strong', 'DNS and certificate for ' + n.name));
    var ul = el('ul');
    (n.checklist.records || []).forEach(function (r) { ul.appendChild(el('li', r.type + '  ' + r.name + '  →  ' + r.value)); });
    box.appendChild(ul);
    (n.checklist.commands || []).forEach(function (c) { box.appendChild(el('pre', c)); });
    (n.checklist.notes || []).forEach(function (t) { box.appendChild(el('div', t)); });
  }

  function changePorts(n) {
    var cur = fmtPorts(n.ports);
    var text = window.prompt('New ports for ' + n.name + ' (the ACL changes; confirm it on the policy page)', cur);
    if (text === null) { return; }
    var ports;
    try { ports = parsePorts(text); } catch (e) { show($('banner'), e.message); return; }
    request('PATCH', '/funnel/namespaces/' + encodeURIComponent(n.name), { ports: ports }).then(function (res) {
      show($('banner'), msg(res));
      load();
    });
  }

  function deleteNS(name) {
    if (!window.confirm('Delete namespace ' + name + '? Its ACL entries are removed through the policy console.')) { return; }
    request('DELETE', '/funnel/namespaces/' + encodeURIComponent(name)).then(function (res) {
      var m = msg(res);
      if (!m && res.data && res.data.unexpired_keys) {
        m = res.data.unexpired_keys + ' publisher key(s) for this namespace have not expired yet; expire them under Keys.';
      }
      show($('banner'), m);
      load();
    });
  }

  function renderRoutes(list) {
    var body = $('routes');
    clear(body);
    list.forEach(function (r) {
      var tr = el('tr');
      cell(tr, r.service + '.' + r.ns);
      cell(tr, r.target);
      cell(tr, r.port);
      cell(tr, r.scheme || 'http');
      var td = cell(tr, '', 'actions');
      td.appendChild(action('Delete', 'danger', function () {
        request('DELETE', '/funnel/routes/' + encodeURIComponent(r.ns) + '/' + encodeURIComponent(r.service)).then(function (res) { show($('route-message'), msg(res)); load(); });
      }));
      body.appendChild(tr);
    });
  }

  function renderBlocks(list) {
    var body = $('blocks');
    clear(body);
    list.forEach(function (b) {
      var tr = el('tr');
      cell(tr, b.service + '.' + b.ns);
      cell(tr, b.reason, 'wrap');
      cell(tr, b.actor);
      cell(tr, b.at);
      var td = cell(tr, '', 'actions');
      td.appendChild(action('Unblock', 'ghost', function () {
        request('DELETE', '/funnel/blocks/' + encodeURIComponent(b.ns) + '/' + encodeURIComponent(b.service)).then(function (res) { show($('block-message'), msg(res)); load(); });
      }));
      body.appendChild(tr);
    });
  }

  function load() {
    request('GET', '/funnel/state').then(function (res) {
      if (res.status !== 200) { show($('banner'), msg(res)); return; }
      var d = res.data;
      renderIngress(d.ingress);
      renderNamespaces(d.namespaces || []);
      renderRoutes(d.routes || []);
      renderBlocks(d.blocks || []);
    });
    request('GET', '/funnel/audit').then(function (res) {
      var body = $('audit');
      clear(body);
      ((res.data && res.data.audit) || []).forEach(function (e) {
        var tr = el('tr');
        cell(tr, e.at); cell(tr, e.actor); cell(tr, e.action); cell(tr, e.detail, 'wrap');
        body.appendChild(tr);
      });
    });
    if (keysNS) { loadKeys(); }
  }

  function openKeys(name) {
    keysNS = name;
    $('keys-panel').hidden = false;
    $('keys-ns').textContent = name;
    $('key-secret').hidden = true;
    $('key-text').value = '';
    loadKeys();
  }

  function loadKeys() {
    request('GET', '/funnel/namespaces/' + encodeURIComponent(keysNS) + '/keys').then(function (res) {
      var body = $('keys');
      clear(body);
      ((res.data && res.data.keys) || []).forEach(function (k) {
        var tr = el('tr');
        cell(tr, k.id);
        cell(tr, k.expiration, k.expired ? 'bad' : '');
        cell(tr, [k.ephemeral ? 'ephemeral' : '', k.reusable ? 'reusable' : '', k.used ? 'used' : '', k.expired ? 'expired' : ''].filter(Boolean).join(', '));
        var td = cell(tr, '', 'actions');
        if (!k.expired) {
          td.appendChild(action('Expire', 'danger', function () {
            request('DELETE', '/funnel/keys/' + k.id).then(function (r) { show($('banner'), msg(r)); loadKeys(); });
          }));
        }
        body.appendChild(tr);
      });
    });
  }

  $('ns-form').addEventListener('submit', function (ev) {
    ev.preventDefault();
    var f = ev.target;
    var ports;
    try { ports = parsePorts(f.ports.value); } catch (e) { show($('ns-message'), e.message); return; }
    request('POST', '/funnel/namespaces', { name: f.name.value, ports: ports, note: f.note.value }).then(function (res) {
      var m = msg(res);
      show($('ns-message'), m || 'ACL change submitted. Confirm it on the policy page; the namespace turns active when it is confirmed.');
      if (!m) { f.reset(); }
      load();
    });
  });

  $('key-form').addEventListener('submit', function (ev) {
    ev.preventDefault();
    var f = ev.target;
    var body = { ephemeral: f.ephemeral.checked, reusable: f.reusable.checked };
    if (f.ttl.value) { body.ttl_hours = parseInt(f.ttl.value, 10); }
    request('POST', '/funnel/namespaces/' + encodeURIComponent(keysNS) + '/keys', body).then(function (res) {
      var m = msg(res);
      show($('banner'), m);
      if (!m) {
        $('key-text').value = res.data.key;
        $('key-secret').hidden = false;
        loadKeys();
      }
    });
  });

  $('key-copy').addEventListener('click', function () {
    var t = $('key-text');
    t.select();
    if (navigator.clipboard) { navigator.clipboard.writeText(t.value); }
  });

  $('route-form').addEventListener('submit', function (ev) {
    ev.preventDefault();
    var f = ev.target;
    request('POST', '/funnel/routes', {
      ns: f.ns.value, service: f.service.value, target: f.target.value, port: parseInt(f.port.value, 10),
      scheme: f.scheme.value, insecure_skip_verify: f.insecure.checked
    }).then(function (res) {
      show($('route-message'), msg(res));
      if (!msg(res)) { f.reset(); }
      load();
    });
  });

  $('block-form').addEventListener('submit', function (ev) {
    ev.preventDefault();
    var f = ev.target;
    request('POST', '/funnel/blocks', { ns: f.ns.value, service: f.service.value, reason: f.reason.value }).then(function (res) {
      show($('block-message'), msg(res));
      if (!msg(res)) { f.reset(); }
      load();
    });
  });

  load();
  setInterval(load, 15000);
})();
