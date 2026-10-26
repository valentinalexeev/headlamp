// Run with: node tasks_page_selftest.js <path to tasks.js>. Exits non-zero on a failed check.
const assert = require('assert');
const h = require(require('path').resolve(process.argv[2]));
const now = Date.parse('2026-01-01T12:00:00Z');

assert.strictEqual(h.relTime('2026-01-01T11:55:00Z', now), '5 min ago');
assert.strictEqual(h.relTime('2026-01-01T12:03:00Z', now), 'in 3 min');
assert.strictEqual(h.relTime('2026-01-01T12:00:01Z', now), 'just now');
assert.strictEqual(h.relTime('2025-12-30T10:00:00Z', now), '2 d 2 h ago');
assert.strictEqual(h.relTime('', now), '');
assert.strictEqual(h.relTime('garbage', now), '');

assert.strictEqual(h.fmtDuration(250), '250 ms');
assert.strictEqual(h.fmtDuration(1500), '1.5 s');
assert.strictEqual(h.fmtDuration(125000), '2 min 5 s');
assert.strictEqual(h.fmtDuration(null), '');

assert.strictEqual(h.fmtValue(12.5), '12.5');
assert.strictEqual(h.fmtValue('<b>x</b>'), '<b>x</b>');
assert.strictEqual(h.fmtValue(null), 'null');
assert.strictEqual(h.fmtValue({ a: 1 }), '{"a":1}');
assert.ok(h.fmtValue('x'.repeat(10) ) === 'xxxxxxxxxx');
assert.ok(h.fmtValue({ a: 'y'.repeat(500) }).length <= 203);

assert.strictEqual(h.errorMessage(409, { error: 'task is already running' }), 'task is already running');
assert.strictEqual(h.errorMessage(409, { detail: 'busy' }), 'busy');
assert.strictEqual(h.errorMessage(500, null), 'request failed (HTTP 500)');
assert.strictEqual(h.errorMessage(500, { error: 5 }), 'request failed (HTTP 500)');

const texts = (t) => h.badgesFor(t).map((b) => b.text);
assert.deepStrictEqual(texts({ running: true, streak: 0, last_success: 'x' }), ['running']);
assert.deepStrictEqual(texts({ running: false, streak: 2, stale: true }), ['failing (2 in a row)', 'stale']);
assert.deepStrictEqual(texts({ streak: 0, last_success: '2026-01-01T00:00:00Z' }), ['ok']);
assert.deepStrictEqual(texts({ streak: 0, last_success: null }), ['no success yet']);

assert.strictEqual(h.targetText({ node: 'n1' }), 'node n1');
assert.strictEqual(h.targetText({ tags: ['tag:a', 'tag:b'], online: true }), 'tags tag:a, tag:b (online only)');
console.log('ok');
