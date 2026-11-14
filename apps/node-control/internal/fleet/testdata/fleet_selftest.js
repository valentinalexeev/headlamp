// Run with: node fleet_selftest.js <path to hl.js> <path to fleet.js>. Exits non-zero on a failed check.
const assert = require('assert');
const path = require('path');

function node(tag) {
  return {
    tag, children: [], className: '', textContent: '', attrs: {}, hidden: false,
    appendChild(c) { this.children.push(c); return c; },
    removeChild(c) { this.children.splice(this.children.indexOf(c), 1); },
    get firstChild() { return this.children[0] || null; },
    setAttribute(k, v) { this.attrs[k] = String(v); },
    get nodeType() { return 1; },
  };
}
function allText(n) { return (n.textContent || '') + n.children.map(allText).join(' '); }

global.document = { createElement: node, createElementNS: (ns, t) => node(t), getElementById: () => null, hidden: false, addEventListener() {} };
global.window = { localStorage: { getItem: () => null, setItem() {}, removeItem() {} } };
require(path.resolve(process.argv[2]));
require(path.resolve(process.argv[3]));
const f = global.window.hlFleet;
assert.ok(f, 'hlFleet is defined');

assert.deepStrictEqual(f.nodeState({ status: 'current', online: true }), ['ok', 'current']);
assert.deepStrictEqual(f.nodeState({ status: 'older', online: true, tags: ['tag:relay'] }), ['crit', 'older than the pin']);
assert.deepStrictEqual(f.nodeState({ status: 'older', online: false }), ['warn', 'older than the pin, offline']);
assert.deepStrictEqual(f.bridgeSync({ active: true, online: false }), ['crit', 'offline']);
assert.deepStrictEqual(f.bridgeSync({ active: true, online: true, sync: 'pending' }), ['crit', 'not in sync']);
assert.deepStrictEqual(f.bridgeSync({ active: true, online: true, sync: 'ok' }), ['ok', 'in sync']);
assert.deepStrictEqual(f.bridgeSync({ active: false }), ['off', 'not active']);
// #300: incompatible outranks the other states
assert.strictEqual(f.bridgeSync({ active: true, online: false, compat: { state: 'incompatible', interface: 'agent-api', reported: 2, hint: 'newer than node-control accepts: upgrade node-control' } })[0], 'crit');
assert.ok(/incompatible: agent-api 2/.test(f.bridgeSync({ active: true, online: true, sync: 'ok', compat: { state: 'incompatible', interface: 'agent-api', reported: 2, hint: 'x' } })[1]));
assert.deepStrictEqual(f.bridgeSync({ active: true, online: true, sync: 'ok', compat: { state: 'unknown' } }), ['ok', 'in sync']);
// #322: under COMPAT_STRICT an unknown part is refused, and that outranks the other states.
assert.deepStrictEqual(f.bridgeSync({ active: true, online: true, sync: 'ok', compat: { state: 'unknown', interface: 'agent-api' } }, true),
  ['crit', 'refused: no agent-api version (before 0.18.0)']);
assert.strictEqual(f.compatWord({ state: 'unknown', interface: 'runner-api' }, false), null);
assert.strictEqual(f.compatWord({ state: 'ok' }, true), null);
assert.strictEqual(f.compatWord({ state: 'ok' }), null);

(async () => {
  const els = {};
  ['error', 'problems', 'notes', 'control', 'bridges', 'bridgesEmpty', 'bridgesTable', 'pin', 'nodes', 'strict'].forEach((k) => { els[k] = node('div'); });
  const data = {
    node_control: 'v0.16.0', headscale: 'v0.29.7', tailscale_pin: '1.104.5',
    nodes: [{ name: 'relay-1', online: true, tailscale: '1.104.5', status: 'current', tags: ['tag:relay'] }],
    bridges: [{ name: 'br-1', source: 'discovered', online: true, active: true, sync: 'ok', agent: '0.16.0+abc', xray: '25.10.15' }],
    funnel: { version: '0.16.0', reported_at: new Date().toISOString(), stale: false, errors: 0 },
    problems: ['<img src=x onerror=alert(1)>'], notes: [],
    runners: [{ address: '100.64.0.8', node: 'runner-1', compat: { interface: 'runner-api', state: 'incompatible', reported: 5, hint: 'newer than node-control accepts: upgrade node-control' } }],
    compat_strict: true,
  };
  const page = f.create(els, { api: () => Promise.resolve(data), now: () => Date.now() });
  await page.load();
  assert.strictEqual(els.error.hidden, true);
  assert.strictEqual(els.problems.hidden, false);
  assert.ok(allText(els.problems).includes('<img'), 'problems are shown as text');
  assert.strictEqual(els.notes.hidden, true);
  assert.strictEqual(els.control.children.length, 4, 'node-control, headscale, funnel, one runner');
  assert.ok(allText(els.control).includes('task runner runner-1') && allText(els.control).includes('incompatible: runner-api 5'));
  assert.strictEqual(els.strict.hidden, false);
  assert.ok(allText(els.control).includes('v0.29.7'));
  assert.strictEqual(els.bridges.children.length, 1);
  assert.ok(allText(els.bridges).includes('25.10.15'));
  assert.strictEqual(els.bridgesEmpty.hidden, true);
  assert.ok(els.pin.textContent.includes('1.104.5'));
  assert.ok(allText(els.nodes).includes('relay-1'));

  const bad = f.create(els, { api: () => Promise.reject(new Error('403 forbidden')) });
  await bad.load();
  assert.strictEqual(els.error.hidden, false);
  assert.ok(els.error.textContent.includes('403'));
  console.log('fleet self-test ok');
})().catch((e) => { console.error(e); process.exit(1); });
