// Run with: node nodes_selftest.js <path to hl.js> <path to nodes.js>. Exits non-zero on a failed check.
const assert = require('assert');
const path = require('path');

// A just-enough DOM: elements keep children and text, record listeners, and refuse innerHTML.
class FakeNode {
  constructor(tag) {
    this.tag = tag; this.children = []; this.parentNode = null; this.attrs = {}; this.listeners = {};
    this.className = ''; this._text = ''; this.hidden = false; this.disabled = false;
  }
  get textContent() { return this._text + this.children.map((c) => c.textContent).join(''); }
  set textContent(v) { this._text = String(v); this.children = []; }
  get firstChild() { return this.children[0] || null; }
  get isConnected() { return !!this.parentNode; }
  appendChild(c) { if (c.parentNode) { c.parentNode.removeChild(c); } c.parentNode = this; this.children.push(c); return c; }
  removeChild(c) { const i = this.children.indexOf(c); if (i >= 0) { this.children.splice(i, 1); } c.parentNode = null; return c; }
  remove() { if (this.parentNode) { this.parentNode.removeChild(this); } }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  addEventListener(t, f) { (this.listeners[t] = this.listeners[t] || []).push(f); }
  focus() { this.focused = true; global.document.activeElement = this; }
  set innerHTML(v) { throw new Error('innerHTML must not be used'); }
  set outerHTML(v) { throw new Error('outerHTML must not be used'); }
  click() { if (this.disabled) { return; } (this.listeners.click || []).forEach((f) => f({ preventDefault() {} })); }
}
const walk = (n, out = []) => { out.push(n); n.children.forEach((c) => walk(c, out)); return out; };
const findAll = (root, pred) => walk(root).filter(pred);
const hasClass = (n, c) => (' ' + n.className + ' ').includes(' ' + c + ' ');
const buttonNamed = (root, text) => findAll(root, (n) => n.tag === 'button' && n.textContent === text)[0];

const replaced = [];
global.document = {
  createElement: (t) => new FakeNode(t), createElementNS: (_, t) => new FakeNode(t), createTextNode: (s) => { const n = new FakeNode('#text'); n._text = s; return n; },
  getElementById: () => null, addEventListener() {}, hidden: false,
};
global.window = {
  location: { search: '', href: 'http://localhost/nodes' },
  history: { replaceState(_a, _b, url) { replaced.push(url); global.window.location.search = url.includes('?') ? url.slice(url.indexOf('?')) : ''; global.window.location.href = 'http://localhost' + url; } },
  localStorage: { getItem: () => null, setItem() {}, removeItem() {} },
};
global.setInterval = () => 0;
const fetches = [];
let fetchReply = () => ({ ok: true, status: 200, body: '{}' });
global.fetch = (url, opts) => {
  fetches.push({ url, method: (opts && opts.method) || 'GET' });
  const r = fetchReply(url, opts);
  if (r instanceof Error) { return Promise.reject(r); }
  return Promise.resolve({ ok: r.ok, status: r.status, text: () => Promise.resolve(r.body) });
};
require(path.resolve(process.argv[2]));
require(path.resolve(process.argv[3]));
const nodes = global.window.hl.nodes;
assert.ok(nodes, 'hl.nodes is defined');
const flush = () => new Promise((r) => setImmediate(r));

const NOW = Date.parse('2026-01-01T12:00:00Z');
const ago = (min) => new Date(NOW - min * 60000).toISOString();
const cfg = (extra) => Object.assign({ provider: 'oci', startStatus: 'RUNNING', stopStatus: 'STOPPED', tailnetName: undefined }, extra);
const tn = (name, online, extra) => Object.assign({ name, online, last_seen: ago(38), ip_addresses: ['100.64.0.7'], approved_route_count: 2, available_route_count: 2 }, extra);
const merge = (n, s, t, i, r) => nodes.mergeNodes(n, s, t, i, r, { now: NOW });
const one = (rows, id) => rows.find((r) => r.id === id);
const ing = (name, extra) => Object.assign({ name, host: name + '.example.net', source: 'configured', approval: 'configured', online: true, sync: { pending: false, state: 'ok', since: null } }, extra);

(async () => {
  // runningOfflineIsCritical
  {
    const rows = merge({ 'exit-b': cfg() }, { 'exit-b': { status: 'RUNNING' } }, { 'exit-b': tn('exit-b', false) }, null);
    const r = one(rows, 'exit-b');
    assert.strictEqual(r.level, 'crit');
    assert.strictEqual(r.state, 'Offline on tailnet');
    assert.ok(/Provider reports running/.test(r.detail));
    assert.strictEqual(r.role, 'Exit');
    assert.strictEqual(r.provider, 'oci');
    assert.strictEqual(r.canPower, true);
  }

  // stoppedIsOff
  {
    const r = one(merge({ 'exit-c': cfg() }, { 'exit-c': { status: 'STOPPED' } }, { 'exit-c': tn('exit-c', false) }, null), 'exit-c');
    assert.strictEqual(r.level, 'off');
    assert.strictEqual(r.state, 'Stopped');
    assert.strictEqual(r.canPower, true);
    assert.strictEqual(r.action, 'Start');
  }

  // tailnetUnknownIsUnknownNotCritical
  {
    for (const t of [null, { status: 'not_configured', detail: 'no headscale' }, {}]) {
      const r = one(merge({ 'exit-a': cfg() }, { 'exit-a': { status: 'RUNNING' } }, t, null), 'exit-a');
      assert.strictEqual(r.level, 'ok', JSON.stringify(t));
      assert.strictEqual(r.state, 'Running');
      assert.strictEqual(r.tailnet, 'Unknown');
    }
    const u = one(merge({ x: cfg({ provider: 'Tailnet only', startStatus: 'UNMANAGED', stopStatus: 'UNMANAGED' }) }, { x: { status: 'unmanaged' } }, null, null), 'x');
    assert.strictEqual(u.level, 'off');
    assert.strictEqual(u.state, 'Unknown');
    assert.strictEqual(u.canPower, false);
  }

  // bridgeMatchesByGivenName
  {
    const n = { 'exit-x': cfg({ tailnetName: 'bridge-1' }) };
    const s = { 'exit-x': { status: 'RUNNING' } };
    const t = { 'bridge-1': tn('bridge-1', true) };
    let rows = merge(n, s, t, [ing('ingress-one', { given_name: 'bridge-1' })]);
    assert.strictEqual(rows.length, 1);
    assert.strictEqual(rows[0].role, 'Bridge');
    assert.strictEqual(rows[0].canPower, true);
    assert.strictEqual(rows[0].provider, 'oci');
    rows = merge(n, s, t, [ing('bridge-1')]);
    assert.strictEqual(rows.length, 1);
    assert.strictEqual(rows[0].role, 'Bridge');
    rows = merge(n, s, t, [ing('other')]);
    assert.strictEqual(rows.length, 2);
  }

  // unmanagedBridgeHasNoPower
  {
    const rows = merge({}, {}, null, [ing('bridge-1')]);
    assert.strictEqual(rows.length, 1);
    const r = rows[0];
    assert.strictEqual(r.canPower, false);
    assert.strictEqual(r.role, 'Bridge');
    assert.strictEqual(r.provider, 'unmanaged');
    assert.strictEqual(r.level, 'ok');
    assert.strictEqual(r.state, 'Online');
  }

  // outOfSyncBeatsRunning
  {
    const bad = { pending: true, state: 'unreachable', since: Math.floor((NOW - 38 * 60000) / 1000) };
    const n = { 'bridge-2': cfg() };
    const s = { 'bridge-2': { status: 'RUNNING' } };
    let r = merge(n, s, { 'bridge-2': tn('bridge-2', true) }, [ing('bridge-2', { sync: bad })])[0];
    assert.strictEqual(r.state, 'Out of sync');
    assert.strictEqual(r.level, 'crit');
    assert.ok(/Agent unreachable/.test(r.detail));
    r = merge(n, s, { 'bridge-2': tn('bridge-2', false) }, [ing('bridge-2', { sync: bad })])[0];
    assert.strictEqual(r.state, 'Out of sync', 'beats running-but-offline');
    // stopped at the provider it serves nobody, so the wait is no risk; the same machine running is
    const waiting = { sync: { pending: true, state: 'unreachable', since: null } };
    const stopped = merge({ 'b-1': cfg() }, { 'b-1': { status: 'STOPPED' } }, null, [ing('b-1', waiting)])[0];
    assert.strictEqual(stopped.state, 'Stopped');
    assert.strictEqual(stopped.level, 'off');
    assert.strictEqual(stopped.detail, 'Sync pending');
    assert.ok(stopped.explain.includes('when it is started again'));
    assert.strictEqual(merge({ 'b-1': cfg() }, { 'b-1': { status: 'RUNNING' } }, null, [ing('b-1', waiting)])[0].state, 'Out of sync');
    // and beats a waiting approval
    r = merge({}, {}, null, [ing('bridge-3', { approval: 'pending', sync: bad })])[0];
    assert.strictEqual(r.state, 'Out of sync');
  }

  // pendingApprovalOffersCompare
  {
    const r = merge({}, {}, null, [ing('bridge-3', { source: 'discovered', approval: 'pending', sync: null })])[0];
    assert.strictEqual(r.approve, true);
    assert.strictEqual(r.state, 'Waiting for approval');
    assert.strictEqual(r.level, 'warn');
    assert.strictEqual(r.action, 'Compare');
    assert.strictEqual(r.canPower, false);
    const okRow = merge({}, {}, null, [ing('bridge-1')])[0];
    assert.strictEqual(okRow.approve, false);
    // running-but-offline comes before a waiting approval
    const both = merge({ 'bridge-3': cfg() }, { 'bridge-3': { status: 'RUNNING' } }, { 'bridge-3': tn('bridge-3', false) },
      [ing('bridge-3', { approval: 'pending', sync: null })])[0];
    assert.strictEqual(both.state, 'Offline on tailnet');
    assert.strictEqual(both.approve, true);
  }

  // runners come from the sessions list; a node that is also a runner reads Runner
  {
    const sessions = [{ id: 'runner-1', name: 'runner-1', available: true, session: { state: 'running' }, url: 'http://secret/?token=abc' }, { id: 'runner-2', name: 'runner-2', available: false, session: null }];
    const rows = merge({ 'runner-1': cfg() }, { 'runner-1': { status: 'RUNNING' } }, { 'runner-1': tn('runner-1', true) }, null, sessions);
    assert.strictEqual(rows.length, 2);
    assert.strictEqual(one(rows, 'runner-1').role, 'Runner');
    assert.strictEqual(one(rows, 'runner-1').canPower, true);
    assert.ok(/1 session/.test(one(rows, 'runner-1').detail));
    const lone = one(rows, 'runner-2');
    assert.strictEqual(lone.role, 'Runner');
    assert.strictEqual(lone.canPower, false);
    assert.ok(!JSON.stringify(rows).includes('secret'), 'the session URL is never carried');
  }

  // sort: severity, then name; summary sentence
  {
    const n = { 'exit-c': cfg(), 'exit-a': cfg(), 'exit-b': cfg() };
    const s = { 'exit-a': { status: 'RUNNING' }, 'exit-b': { status: 'RUNNING' }, 'exit-c': { status: 'STOPPED' } };
    const t = { 'exit-a': tn('exit-a', true), 'exit-b': tn('exit-b', false), 'exit-c': tn('exit-c', false) };
    const rows = merge(n, s, t, [ing('bridge-3', { approval: 'pending', sync: null })]);
    assert.deepStrictEqual(rows.map((r) => r.id), ['exit-b', 'bridge-3', 'exit-a', 'exit-c']);
    assert.strictEqual(nodes.summary(rows), '1 impaired, 1 waiting for approval, 1 stopped on purpose');
    assert.strictEqual(nodes.summary([]), '');
    assert.strictEqual(nodes.summary(rows.filter((r) => r.id === 'exit-a')), '1 node, nothing needs you');
  }

  // hostileNameStaysText
  {
    const evil = '<img src=x onerror=alert(1)>‮evil';
    const els = mkEls();
    const ctrl = nodes.mount(els);
    ctrl.update({ nodes: { [evil]: cfg() }, status: { [evil]: { status: 'RUNNING', detail: '<b>x</b>' } }, tailnet: null, ingresses: null, sessions: null, statusFresh: true });
    const cell = findAll(els.rows, (n) => hasClass(n, 'name'))[0];
    assert.ok(cell.textContent.includes('<img src=x onerror=alert(1)>'));
    assert.ok(!cell.textContent.includes('‮'));
    assert.strictEqual(findAll(els.rows, (n) => n.tag === 'img').length, 0);
    const a = findAll(cell, (n) => n.tag === 'a')[0];
    assert.ok(a.attrs.href.startsWith('/nodes?sel='));
    assert.ok(!a.attrs.href.includes('<'));
  }

  // roleFilterInURL
  {
    const els = mkEls();
    global.window.location.search = '?role=Bridge';
    const ctrl = nodes.mount(els);
    ctrl.update(sample());
    let ids = findAll(els.rows, (n) => hasClass(n, 'name')).map((n) => n.textContent);
    assert.deepStrictEqual(ids, ['bridge-3']);
    const chips = findAll(els.chips, (n) => n.tag === 'button').map((n) => n.textContent);
    assert.deepStrictEqual(chips, ['All', 'Exit', 'Bridge'], 'a chip only when it matches rows');
    buttonNamed(els.chips, 'Exit').click();
    assert.ok(replaced[replaced.length - 1].includes('role=Exit'));
    ids = findAll(els.rows, (n) => hasClass(n, 'name')).map((n) => n.textContent);
    assert.deepStrictEqual(ids, ['exit-a', 'exit-b']);
    buttonNamed(els.chips, 'All').click();
    assert.ok(!replaced[replaced.length - 1].includes('role='));
    assert.strictEqual(findAll(els.rows, (n) => hasClass(n, 'name')).length, 3);
    // a role nobody has shows everything
    global.window.location.search = '?role=Runner';
    const els2 = mkEls();
    nodes.mount(els2).update(sample());
    assert.strictEqual(findAll(els2.rows, (n) => hasClass(n, 'name')).length, 3);
    global.window.location.search = '';
  }

  // ?sel= selects the row and fills the panel
  {
    global.window.location.search = '?sel=bridge-3';
    const els = mkEls();
    nodes.mount(els).update(sample());
    assert.ok(findAll(els.rows, (n) => n.tag === 'tr' && hasClass(n, 'sel')).length === 1);
    assert.ok(/bridge-3/.test(els.panel.textContent));
    const link = findAll(els.panel, (n) => n.tag === 'a' && n.textContent === 'Compare and approve')[0];
    assert.strictEqual(link.attrs.href, '/enroll/approve?name=bridge-3');
    global.window.location.search = '';
  }

  // manageApprovalLinksToTheApprovalPage: only for an approved, discovered bridge
  {
    const appr = merge({}, {}, null, [ing('bridge-3', { source: 'discovered', approval: 'approved', approved: true })])[0];
    assert.strictEqual(appr.manage, true);
    assert.strictEqual(appr.approve, false);
    for (const x of [ing('bridge-1'), ing('bridge-3', { source: 'discovered', approval: 'pending' }), ing('bridge-3', { source: 'discovered', approval: 'changed', approved: true })]) {
      assert.strictEqual(merge({}, {}, null, [x])[0].manage, false);
    }
    global.window.location.search = '?sel=bridge-3';
    const els = mkEls();
    const d = sample();
    d.ingresses = d.ingresses.map((i) => (i.name === 'bridge-3' ? Object.assign({}, i, { source: 'discovered', approval: 'approved', approved: true }) : i));
    nodes.mount(els).update(d);
    const link = findAll(els.panel, (n) => n.tag === 'a' && n.textContent === 'Manage approval')[0];
    assert.ok(link, 'Manage approval is offered');
    assert.strictEqual(link.attrs.href, '/enroll/approve?name=bridge-3');
    assert.strictEqual(findAll(els.panel, (n) => n.tag === 'a' && n.textContent === 'Compare and approve').length, 0);
    global.window.location.search = '';
  }

  // stopNeedsConfirm
  {
    global.window.location.search = '?sel=exit-a';
    fetches.length = 0;
    const els = mkEls();
    const ctrl = nodes.mount(els);
    ctrl.update(sample());
    buttonNamed(els.panel, 'Stop').click();
    assert.strictEqual(fetches.length, 0, 'Stop alone sends nothing');
    assert.ok(/Clients using it as an exit node lose that route/.test(els.panel.textContent));
    buttonNamed(els.panel, 'Keep running').click();
    assert.strictEqual(fetches.length, 0);
    assert.strictEqual(ctrl.busyOf('exit-a'), null);
    buttonNamed(els.panel, 'Stop').click();
    buttonNamed(els.panel, 'Stop exit-a').click();
    assert.deepStrictEqual(fetches.filter((f) => f.method === 'POST'), [{ url: '/nodes/exit-a/stop', method: 'POST' }]);
    await flush();
    global.window.location.search = '';
  }

  // busyUntilPollConfirms
  {
    global.window.location.search = '?sel=exit-a';
    fetches.length = 0;
    fetchReply = () => ({ ok: true, status: 200, body: '{}' });
    const els = mkEls();
    const ctrl = nodes.mount(els);
    const data = (status, fresh) => Object.assign(sample(), { status: { 'exit-a': { status }, 'exit-b': { status: 'RUNNING' } }, statusFresh: fresh });
    ctrl.update(data('RUNNING', true));
    buttonNamed(els.panel, 'Stop').click();
    buttonNamed(els.panel, 'Stop exit-a').click();
    await flush();
    assert.strictEqual(ctrl.busyOf('exit-a'), 'stop', 'a good POST response does not clear busy');
    ctrl.update(data('RUNNING', true));
    assert.strictEqual(ctrl.busyOf('exit-a'), 'stop', 'still running: still busy');
    assert.ok(findAll(els.panel, (n) => n.tag === 'button').every((b) => b.disabled), 'both buttons are off while busy');
    ctrl.update(data('STOPPED', false));
    assert.strictEqual(ctrl.busyOf('exit-a'), 'stop', 'a failed status poll does not clear busy');
    ctrl.update(data('STOPPED', true));
    assert.strictEqual(ctrl.busyOf('exit-a'), null, 'the poll shows the target state');
    assert.ok(buttonNamed(els.panel, 'Start'));

    // a failed action keeps busy and says so
    fetchReply = () => ({ ok: false, status: 502, body: '{"detail":"provider refused"}' });
    buttonNamed(els.panel, 'Start').click();
    await flush();
    assert.strictEqual(ctrl.busyOf('exit-a'), 'start');
    assert.ok(!els.error.hidden && /provider refused/.test(els.error.textContent));
    fetchReply = () => ({ ok: true, status: 200, body: '{}' });
    global.window.location.search = '';
  }


  // busy rows read Starting or Stopping in the table and the panel header
  {
    global.window.location.search = '?sel=exit-a';
    fetchReply = () => ({ ok: true, status: 200, body: '{}' });
    const els = mkEls();
    const ctrl = nodes.mount(els);
    ctrl.update(sample());
    buttonNamed(els.panel, 'Stop').click();
    buttonNamed(els.panel, 'Stop exit-a').click();
    await flush();
    const tr = findAll(els.rows, (n) => n.tag === 'tr' && n.textContent.startsWith('exit-a'))[0];
    assert.ok(/Stopping/.test(tr.textContent), tr.textContent);
    assert.ok(/Stopping/.test(findAll(els.panel, (n) => hasClass(n, 'panelhead'))[0].textContent));
    global.window.location.search = '';
  }

  // a pending Stop confirmation and focus survive a poll whose text changed
  {
    global.window.location.search = '?sel=exit-a';
    const els = mkEls();
    const ctrl = nodes.mount(els);
    const withTime = (min) => Object.assign(sample(), { now: NOW + min * 60000, tailnet: { 'exit-a': tn('exit-a', true, { available_route_count: 2 + min }), 'exit-b': tn('exit-b', false) } });
    ctrl.update(withTime(0));
    buttonNamed(els.panel, 'Stop').click();
    const keep = buttonNamed(els.panel, 'Keep running');
    ctrl.update(withTime(5));
    assert.strictEqual(buttonNamed(els.panel, 'Keep running'), keep, 'the confirmation is not rebuilt');
    keep.click();
    // focus: the focused name link is focused again on its replacement
    ctrl.update(withTime(10));
    const link = findAll(els.rows, (n) => n.tag === 'a' && n.attrs['data-fk'] === 'name:exit-b')[0];
    link.focus();
    ctrl.update(withTime(70));
    const again = findAll(els.rows, (n) => n.tag === 'a' && n.attrs['data-fk'] === 'name:exit-b')[0];
    assert.notStrictEqual(again, link, 'the row was rebuilt');
    assert.strictEqual(again.focused, true, 'focus came back');
    global.window.location.search = '';
  }

  // the row Start button starts a stopped node under the busy rule; Open and Compare only select
  {
    fetches.length = 0;
    fetchReply = () => ({ ok: true, status: 200, body: '{}' });
    const els = mkEls();
    const ctrl = nodes.mount(els);
    const d = sample();
    d.nodes['exit-c'] = cfg(); d.status['exit-c'] = { status: 'STOPPED' }; d.tailnet['exit-c'] = tn('exit-c', false);
    ctrl.update(d);
    const startBtn = buttonNamed(els.rows, 'Start');
    startBtn.click();
    assert.deepStrictEqual(fetches.filter((f) => f.method === 'POST'), [{ url: '/nodes/exit-c/start', method: 'POST' }]);
    assert.strictEqual(ctrl.busyOf('exit-c'), 'start');
    assert.ok(buttonNamed(els.rows, 'Start').disabled, 'off while busy');
    await flush();
    fetches.length = 0;
    buttonNamed(els.rows, 'Compare').click();
    assert.strictEqual(fetches.filter((f) => f.method === 'POST').length, 0);
    assert.ok(/bridge-3/.test(els.panel.textContent));
    ctrl.update(Object.assign(d, { status: Object.assign({}, d.status, { 'exit-c': { status: 'RUNNING' } }) }));
    assert.strictEqual(ctrl.busyOf('exit-c'), null);
  }

  // Start and Stop are offered only where they make sense
  {
    for (const [status, want] of [['RUNNING', ['Stop']], ['STOPPED', ['Start']], ['STOPPING', ['Start', 'Stop']], ['error', ['Start', 'Stop']], ['unknown', []]]) {
      global.window.location.search = '?sel=x';
      const els = mkEls();
      nodes.mount(els).update({ nodes: { x: cfg() }, status: { x: { status } }, tailnet: null, ingresses: null, sessions: null, statusFresh: true });
      const got = findAll(els.panel, (n) => n.tag === 'button').map((b) => b.textContent);
      assert.deepStrictEqual(got, want, status);
    }
    global.window.location.search = '';
  }

  // row ids are unique when a bridge or runner shares an id with a node
  {
    const rows = merge({ 'bridge-1': cfg() }, { 'bridge-1': { status: 'RUNNING' } }, null, [ing('other', { given_name: 'zzz' }), ing('bridge-1', { given_name: 'nope' })], null);
    const solo = merge({ a: cfg({ tailnetName: 'real' }) }, { a: { status: 'RUNNING' } }, null, [ing('a')], [{ id: 'a', name: 'a', available: true }]);
    for (const rs of [rows, solo]) {
      assert.strictEqual(new Set(rs.map((r) => r.id)).size, rs.length, JSON.stringify(rs.map((r) => r.id)));
    }
    assert.ok(solo.length === 2 || solo.length === 3);
  }

  // an action error clears when a poll resolves that node; Refresh drops a stuck wait
  {
    global.window.location.search = '?sel=exit-a';
    const els = mkEls();
    const ctrl = nodes.mount(els);
    const data = (status) => Object.assign(sample(), { status: { 'exit-a': { status }, 'exit-b': { status: 'RUNNING' } } });
    ctrl.update(data('RUNNING'));
    fetchReply = () => ({ ok: false, status: 502, body: '{"detail":"nope"}' });
    buttonNamed(els.panel, 'Stop').click();
    buttonNamed(els.panel, 'Stop exit-a').click();
    await flush();
    assert.ok(!els.error.hidden);
    ctrl.update(data('RUNNING'));
    assert.ok(!els.error.hidden, 'not resolved yet');
    ctrl.update(data('STOPPED'));
    assert.ok(els.error.hidden, 'resolved: the error goes');
    // stuck: failed start, node stays stopped
    buttonNamed(els.panel, 'Start').click();
    await flush();
    assert.strictEqual(ctrl.busyOf('exit-a'), 'start');
    ctrl.refresh();
    assert.strictEqual(ctrl.busyOf('exit-a'), null);
    assert.ok(els.error.hidden);
    fetchReply = () => ({ ok: true, status: 200, body: '{}' });
    global.window.location.search = '';
  }

  // Refresh does not forget a wait while its request is still out, so it cannot lead to a second click
  {
    global.window.location.search = '?sel=exit-a';
    const els = mkEls();
    const ctrl = nodes.mount(els);
    ctrl.update(Object.assign(sample(), { status: { 'exit-a': { status: 'STOPPED' }, 'exit-b': { status: 'RUNNING' } } }));
    const realFetch = global.fetch;
    let answer;
    global.fetch = () => new Promise((r) => { answer = () => r({ ok: true, status: 200, text: async () => '{}' }); });
    buttonNamed(els.panel, 'Start').click();
    assert.strictEqual(ctrl.busyOf('exit-a'), 'start');
    ctrl.refresh();
    assert.strictEqual(ctrl.busyOf('exit-a'), 'start', 'the request has not answered');
    assert.ok(buttonNamed(els.panel, 'Start').disabled);
    global.fetch = realFetch;
    answer();
    await flush();
    assert.strictEqual(ctrl.busyOf('exit-a'), 'start', 'still waiting for the status to show it');
    ctrl.refresh();
    assert.strictEqual(ctrl.busyOf('exit-a'), null, 'answered, so Refresh is the way out again');
    global.window.location.search = '';
  }

  // a node in error or changing state can still be started or stopped: that is when it is needed
  for (const status of ['ERROR', 'STARTING']) {
    global.window.location.search = '?sel=exit-a';
    const els = mkEls();
    const ctrl = nodes.mount(els);
    ctrl.update(Object.assign(sample(), { status: { 'exit-a': { status }, 'exit-b': { status: 'RUNNING' } } }));
    assert.ok(buttonNamed(els.panel, 'Start'), status + ' offers Start');
    buttonNamed(els.panel, 'Stop').click();
    assert.ok(buttonNamed(els.panel, 'Stop exit-a'), status + ': Stop still asks first');
    global.window.location.search = '';
  }

  // emptyState
  {
    const els = mkEls();
    const ctrl = nodes.mount(els);
    ctrl.update({ nodes: {}, status: {}, tailnet: null, ingresses: null, sessions: null, statusFresh: true });
    assert.ok(!els.empty.hidden);
    assert.ok(els.cols.hidden);
    assert.ok(els.chips.hidden);
    assert.strictEqual(els.sub.textContent, '');
    ctrl.update(sample());
    assert.ok(els.empty.hidden);
    assert.ok(!els.cols.hidden);
  }

  assert.strictEqual(nodes.plainRole('Bridge'), 'Bridge');
  assert.strictEqual(nodes.plainRole('<x>'), 'All');

  // a 404 or 403 on the optional reads is remembered for the page session
  {
    const calls = [];
    const updates = [];
    const ctrl = { update: (d) => updates.push(d), setFailed() {} };
    let ingStatus = 404;
    const api = async (m, u) => {
      calls.push(u);
      if (u === '/nodes.json') return { nodes: { 'exit-a': cfg() } };
      if (u === '/status') return {};
      if (u === '/tailnet/status') return null;
      if (u === '/enroll/ingresses') { const e = new Error('x'); e.status = ingStatus; throw e; }
      if (u === '/sessions.json?links=0') { const e = new Error('x'); e.status = 403; throw e; }
      throw new Error('unexpected ' + u);
    };
    const reader = nodes.makeReader(api, ctrl);
    await reader.refresh();
    await reader.refresh();
    await reader.refresh();
    assert.strictEqual(calls.filter((u) => u === '/enroll/ingresses').length, 1, 'ingresses asked again after a 404');
    assert.strictEqual(calls.filter((u) => u === '/sessions.json?links=0').length, 1, 'sessions asked again after a 403');
    assert.strictEqual(calls.filter((u) => u === '/nodes.json').length, 3);
    assert.strictEqual(updates[2].ingresses, null);
    // any other failure is asked again
    const calls2 = [];
    const flaky = nodes.makeReader(async (m, u) => {
      calls2.push(u);
      if (u === '/nodes.json') return { nodes: { 'exit-a': cfg() } };
      if (u === '/status') return {};
      if (u === '/tailnet/status') return null;
      const e = new Error('x'); e.status = 500; throw e;
    }, ctrl);
    await flaky.refresh(); await flaky.refresh();
    assert.strictEqual(calls2.filter((u) => u === '/enroll/ingresses').length, 2);
  }
  console.log('ok');

  function mkEls() {
    const mk = (tag) => document.createElement(tag);
    const e = { sub: mk('p'), chips: mk('div'), error: mk('div'), empty: mk('div'), cols: mk('div'), rows: mk('tbody'), panel: mk('div') };
    e.error.hidden = true; e.empty.hidden = true; e.cols.hidden = true; e.chips.hidden = true;
    return e;
  }
  function sample() {
    return {
      nodes: { 'exit-a': cfg(), 'exit-b': cfg() },
      status: { 'exit-a': { status: 'RUNNING' }, 'exit-b': { status: 'RUNNING' } },
      tailnet: { 'exit-a': tn('exit-a', true), 'exit-b': tn('exit-b', true) },
      ingresses: [ing('bridge-3', { source: 'discovered', approval: 'pending', sync: null })],
      sessions: null, statusFresh: true,
    };
  }
})().catch((e) => { console.error(e); process.exit(1); });
