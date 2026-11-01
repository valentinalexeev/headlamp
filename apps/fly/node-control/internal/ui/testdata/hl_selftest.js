// Run with: node hl_selftest.js <path to hl.js>. Exits non-zero on a failed check.
const assert = require('assert');
const path = require('path');

// A tiny DOM stand-in, enough for el(), icon() and confirmPanel().
function node(tag) {
  return {
    tag, children: [], className: '', textContent: '', attrs: {}, listeners: {}, focused: false,
    appendChild(c) { this.children.push(c); return c; },
    setAttribute(k, v) { this.attrs[k] = String(v); },
    addEventListener(t, f) { this.listeners[t] = f; },
    focus() { this.focused = true; },
    remove() { this.removed = true; },
    get firstChild() { return this.children[0] || null; },
    removeChild(c) { this.children.splice(this.children.indexOf(c), 1); },
  };
}
global.document = { createElement: node, createElementNS: (ns, tag) => node(tag) };
global.window = {};
require(path.resolve(process.argv[2]));
const hl = global.window.hl;
assert.ok(hl, 'window.hl is defined');
for (const k of ['el', 'icon', 'relTime', 'fmtBytes', 'plain', 'api', 'poll', 'status', 'queryParam', 'setQueryParam', 'confirmPanel']) {
  assert.strictEqual(typeof hl[k], 'function', k);
}

const now = Date.parse('2026-01-01T12:00:00Z');
assert.strictEqual(hl.relTime('2026-01-01T11:55:00Z', now), '5 min ago');
assert.strictEqual(hl.relTime('2026-01-01T12:03:00Z', now), 'in 3 min');
assert.strictEqual(hl.relTime('2026-01-01T12:00:01Z', now), 'just now');
assert.strictEqual(hl.relTime('2025-12-30T10:00:00Z', now), '2 d 2 h ago');
assert.strictEqual(hl.relTime('', now), '');
assert.strictEqual(hl.relTime('garbage', now), '');

assert.strictEqual(hl.fmtBytes(0), '0 B');
assert.strictEqual(hl.fmtBytes(1023), '1023 B');
assert.strictEqual(hl.fmtBytes(1536), '1.5 KB');
assert.strictEqual(hl.fmtBytes(5 * 1024 * 1024), '5.0 MB');
assert.strictEqual(hl.fmtBytes(3 * 1024 ** 3), '3.0 GB');
assert.strictEqual(hl.fmtBytes(null), '');
assert.strictEqual(hl.fmtBytes(-1), '');

assert.strictEqual(hl.plain('a\u202eb\u0000c', 63), 'abc');
assert.strictEqual(hl.plain('a\u200bb\u2066c\ufeffd', 63), 'abcd');
assert.strictEqual(hl.plain('abcdef', 3), 'abc');
assert.strictEqual(hl.plain(null, 5), '');
assert.strictEqual(hl.plain(42, 5), '42');
assert.strictEqual(hl.plain('<b>x</b>', 63), '<b>x</b>');

const e = hl.el('span', 'cls', '<img src=x onerror=alert(1)>');
assert.strictEqual(e.tag, 'span');
assert.strictEqual(e.className, 'cls');
assert.strictEqual(e.textContent, '<img src=x onerror=alert(1)>');
assert.strictEqual(hl.el('div').textContent, '');

const st = hl.status('crit', 'Offline');
assert.strictEqual(st.className, 'st crit');
assert.strictEqual(st.children.length, 2);
assert.strictEqual(st.children[1].textContent, 'Offline');
assert.strictEqual(hl.status('bogus', 'x').className, 'st ok');

// confirmPanel: names the object and the consequence, the destructive button is never focused first.
let confirmed = 0, cancelled = 0;
const panel = hl.confirmPanel({
  title: 'Revoke key', object: 'key <b>a</b>', consequence: 'Devices using it lose access.',
  confirmLabel: 'Revoke', onConfirm: () => confirmed++, onCancel: () => cancelled++,
});
const all = [];
(function walk(n) { all.push(n); n.children.forEach(walk); })(panel);
const text = all.map((n) => n.textContent).join('|');
assert.ok(text.includes('key <b>a</b>') && text.includes('Devices using it lose access.'));
const buttons = all.filter((n) => n.tag === 'button');
assert.strictEqual(buttons.length, 2);
const destructive = buttons.find((b) => b.textContent === 'Revoke' || b.children.some((c) => c.textContent === 'Revoke'));
const cancel = buttons.find((b) => b !== destructive);
assert.ok(destructive && cancel);
assert.ok(cancel.focused, 'cancel is focused first');
assert.ok(!destructive.focused, 'the destructive button is never focused first');
assert.ok(!/\bprimary\b/.test(destructive.className) || /danger/.test(destructive.className));
destructive.listeners.click();
assert.strictEqual(confirmed, 1);
cancel.listeners.click();
assert.strictEqual(cancelled, 1);
assert.ok(panel.removed);

console.log('ok');
