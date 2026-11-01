// Run with: node activity_selftest.js <path to hl.js> <path to activity.js> <path to activity-text.js>. Exits non-zero on a failed check.
const assert = require('assert');
const path = require('path');

function node(tag) {
  return {
    tag, children: [], className: '', textContent: '', attrs: {}, listeners: {}, hidden: false,
    appendChild(c) { this.children.push(c); return c; },
    removeChild(c) { this.children.splice(this.children.indexOf(c), 1); },
    get firstChild() { return this.children[0] || null; },
    setAttribute(k, v) { this.attrs[k] = String(v); },
    getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; },
    addEventListener(t, f) { this.listeners[t] = f; },
    click() { return this.listeners.click(); },
  };
}
function find(n, pred, out = []) { if (pred(n)) out.push(n); n.children.forEach((c) => find(c, pred, out)); return out; }

global.document = { createElement: node, createElementNS: (ns, t) => node(t), getElementById: () => null, hidden: false, addEventListener() {} };
global.window = { localStorage: { getItem: () => null, setItem() {}, removeItem() {} } };
global.setInterval = () => 0;
require(path.resolve(process.argv[2]));
require(path.resolve(process.argv[4])); // the shared sentences load first, as on the page
require(path.resolve(process.argv[3]));
const act = global.window.hl.activity;
assert.ok(act, 'hl.activity is defined');

function els() {
  const e = {};
  ['chips', 'error', 'notice', 'empty', 'emptyhead', 'emptytext', 'table', 'rows'].forEach((k) => { e[k] = node('div'); });
  return e;
}
const flush = () => new Promise((r) => setImmediate(r));
const NOW = new Date(2026, 9, 31, 15, 0).getTime();
const at = (d, h, m) => new Date(2026, 9, d, h, m).toISOString();
const rowText = (tr) => tr.children.map((td) => td.textContent);

(async () => {
  // describe: the friendly sentence for each action
  {
    const d = (action, detail) => act.describe({ action, detail });
    assert.strictEqual(d('device.create', 'tablet-2, expires 2027-01-01'), 'tablet-2 enrolled');
    assert.strictEqual(d('device.revoke', 'phone-3'), 'phone-3 revoked');
    assert.strictEqual(d('device.extend', 'phone, expires 2027-02-01'), 'phone extended until 2027-02-01');
    assert.strictEqual(d('bridge.approve', 'bridge-1'), 'Bridge bridge-1 approved');
    assert.strictEqual(d('node.stop', 'exit-c: ok'), 'exit-c stopped');
    assert.strictEqual(d('node.start', 'exit-a: failed (timeout)'), 'Starting exit-a failed (timeout)');
    assert.strictEqual(d('namespace.create', 'demo ports 9000-9010'), 'Namespace demo created, ports 9000-9010');
    assert.strictEqual(d('key.mint', 'lab id 14 ephemeral=false reusable=true ttl=24h'), 'Key 14 minted for lab');
    assert.strictEqual(d('route.put', 'demo/wiki port 80'), 'Route wiki.demo set, port 80');
    assert.strictEqual(d('block.put', 'demo/wiki'), 'wiki.demo blocked');
    assert.strictEqual(d('change.applied', '48: add namespace demo'), 'Change #48 applied: add namespace demo');
    assert.strictEqual(d('change.applied', '48'), 'Change #48 applied');
    assert.strictEqual(d('change.reverted', '46'), 'Change #46 reverted by itself after no confirmation');
    assert.strictEqual(d('change.confirmed', '47'), 'Change #47 confirmed');
    assert.strictEqual(d('change.reverted_earlier', '45'), 'Change #45 reverted');
    assert.strictEqual(d('change.reverted_on_request', '46'), 'Change #46 reverted on request');
    assert.strictEqual(act.describe, global.window.hl.activityText.describe, 'one shared description, not a copy');
    assert.strictEqual(d('task.manual', 'login-check: succeeded'), 'login-check run by hand');
    assert.strictEqual(d('task.run', 'login-check: failed'), 'login-check run failed');
    // not recognised: shown as it is, but cleaned
    assert.strictEqual(d('thing.done', 'x‮y'), 'thing.done: xy');
    assert.strictEqual(d('key.mint', 'garbage'), 'Key minted');
  }

  // fmtWhen
  {
    assert.strictEqual(act.fmtWhen(at(31, 14, 31), NOW), 'Today 14:31');
    assert.strictEqual(act.fmtWhen(at(30, 18, 40), NOW), 'Yesterday 18:40');
    assert.strictEqual(act.fmtWhen(at(29, 16, 22), NOW), 'Oct 29 16:22');
    assert.strictEqual(act.fmtWhen('nonsense', NOW), '');
  }

  // sourceFilterInURL
  {
    const LIST = ['Nodes', 'Devices', 'Funnel', 'Tasks', 'Policy'];
    assert.strictEqual(act.sourceOf('Funnel', LIST), 'Funnel');
    assert.strictEqual(act.sourceOf('All', LIST), 'All');
    assert.strictEqual(act.sourceOf('Bogus', LIST), 'All');
    assert.strictEqual(act.sourceOf(null, LIST), 'All');
    assert.strictEqual(act.sourceOf('Funnel'), 'Funnel', 'taken provisionally until the nav answers');
    const calls = [], params = [];
    const e = els();
    const ctl = act.create(e, { now: () => NOW, sources: LIST, getParam: () => 'Devices', setParam: (k, v) => params.push([k, v]),
      api: async (m, u) => { calls.push(m + ' ' + u); return { entries: [] }; } });
    assert.strictEqual(ctl.state.source, 'Devices');
    await ctl.refresh();
    assert.deepStrictEqual(calls, ['GET /_activity.json?limit=200&source=Devices']);
    const on = ctl.state.chips.filter((c) => c.el.attrs['aria-pressed'] === 'true').map((c) => c.name);
    assert.deepStrictEqual(on, ['Devices']);
    assert.deepStrictEqual(ctl.state.chips.map((c) => c.name), ['All', 'Nodes', 'Devices', 'Funnel', 'Tasks', 'Policy']);
    await ctl.pick('Policy');
    assert.deepStrictEqual(params, [['source', 'Policy']]);
    assert.strictEqual(calls[1], 'GET /_activity.json?limit=200&source=Policy');
    await ctl.pick('All');
    assert.deepStrictEqual(params[1], ['source', '']);
    assert.strictEqual(calls[2], 'GET /_activity.json?limit=200');
    // a slow answer for a source no longer chosen is dropped
    let release;
    const gate = new Promise((r) => { release = r; });
    let n = 0;
    const slow = act.create(els(), { now: () => NOW, sources: LIST, getParam: () => null, setParam() {},
      api: async () => { n += 1; if (n === 1) { await gate; return { entries: [{ at: at(31, 9, 0), actor: 'a', source: 'Nodes', action: 'x', detail: '' }] }; } return { entries: [] }; } });
    const first = slow.refresh();
    await slow.pick('Tasks');
    release(); await first;
    assert.strictEqual(slow.state.data.entries.length, 0);
  }

  // chips only for what would match something
  {
    const nav = [{ label: 'Overview', path: '/' }, { label: 'Nodes', path: '/nodes' }, { label: 'Devices', path: '/enroll' }, { label: 'Policy', path: '/policy' }, { label: 'Activity', path: '/activity' }];
    assert.deepStrictEqual(act.sourcesFromNav(nav), ['Nodes', 'Devices', 'Policy']);
    assert.deepStrictEqual(act.sourcesFromNav(null), []);
    const params = [];
    let want = 'Funnel'; // a URL naming a source this console does not have
    const e = els();
    const ctl = act.create(e, { now: () => NOW, getParam: () => want, setParam: (k, v) => params.push([k, v]), api: async () => ({ entries: [] }) });
    assert.strictEqual(ctl.state.source, 'Funnel');
    await ctl.setSources(act.sourcesFromNav(nav));
    assert.strictEqual(ctl.state.source, 'All');
    assert.deepStrictEqual(params, [['source', '']]);
    assert.deepStrictEqual(ctl.state.chips.map((c) => c.name), ['All', 'Nodes', 'Devices', 'Policy']);
    // no nav: chips come from the entries seen
    const f = els();
    const c2 = act.create(f, { now: () => NOW, getParam: () => null, setParam() {}, api: async () => ({ entries: [{ at: at(31, 1, 0), actor: 'a', source: 'Tasks', action: 'x', detail: '' }] }) });
    c2.render();
    assert.deepStrictEqual(c2.state.chips.map((c) => c.name), ['All']);
    await c2.refresh();
    assert.deepStrictEqual(c2.state.chips.map((c) => c.name), ['All', 'Tasks']);
  }

  // rows
  {
    const e = els();
    const ctl = act.create(e, { now: () => NOW, getParam: () => null, setParam() {},
      api: async () => ({ entries: [
        { at: at(31, 14, 31), actor: 'operator', source: 'Policy', action: 'change.applied', detail: '48: add namespace demo' },
        { at: at(30, 18, 40), actor: '', source: 'Tasks', action: 'task.manual', detail: 'login-check: succeeded' },
      ] }) });
    await ctl.refresh();
    const trs = find(e.rows, (x) => x.tag === 'tr');
    assert.deepStrictEqual(rowText(trs[0]), ['Today 14:31', 'operator', 'Policy', 'Change #48 applied: add namespace demo']);
    assert.deepStrictEqual(rowText(trs[1]), ['Yesterday 18:40', '-', 'Tasks', 'login-check run by hand']);
    assert.strictEqual(e.table.hidden, false);
    assert.strictEqual(e.empty.hidden, true);
    assert.strictEqual(e.notice.hidden, true);
  }

  // text is text
  {
    const e = els();
    const hostile = '<img src=x onerror=alert(1)>';
    const ctl = act.create(e, { now: () => NOW, getParam: () => null, setParam() {},
      api: async () => ({ entries: [{ at: at(31, 1, 0), actor: hostile, source: hostile, action: hostile, detail: hostile }] }) });
    await ctl.refresh();
    const tds = find(e.rows, (x) => x.tag === 'td');
    assert.strictEqual(tds[1].textContent, hostile);
    assert.ok(tds[3].textContent.includes(hostile));
    assert.strictEqual(find(e.rows, (x) => x.tag === 'img').length, 0);
  }

  // partialNotice
  {
    assert.deepStrictEqual(act.notices({ partial: false, hidden_systems: false }), []);
    assert.deepStrictEqual(act.notices({ partial: true }), ['Some sources did not answer, so this list may be incomplete.']);
    assert.strictEqual(act.notices({ partial: true, hidden_systems: true }).length, 2);
    const e = els();
    const ctl = act.create(e, { now: () => NOW, getParam: () => null, setParam() {},
      api: async () => ({ entries: [{ at: at(31, 1, 0), actor: 'a', source: 'Nodes', action: 'x', detail: '' }], partial: true, hidden_systems: true }) });
    await ctl.refresh();
    assert.strictEqual(e.notice.hidden, false);
    assert.ok(/did not answer/.test(e.notice.textContent) && /do not have access/.test(e.notice.textContent));
  }

  // emptyState
  {
    const e = els();
    const ctl = act.create(e, { now: () => NOW, sources: ['Funnel'], getParam: () => null, setParam() {}, api: async () => ({ entries: [], partial: false }) });
    ctl.render();
    assert.strictEqual(e.empty.hidden, true, 'nothing is claimed before the first answer');
    await ctl.refresh();
    assert.strictEqual(e.empty.hidden, false);
    assert.strictEqual(e.table.hidden, true);
    assert.strictEqual(e.emptyhead.textContent, 'No activity yet');
    await ctl.pick('Funnel');
    assert.strictEqual(e.emptyhead.textContent, 'No activity from Funnel');
    assert.ok(/Choose All/.test(e.emptytext.textContent));
  }

  // a failed read keeps the rows and says so
  {
    const e = els();
    let fail = false;
    const ctl = act.create(e, { now: () => NOW, getParam: () => null, setParam() {},
      api: async () => { if (fail) throw new Error('down'); return { entries: [{ at: at(31, 1, 0), actor: 'a', source: 'Nodes', action: 'x', detail: '' }] }; } });
    await ctl.refresh();
    fail = true;
    await ctl.refresh();
    assert.strictEqual(e.error.hidden, false);
    assert.ok(/Showing what was loaded before/.test(e.error.textContent));
    assert.strictEqual(find(e.rows, (x) => x.tag === 'tr').length, 1);
    const f = els();
    await act.create(f, { now: () => NOW, getParam: () => null, setParam() {}, api: async () => { throw new Error('down'); } }).refresh();
    assert.ok(/Could not load/.test(f.error.textContent));
    assert.strictEqual(f.empty.hidden, true);
  }
  console.log('activity self-test ok');
})().catch((e) => { console.error(e); process.exit(1); });
