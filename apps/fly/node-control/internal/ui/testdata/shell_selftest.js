// Run with: node shell_selftest.js <path to hl.js> <path to shell.js>. Exits non-zero on a failed check.
const assert = require('assert');
const path = require('path');

// A small DOM stand-in: elements with children, attributes, listeners and the few properties the shell uses.
function node(tag) {
  return {
    tag, children: [], className: '', textContent: '', attrs: {}, listeners: {}, hidden: false, disabled: false,
    appendChild(c) { this.children.push(c); return c; },
    removeChild(c) { this.children.splice(this.children.indexOf(c), 1); },
    get firstChild() { return this.children[0] || null; },
    setAttribute(k, v) { this.attrs[k] = String(v); },
    getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; },
    removeAttribute(k) { delete this.attrs[k]; },
    addEventListener(t, f) { this.listeners[t] = f; },
    click() { this.listeners.click(); },
  };
}
function text(n) { return n.textContent + n.children.map(text).join(' '); }
function find(n, pred, out = []) { if (pred(n)) out.push(n); n.children.forEach((c) => find(c, pred, out)); return out; }
function buttons(n) { return find(n, (x) => x.tag === 'button').map((b) => text(b).trim()); }

const root = node('html');
global.document = { createElement: node, createElementNS: (ns, tag) => node(tag), documentElement: root, querySelector: () => null, querySelectorAll: () => [], hidden: false };
let store = new Map();
let storageThrows = false;
global.window = {
  addEventListener() {},
  get localStorage() {
    if (storageThrows) { throw new Error('denied'); }
    return { getItem: (k) => (store.has(k) ? store.get(k) : null), setItem: (k, v) => store.set(k, v), removeItem: (k) => store.delete(k) };
  },
};
global.setInterval = () => 0;
require(path.resolve(process.argv[2]));
require(path.resolve(process.argv[3]));
const shell = global.window.hl.shell;
assert.ok(shell, 'hl.shell is defined');

const NOW = Date.parse('2026-01-01T12:00:00Z');
const iso = (s) => new Date(NOW + s * 1000).toISOString();
const err = (status, msg) => Object.assign(new Error(msg || 'failed'), { status });

function parts() {
  return { strip: node('div'), fresh: node('span'), top: Object.assign(node('div'), { hidden: true }), lamp: node('span'), counts: [] };
}
function ctl(p, api, extra) { return shell.controller(p, Object.assign({ api, now: () => NOW, reload: () => {} }, extra)); }

(async () => {
  // themeChoicePersists
  {
    shell.pickTheme('light');
    assert.strictEqual(root.getAttribute('data-hl-theme'), 'light');
    assert.strictEqual(store.get('hl-theme'), 'light');
    shell.pickTheme('dark');
    assert.strictEqual(root.getAttribute('data-hl-theme'), 'dark');
    assert.strictEqual(store.get('hl-theme'), 'dark');
    shell.pickTheme('system');
    assert.strictEqual(root.getAttribute('data-hl-theme'), null);
    assert.ok(!store.has('hl-theme'));
    shell.pickTheme('nonsense');
    assert.strictEqual(root.getAttribute('data-hl-theme'), null);
    storageThrows = true;
    assert.doesNotThrow(() => shell.pickTheme('light'));
    assert.strictEqual(root.getAttribute('data-hl-theme'), 'light');
    assert.doesNotThrow(() => shell.pickTheme('system'));
    assert.strictEqual(root.getAttribute('data-hl-theme'), null);
    storageThrows = false;
    const b = ['system', 'light', 'dark'].map((t) => { const x = node('button'); x.setAttribute('data-theme', t); return x; });
    shell.pickTheme('dark');
    shell.syncThemeButtons(b);
    assert.deepStrictEqual(b.map((x) => x.getAttribute('aria-pressed')), ['false', 'false', 'true']);
    assert.deepStrictEqual(b.map((x) => x.className), ['', '', 'on']);
    shell.pickTheme('system');
  }

  // stripShowsOnlyTimedItems
  {
    const p = parts();
    const data = { items: [
      { id: 'a', source: 'nodes', level: 'critical', title: 'Exit offline' },
      { id: 'b', source: 'policy', level: 'timed', title: 'Policy change #48 is pending', deadline: iso(252), change_id: 48, applied_at: iso(-10) },
      { id: 'c', source: 'tasks', level: 'waiting', title: 'Check failing' },
      { id: 'd', source: 'enroll', level: 'info', title: 'Note' },
    ] };
    const c = ctl(p, async () => data);
    await c.poll();
    const t = text(p.strip);
    assert.strictEqual(p.strip.hidden, false);
    assert.ok(t.includes('Policy change #48 is pending'), t);
    assert.ok(t.includes('04:12'), t);
    for (const other of ['Exit offline', 'Check failing', 'Note']) { assert.ok(!t.includes(other), other); }
    assert.strictEqual(shell.timedItems(data).length, 1);
    // No timed item: the strip is empty and hidden.
    const q = parts();
    await ctl(q, async () => ({ items: [data.items[0]] })).poll();
    assert.strictEqual(q.strip.hidden, true);
    assert.strictEqual(q.strip.children.length, 0);
    assert.strictEqual(shell.countdown(iso(-5), NOW), '00:00');
    assert.strictEqual(shell.countdown(iso(3725), NOW), '62:05');
  }

  // confirmNeedsFreshPage
  {
    const applied = '2026-01-01T12:00:00Z';
    const appliedSec = Date.parse(applied) / 1000;
    const item = { level: 'timed', source: 'policy', title: 'Policy change #48 is pending', change_id: 48, applied_at: applied, deadline: iso(100) };
    for (const [loaded, want] of [[appliedSec - 1, 'Reload to confirm'], [appliedSec, 'Confirm'], [appliedSec + 30, 'Confirm']]) {
      const p = parts();
      p.strip.setAttribute('data-hl-loaded', String(loaded));
      await ctl(p, async () => ({ items: [item] })).poll();
      const names = buttons(p.strip);
      assert.deepStrictEqual(names, [want, 'Revert now'], 'loaded ' + (loaded - appliedSec));
    }
    // Confirm and Revert now post the change id; Reload to confirm only reloads.
    const calls = [];
    let reloads = 0;
    const p = parts();
    p.strip.setAttribute('data-hl-loaded', String(appliedSec));
    const c = ctl(p, async (m, u, b) => { calls.push([m, u, b]); return u === '/_attention.json' ? { items: [item] } : null; }, { reload: () => { reloads++; } });
    await c.poll();
    find(p.strip, (x) => x.tag === 'button' && text(x).includes('Revert now'))[0].click();
    await new Promise((r) => setImmediate(r));
    assert.deepStrictEqual(calls.filter((x) => x[1] !== '/_attention.json'), [['POST', '/policy/revert-now', { change_id: 48 }]]);
    const q = parts();
    q.strip.setAttribute('data-hl-loaded', String(appliedSec - 5));
    const c2 = ctl(q, async () => ({ items: [item] }), { reload: () => { reloads++; } });
    await c2.poll();
    find(q.strip, (x) => x.tag === 'button' && text(x).includes('Reload'))[0].click();
    assert.strictEqual(reloads, 1);
    // A failed action shows its message in the strip and offers the buttons again.
    const r = parts();
    r.strip.setAttribute('data-hl-loaded', String(appliedSec));
    const c3 = ctl(r, async (m, u) => { if (u === '/_attention.json') return { items: [item] }; throw err(409, 'the policy changed'); });
    await c3.poll();
    find(r.strip, (x) => x.tag === 'button' && text(x).trim() === 'Confirm')[0].click();
    await new Promise((x) => setImmediate(x));
    assert.ok(text(r.strip).includes('the policy changed'));
    assert.deepStrictEqual(buttons(r.strip), ['Confirm', 'Revert now']);
    assert.ok(find(r.strip, (x) => x.tag === 'button').every((b) => !b.disabled));
  }

  // staleAfterFailedPoll
  {
    const p = parts();
    const entry = { source: 'nodes', el: node('span') };
    p.counts = [entry];
    let mode = 'ok';
    const c = ctl(p, async () => {
      if (mode === 'ok') { return { items: [{ source: 'nodes', level: 'critical', title: 'x' }] }; }
      throw err(mode === '404' ? 404 : 500);
    });
    await c.poll();
    assert.ok(text(p.fresh).startsWith('Updated'), text(p.fresh));
    assert.strictEqual(entry.el.textContent, '1');
    mode = '500';
    await c.poll();
    assert.ok(!text(p.fresh).includes('stale'), 'one failed poll is not stale yet');
    await c.poll();
    assert.ok(text(p.fresh).includes('Data may be stale'), text(p.fresh));
    assert.strictEqual(entry.el.textContent, '1', 'last counts are kept');
    mode = 'ok';
    await c.poll();
    assert.ok(!text(p.fresh).includes('stale'));
    // A 404 (no attention endpoint yet) is quiet: nothing shown, nothing stale.
    const q = parts();
    const c2 = ctl(q, async () => { throw err(404); });
    for (let i = 0; i < 4; i++) { await c2.poll(); }
    assert.strictEqual(text(q.fresh), '');
    assert.strictEqual(q.strip.hidden === true || q.strip.children.length === 0, true);
    assert.strictEqual(c2.state.fails, 0);
  }

  // sidebarCountsBySource
  {
    const mk = (s) => ({ source: s, el: node('span') });
    const [nodes, policy, tasks, none] = ['nodes', 'policy', 'tasks', 'enroll'].map(mk);
    shell.applyCounts([nodes, policy, tasks, none], { items: [
      { source: 'nodes', level: 'critical' }, { source: 'nodes', level: 'waiting' },
      { source: 'policy', level: 'timed' }, { source: 'tasks', level: 'waiting' }, { source: 'enroll', level: 'info' },
    ] });
    assert.strictEqual(nodes.el.textContent, '2');
    assert.strictEqual(nodes.el.className, 'navcount crit');
    assert.strictEqual(policy.el.className, 'navcount');
    assert.strictEqual(tasks.el.textContent, '1');
    assert.strictEqual(tasks.el.className, 'navcount');
    assert.strictEqual(none.el.hidden, true);
    assert.strictEqual(none.el.textContent, '');
    assert.strictEqual(Object.keys(shell.countsBySource({ items: [{ source: '__proto__', level: 'waiting' }] })).length, 1);
    assert.strictEqual(({}).waiting, undefined);
  }
  console.log('shell self-test ok');
})().catch((e) => { console.error(e); process.exit(1); });
