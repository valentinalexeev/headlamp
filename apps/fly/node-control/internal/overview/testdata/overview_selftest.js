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

// acknowledged items ask nothing of anyone
{
  const ack = (level) => item(level, { acknowledged: { by: 'alice', at: new Date(NOW - 7200000).toISOString() } });
  let m = ov.model({ items: [ack('critical'), ack('waiting'), item('waiting')], systems: [] }, { now: NOW });
  assert.strictEqual(m.heading, '1 item needs you');
  assert.strictEqual(m.risk, 0);
  assert.strictEqual(m.sub, '');
  assert.strictEqual(m.acked, 2);
  m = ov.model({ items: [ack('critical')], systems: [line('ok')] }, { now: NOW });
  assert.strictEqual(m.kind, 'clear', 'an acknowledged item never blocks All clear');
  assert.strictEqual(m.heading, 'All clear');
  // an unknown system still does
  assert.strictEqual(ov.model({ items: [ack('critical')], systems: [line('unknown')] }, { now: NOW }).kind, 'unknown');

  const split = ov.splitItems([ack('critical'), item('waiting'), null, ack('info')]);
  assert.strictEqual(split.active.length, 1);
  assert.strictEqual(split.acked.length, 2);

  assert.strictEqual(ov.ackedText(ack('critical'), NOW), 'At risk. Acknowledged by alice 2 h 0 min ago, until it changes.');
  const until = item('waiting', { acknowledged: { by: 'bob\u202e', at: new Date(NOW - 60000).toISOString(), until: new Date(NOW + 86400000).toISOString() } });
  assert.strictEqual(ov.ackedText(until, NOW), 'Waiting. Acknowledged by bob 1 min ago, ends in 1 d 0 h.');

  // the choices the page offers, and a row offers them only when the server said it can be acknowledged
  assert.deepStrictEqual(ov.ACK_CHOICES.map((c) => [c.label, c.seconds]), [['Until it changes', 0], ['1 day', 86400], ['7 days', 604800]]);
  assert.strictEqual(ov.rowModel(item('waiting', { ack_token: 'abc' }), NOW).ackToken, 'abc');
  assert.strictEqual(ov.rowModel(item('waiting'), NOW).ackToken, '');
  assert.strictEqual(ov.rowModel(item('timed', { ack_token: 'abc' }), NOW).ackToken, '', 'never for what counts down');
}

// the Activity sentences for acknowledging
assert.strictEqual(ov.describe({ action: 'attention.acknowledge', detail: 'bridge-2 is out of sync' }), 'Acknowledged: bridge-2 is out of sync');
assert.strictEqual(ov.describe({ action: 'attention.unacknowledge', detail: 'bridge-2 is out of sync' }), 'Acknowledgement undone: bridge-2 is out of sync');

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

// The page itself, on a small DOM: where focus goes after acknowledging and undoing, and that the shell re-reads.
(async () => {
  let active = null;
  class El {
    constructor(tag) { this.tag = tag; this.children = []; this.parentNode = null; this.attrs = {}; this.listeners = {}; this._text = ''; this.hidden = false; this.disabled = false; this.className = ''; }
    get textContent() { return this._text + this.children.map((c) => c.textContent).join(''); }
    set textContent(v) { this._text = String(v); this.children.slice().forEach((c) => this.removeChild(c)); }
    get firstChild() { return this.children[0] || null; }
    get isConnected() { let n = this; while (n.parentNode) { n = n.parentNode; } return n.isRoot === true; }
    appendChild(c) { c.parentNode = this; this.children.push(c); return c; }
    removeChild(c) { this.children.splice(this.children.indexOf(c), 1); c.parentNode = null; return c; }
    setAttribute(k, v) { this.attrs[k] = String(v); }
    getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
    addEventListener(t, f) { this.listeners[t] = f; }
    focus() { if (!this.disabled) { active = this; } }
    click() { if (!this.disabled) { this.listeners.click({ preventDefault() {} }); } }
    querySelector() { return null; }
  }
  const body = new El('body'); body.isRoot = true;
  const ids = {};
  ['overview', 'heading', 'sub', 'glyph', 'queue', 'systems', 'activity', 'activity-rows', 'acked', 'acked-summary', 'acked-rows', 'ack-error'].forEach((id) => {
    ids[id] = new El('div'); body.appendChild(ids[id]);
  });
  ids.queue.parentNode = ids.overview; ids['acked-rows'].parentNode = ids.overview; ids.heading.parentNode = ids.overview;
  ids.overview.children.push(ids.queue, ids['acked-rows'], ids.heading);
  global.document = {
    hidden: false, body, createElement: (t) => new El(t), createElementNS: (_n, t) => new El(t), createTextNode: (t) => { const e = new El('#text'); e._text = String(t); return e; },
    getElementById: (id) => ids[id] || null, addEventListener() {},
    get activeElement() { return active && active.isConnected ? active : body; },
  };
  const hl = global.window.hl;
  const mk = (id, extra) => Object.assign({ id, source: 'nodes', level: 'waiting', title: 'Item ' + id, action_label: 'Open', action_path: '/x', ack_token: 't-' + id }, extra);
  const acked = { by: 'alice', at: new Date(NOW).toISOString() };
  let items = [mk('a'), mk('b'), mk('c')];
  let posts = [];
  let failNext = false;
  let shellReads = 0;
  hl.shell = { refresh() { shellReads++; } };
  hl.api = async (method, url, b) => {
    if (method === 'POST') {
      posts.push([url, b]);
      if (failNext) { failNext = false; throw Object.assign(new Error('no'), { status: 409 }); }
      items = items.map((it) => (it.id === b.id ? Object.assign({}, it, { acknowledged: url.includes('unack') ? undefined : acked }) : it));
      return { status: 'ok' };
    }
    return url.includes('activity') ? { entries: [] } : { items: items.map((x) => x), systems: [] };
  };
  let tick = null;
  hl.poll = (fn) => { tick = fn; return { now: fn }; };
  ov.init();
  const settle = async () => { for (let i = 0; i < 20; i++) { await Promise.resolve(); } await new Promise((r) => setImmediate(r)); };
  await tick();

  const walk = (n, out = []) => { out.push(n); n.children.forEach((c) => walk(c, out)); return out; };
  const key = (k) => walk(ids.overview).find((n) => n.getAttribute('data-fk') === k);
  const named = (root, text) => walk(root).find((n) => n.tag === 'button' && n.textContent === text);
  const fk = () => (active === ids.heading ? 'heading' : active && active.isConnected ? active.getAttribute('data-fk') : 'lost');

  // choosing, cancelling: focus goes in and comes back
  key('ack:a').click();
  assert.strictEqual(fk(), 'choice:a');
  named(ids.queue, 'Cancel').click();
  assert.strictEqual(fk(), 'ack:a');

  // acknowledging moves on to the next item's main control, and the shell re-reads at once
  key('ack:a').click();
  named(ids.queue, 'Until it changes').click();
  await settle();
  assert.strictEqual(fk(), 'main:b');
  assert.strictEqual(shellReads, 1, 'the sidebar counts follow the write');

  // undo returns to that item's Acknowledge button
  key('undo:a').click();
  await settle();
  assert.strictEqual(fk(), 'ack:a');
  assert.strictEqual(shellReads, 2);

  // the last item in the queue moves back to the one before it
  key('ack:c').click();
  named(ids.queue, '7 days').click();
  await settle();
  assert.strictEqual(fk(), 'main:b');
  assert.strictEqual(posts[posts.length - 1][1].expires_in_seconds, 604800);

  // a refused request leaves focus with the choice that failed, and with the Undo that failed
  failNext = true;
  key('ack:b').click();
  named(ids.queue, '1 day').click();
  await settle();
  assert.strictEqual(fk(), 'choice:b');
  assert.strictEqual(ids['ack-error'].textContent, 'no');
  named(ids.queue, 'Cancel').click();
  failNext = true;
  key('undo:c').click();
  await settle();
  assert.strictEqual(fk(), 'undo:c');

  // with nothing left in the queue, the heading; a row without a link goes to its Acknowledge button
  items = [mk('a'), mk('b', { action_label: '', action_path: '' })];
  await tick();
  key('ack:a').click();
  named(ids.queue, 'Until it changes').click();
  await settle();
  assert.strictEqual(fk(), 'ack:b');
  key('ack:b').click();
  named(ids.queue, 'Until it changes').click();
  await settle();
  assert.strictEqual(fk(), 'heading');

  // an ordinary poll that rebuilds the rows keeps focus on the same control
  items = [mk('p')];
  await tick();
  key('main:p').focus();
  items = [mk('p', { title: 'Changed' })];
  await tick();
  assert.strictEqual(fk(), 'main:p');

  // focus the operator has moved elsewhere is left alone
  const other = new El('input'); body.appendChild(other);
  key('ack:p').click();
  other.focus();
  named(ids.queue, 'Until it changes').click();
  other.focus();
  await settle();
  assert.strictEqual(active, other);
})().catch((e) => { console.error(e); process.exit(1); });
