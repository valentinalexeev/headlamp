// Run with: node overview_selftest.js <path to hl.js> <path to overview.js> <path to activity-text.js>. Exits non-zero on a failed check.
const assert = require('assert');
const path = require('path');

global.document = { createElement: () => ({}), createElementNS: () => ({}), getElementById: () => null, hidden: false };
global.window = { localStorage: { getItem: () => null, setItem() {}, removeItem() {} } };
global.setInterval = () => 0;
require(path.resolve(process.argv[2]));
require(path.resolve(process.argv[4])); // the shared sentences load first, as on the page
require(path.resolve(process.argv[3]));
const ov = global.window.hl.overview;
assert.ok(ov, 'hl.overview is defined');

// the activity box says what the Activity page says
assert.strictEqual(ov.describe({ action: 'device.revoke', detail: 'phone-3' }), 'phone-3 revoked');
assert.strictEqual(ov.describe({ action: 'change.reverted_on_request', detail: '46' }), 'Change #46 reverted on request');
assert.strictEqual(ov.describe, global.window.hl.activityText.describe, 'one shared description, not a copy');

const NOW = Date.parse('2026-01-01T12:00:00Z');
const item = (level, extra) => Object.assign({ id: 'i-' + Math.random(), source: 'x', level, title: 't', action_label: 'Open', action_path: '/x' }, extra);
const line = (state, name) => ({ name: name || 'Sys', path: '/sys', state, summary: '' });

// heading
{
  const six = [item('timed'), item('critical'), item('critical'), item('critical'), item('waiting'), item('waiting')];
  assert.strictEqual(ov.model({ items: six, systems: [] }, { now: NOW }).heading, '6 items need you');
  assert.strictEqual(ov.model({ items: [item('waiting')], systems: [] }, { now: NOW }).heading, '1 item needs you');
  // info items are notices: they do not count
  assert.strictEqual(ov.model({ items: [item('info'), item('waiting')], systems: [] }, { now: NOW }).heading, '1 item needs you');
}

// riskLine
{
  const three = [item('critical'), item('critical'), item('critical'), item('waiting')];
  assert.strictEqual(ov.model({ items: three, systems: [] }, { now: NOW }).sub, '3 are putting access at risk.');
  assert.strictEqual(ov.model({ items: [item('critical')], systems: [] }, { now: NOW }).sub, '1 is putting access at risk.');
  assert.ok(!/risk/.test(ov.model({ items: [item('waiting'), item('timed')], systems: [] }, { now: NOW }).sub));
}

// allClear
{
  const m = ov.model({ items: [], systems: [line('ok')] }, { now: NOW });
  assert.strictEqual(m.kind, 'clear');
  assert.strictEqual(m.heading, 'All clear');
  assert.strictEqual(m.sub, 'Nothing needs you.');
  const later = ov.model({ items: [], systems: [line('ok')] }, { now: NOW, lastIssue: NOW - 2 * 86400000 - 3600000 });
  assert.strictEqual(later.sub, 'Nothing needs you. Last issue seen 2 d 1 h ago.');
  // an info item alone does not break "All clear"
  assert.strictEqual(ov.model({ items: [item('info')], systems: [line('ok')] }, { now: NOW }).kind, 'clear');
}

// allClearPartial
{
  const m = ov.model({ items: [], systems: [line('ok')], hidden_systems: true }, { now: NOW });
  assert.strictEqual(m.heading, 'All clear in the systems you can see');
  assert.strictEqual(ov.model({ items: [], systems: [], hidden_systems: false }, { now: NOW }).heading, 'All clear');
}

// unknownIsNotClear
{
  const m = ov.model({ items: [], systems: [line('ok'), line('unknown', 'Nodes')] }, { now: NOW, lastIssue: NOW - 1000 });
  assert.notStrictEqual(m.kind, 'clear');
  assert.ok(!/All clear/.test(m.heading));
  assert.ok(/Nodes/.test(m.sub));
  // with items too: the count stays, the unknown system is said
  const both = ov.model({ items: [item('waiting')], systems: [line('unknown')] }, { now: NOW });
  assert.strictEqual(both.heading, '1 item needs you');
  assert.ok(/unknown status/.test(both.sub));
}

// queueRowHasOneAction
{
  const withChange = ov.rowModel(item('timed', { change_id: 48, action_label: 'Review and confirm', action_path: '/policy', deadline: '2026-01-01T12:04:12Z' }), NOW);
  assert.strictEqual(withChange.actions.length, 1);
  assert.strictEqual(withChange.actions[0].path, '/policy');
  assert.strictEqual(withChange.age, 'reverts in 04:12');
  assert.strictEqual(ov.rowModel(item('critical', { action_path: '//evil.test/x' }), NOW).actions.length, 0);
  assert.strictEqual(ov.rowModel(item('critical', { action_path: 'https://evil.test/' }), NOW).actions.length, 0);
  assert.strictEqual(ov.rowModel(item('critical', { action_label: '' }), NOW).actions.length, 0);
  assert.strictEqual(ov.rowModel(item('waiting'), NOW).actions.length, 1);
  assert.strictEqual(ov.rowModel(item('critical', { since: '2026-01-01T11:22:00Z' }), NOW).age, '38 min ago');
  assert.strictEqual(ov.rowModel(item('waiting', { deadline: '2026-01-05T12:00:00Z' }), NOW).age, 'next: 4 d 0 h');
  assert.strictEqual(ov.rowModel(item('mystery'), NOW).level, 'info');
}

// text from the network is plain text
{
  const r = ov.rowModel(item('waiting', { title: 'a‮b\u0000c' }), NOW);
  assert.strictEqual(r.title, 'abc');
}

assert.strictEqual(ov.countdown('2026-01-01T11:00:00Z', NOW), '00:00');
assert.strictEqual(ov.fmtWhen('garbage', NOW), '');

// init: the page against a small DOM and a fake fetch. renderQueue and renderSystems are reached through it.
class FakeNode {
  constructor(tag) { this.tag = tag; this.children = []; this.className = ''; this._text = ''; this.attrs = {}; this.hidden = false; }
  get textContent() { return this._text + this.children.map((c) => c.textContent).join(''); }
  set textContent(v) { this._text = String(v); this.children = []; }
  get firstChild() { return this.children[0] || null; }
  appendChild(c) { this.children.push(c); return c; }
  removeChild(c) { const i = this.children.indexOf(c); if (i >= 0) { this.children.splice(i, 1); } return c; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  addEventListener() {}
  set innerHTML(v) { throw new Error('innerHTML must not be used'); }
  querySelector(sel) {
    const cls = sel.replace(/^\./, '');
    const hit = (n) => (' ' + n.className + ' ').includes(' ' + cls + ' ');
    const walk = (n) => { for (const c of n.children) { if (hit(c)) { return c; } const f = walk(c); if (f) { return f; } } return null; };
    return walk(this);
  }
}
const all = (n, pred, out = []) => { n.children.forEach((c) => { if (pred(c)) { out.push(c); } all(c, pred, out); }); return out; };
const hasCls = (n, c) => (' ' + n.className + ' ').includes(' ' + c + ' ');

(async () => {
  const ids = ['overview', 'heading', 'sub', 'glyph', 'queue', 'systems', 'activity', 'activity-rows'];
  const dom = {};
  const reset = () => { ids.forEach((id) => { dom[id] = new FakeNode('div'); }); dom.glyph.hidden = true; dom.queue.hidden = true; dom.activity.hidden = true; };
  const docListeners = {};
  global.document = {
    createElement: (t) => new FakeNode(t), createElementNS: (_, t) => new FakeNode(t), createTextNode: (s) => { const n = new FakeNode('#text'); n._text = s; return n; },
    getElementById: (id) => dom[id] || null, addEventListener: (t, f) => { docListeners[t] = f; }, hidden: false,
  };
  const realSet = global.setTimeout, realClear = global.clearTimeout;
  let timers = [], nextId = 1, ageTick = null;
  global.setTimeout = (f, ms) => { const id = nextId++; timers.push({ id, f, ms }); return id; };
  global.clearTimeout = (id) => { timers = timers.filter((t) => t.id !== id); };
  global.setInterval = (f) => { ageTick = f; return 0; };
  const pollTimer = () => timers.filter((t) => t.ms === 15000);
  const settle = () => new Promise((r) => setImmediate(r));

  let attention = { items: [], systems: [], hidden_systems: false };
  let activity = { entries: [] };
  let fail = {}; // url prefix -> status, or 'net'
  const fetched = [];
  global.fetch = (url) => {
    fetched.push(url);
    const key = Object.keys(fail).find((k) => url.startsWith(k));
    if (key) {
      if (fail[key] === 'net') { return Promise.reject(new Error('network down')); }
      return Promise.resolve({ ok: false, status: fail[key], text: () => Promise.resolve('{}') });
    }
    const body = url.startsWith('/_attention.json') ? attention : activity;
    return Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve(JSON.stringify(body)) });
  };
  const rowsOf = () => dom.queue.children;
  const titleOf = (row) => row.querySelector('.qtitle').textContent;

  try {
    // no root element: init does nothing, and asks nothing
    reset(); delete dom.overview;
    ov.init(); await settle();
    assert.strictEqual(fetched.length, 0, 'no overview element, no requests');

    // first load: both lists are asked for, the queue, systems and activity are drawn
    reset();
    attention = {
      items: [item('timed', { id: 'a', title: 'Confirm change', deadline: new Date(Date.now() + 252000).toISOString(), action_label: 'Review', action_path: '/policy' }),
        item('waiting', { id: 'b', title: 'Approve bridge', detail: 'one waiting', action_path: '//evil.test/x' })],
      systems: [{ name: 'Nodes', path: '/nodes', state: 'critical', summary: 'one down' }, { name: 'Od\u202ed<b>', path: '//evil.test', state: 'mystery', summary: 's' }],
    };
    activity = { entries: [{ at: new Date().toISOString(), action: 'device.revoke', detail: 'phone-3' }] };
    ov.init(); await settle();
    assert.deepStrictEqual(fetched.slice().sort(), ['/_activity.json?limit=6', '/_attention.json']);
    assert.strictEqual(dom.heading.textContent, '2 items need you');
    assert.strictEqual(dom.glyph.hidden, true, 'no all-clear mark while something needs you');
    assert.strictEqual(dom.queue.hidden, false);
    assert.strictEqual(rowsOf().length, 2);
    assert.ok(/Confirm change/.test(titleOf(rowsOf()[0])));
    assert.ok(/^Waiting: /.test(titleOf(rowsOf()[0])), 'the screen reader word comes first');
    const links = all(rowsOf()[0], (n) => n.tag === 'a');
    assert.strictEqual(links.length, 1);
    assert.strictEqual(links[0].attrs.href, '/policy');
    assert.ok(hasCls(links[0], 'primary'), 'a timed row has a primary action');
    assert.strictEqual(all(rowsOf()[1], (n) => n.tag === 'a').length, 0, 'a path off this site is not a link');
    assert.ok(/^reverts in 0[34]:\d\d$/.test(rowsOf()[0].querySelector('.qage').textContent), rowsOf()[0].querySelector('.qage').textContent);
    // systems: a status column while anything needs you, a link only for a local path, an unknown state falls back
    const sysRows = dom.systems.children;
    assert.strictEqual(sysRows.length, 2);
    assert.strictEqual(sysRows[0].children.length, 3, 'name, status, summary');
    assert.strictEqual(all(sysRows[0], (n) => n.tag === 'a')[0].attrs.href, '/nodes');
    assert.ok(/At risk/.test(sysRows[0].children[1].textContent));
    assert.strictEqual(all(sysRows[1], (n) => n.tag === 'a').length, 0, 'no link for a path off this site');
    assert.strictEqual(sysRows[1].children[0].textContent, 'Odd<b>', 'shown as plain text');
    assert.ok(/Unknown/.test(sysRows[1].children[1].textContent));
    assert.strictEqual(dom.activity.hidden, false);
    assert.strictEqual(dom['activity-rows'].children.length, 1);
    assert.ok(/phone-3 revoked/.test(dom['activity-rows'].children[0].children[1].textContent));
    assert.strictEqual(pollTimer().length, 1, 'the next poll is scheduled');

    // a poll with the same queue keeps the rows (focus and a pending click survive) and only moves the ages
    const before = rowsOf().slice();
    const ageEl = before[0].querySelector('.qage');
    ageEl.textContent = 'stale';
    pollTimer()[0].f(); await settle();
    assert.strictEqual(rowsOf().length, 2);
    assert.ok(rowsOf().every((r, i) => r === before[i]), 'same rows, not rebuilt');
    assert.notStrictEqual(ageEl.textContent, 'stale', 'the age is refreshed');
    // the one-second tick moves the ages without a request
    const n = fetched.length;
    ageEl.textContent = 'stale';
    ageTick();
    assert.strictEqual(fetched.length, n);
    assert.notStrictEqual(ageEl.textContent, 'stale');
    global.document.hidden = true;
    ageEl.textContent = 'stale';
    ageTick();
    assert.strictEqual(ageEl.textContent, 'stale', 'a hidden tab does no work');
    global.document.hidden = false;

    // a changed queue is rebuilt
    attention = Object.assign({}, attention, { items: [attention.items[0]] });
    pollTimer()[0].f(); await settle();
    assert.strictEqual(rowsOf().length, 1);
    assert.notStrictEqual(rowsOf()[0], before[0], 'new rows after a change');
    assert.strictEqual(dom.heading.textContent, '1 item needs you');

    // nothing needing you: the queue hides, the all-clear mark shows, the systems lose their status column
    attention = { items: [], systems: [{ name: 'Nodes', path: '/nodes', state: 'ok', summary: 'fine' }] };
    pollTimer()[0].f(); await settle();
    assert.strictEqual(dom.queue.hidden, true);
    assert.strictEqual(rowsOf().length, 0);
    assert.strictEqual(dom.glyph.hidden, false);
    assert.strictEqual(dom.heading.textContent, 'All clear');
    assert.strictEqual(dom.systems.children[0].children.length, 2, 'name and summary only when clear');
    // and an empty activity hides its section
    activity = { entries: [] };
    pollTimer()[0].f(); await settle();
    assert.strictEqual(dom.activity.hidden, true);

    // a failed poll keeps what is shown and says it may be old; a 404 (the module is off) is not a failure
    fail = { '/_attention.json': 404 };
    pollTimer()[0].f(); await settle();
    assert.ok(!/out of date/.test(dom.sub.textContent), dom.sub.textContent);
    fail = { '/_attention.json': 'net' };
    pollTimer()[0].f(); await settle();
    assert.ok(/The data may be out of date\./.test(dom.sub.textContent), dom.sub.textContent);
    assert.strictEqual(dom.heading.textContent, 'All clear', 'the last answer stays up');
    fail = {};
    pollTimer()[0].f(); await settle();
    assert.ok(!/out of date/.test(dom.sub.textContent));
    // a failing activity list leaves the section as it was
    activity = { entries: [{ at: new Date().toISOString(), action: 'device.revoke', detail: 'phone-4' }] };
    pollTimer()[0].f(); await settle();
    fail = { '/_activity.json': 500 };
    pollTimer()[0].f(); await settle();
    assert.strictEqual(dom['activity-rows'].children.length, 1);
    assert.strictEqual(dom.activity.hidden, false);
    fail = {};

    // the first load failing says so, and the next success clears it
    reset(); timers = []; fetched.length = 0;
    fail = { '/_attention.json': 'net' };
    ov.init(); await settle();
    assert.strictEqual(dom.sub.textContent, 'Could not load the status. Retrying.');
    fail = {};
    attention = { items: [], systems: [] };
    pollTimer()[pollTimer().length - 1].f(); await settle();
    assert.strictEqual(dom.heading.textContent, 'All clear');

    // a tab shown again polls at once; one opened hidden still loads once and then waits
    reset(); timers = []; fetched.length = 0;
    global.document.hidden = true;
    ov.init(); await settle();
    assert.strictEqual(fetched.filter((u) => u === '/_attention.json').length, 1, 'loads once while hidden');
    assert.strictEqual(dom.heading.textContent, 'All clear');
    fetched.length = 0;
    global.document.hidden = false;
    docListeners.visibilitychange(); await settle();
    assert.strictEqual(fetched.filter((u) => u === '/_attention.json').length, 1, 'shown: asks at once');
  } finally {
    global.setTimeout = realSet; global.clearTimeout = realClear;
  }
  console.log('ok');
})().catch((e) => { console.error(e); process.exit(1); });
