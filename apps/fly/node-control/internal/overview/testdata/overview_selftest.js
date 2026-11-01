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
console.log('ok');
