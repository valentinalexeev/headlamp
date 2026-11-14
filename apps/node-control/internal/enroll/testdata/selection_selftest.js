// Run with: node selection_selftest.js <hl.js> <selection.js>. Exits non-zero on a failed check.
const assert = require('assert');
const path = require('path');

// A just-enough DOM: elements keep children and text, record listeners, and refuse innerHTML.
class FakeNode {
  constructor(tag) {
    this.tag = tag; this.children = []; this.parentNode = null; this.attrs = {}; this.listeners = {};
    this.className = ''; this._text = ''; this.hidden = false; this.disabled = false; this.value = ''; this.checked = false;
  }
  get textContent() { return this._text + this.children.map((c) => c.textContent).join(''); }
  set textContent(v) { this._text = String(v); this.children = []; }
  get firstChild() { return this.children[0] || null; }
  appendChild(c) { if (c.parentNode) { c.parentNode.removeChild(c); } c.parentNode = this; this.children.push(c); return c; }
  removeChild(c) { const i = this.children.indexOf(c); if (i >= 0) { this.children.splice(i, 1); } c.parentNode = null; return c; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  addEventListener(t, f) { (this.listeners[t] = this.listeners[t] || []).push(f); }
  set innerHTML(v) { throw new Error('innerHTML must not be used'); }
  click() { if (this.disabled) { return; } fire(this, 'click'); }
}
const fire = (n, type) => (n.listeners[type] || []).forEach((f) => f({ preventDefault() {}, target: n }));
const walk = (n, out = []) => { out.push(n); n.children.forEach((c) => walk(c, out)); return out; };
const findAll = (root, pred) => walk(root).filter(pred);
const buttonNamed = (root, text) => findAll(root, (n) => n.tag === 'button' && n.textContent === text)[0];
const byAttr = (root, k, v) => findAll(root, (n) => n.getAttribute(k) === v)[0];

global.document = {
  createElement: (t) => new FakeNode(t), createElementNS: (_, t) => new FakeNode(t),
  createTextNode: (s) => { const n = new FakeNode('#text'); n._text = s; return n; },
  getElementById: () => null, addEventListener() {}, activeElement: null,
};
global.window = { location: { search: '', href: 'http://localhost/enroll/selection' }, history: { replaceState() {} } };
global.setInterval = () => 0;
const fetches = [];
let fetchReply = () => ({ ok: true, status: 200, body: '{}' });
global.fetch = (url, opts) => {
  fetches.push({ url, method: (opts && opts.method) || 'GET', body: opts && opts.body });
  const r = fetchReply(url, opts);
  return Promise.resolve({ ok: r.ok, status: r.status, text: () => Promise.resolve(r.body) });
};
const json = (v, status) => ({ ok: (status || 200) < 400, status: status || 200, body: JSON.stringify(v) });
const flush = async () => { for (let i = 0; i < 6; i++) { await new Promise((r) => setImmediate(r)); } };

require(path.resolve(process.argv[2]));
require(path.resolve(process.argv[3]));
const sel = global.window.hl.selection;
assert.ok(sel, 'hl.selection is defined');

const v = (value, source) => ({ value, source });
const state = (over) => Object.assign({
  effective: {
    tolerance: v(120, 'file'), interval: v('90s', 'file'), url: v('https://probe.example.com/204', 'file'), strategy: v('url-test', 'default'),
    order_source: 'file',
    bridges: [
      { name: 'ingress-1', group: 'group-a', group_source: 'file', file_group: 'group-a', drained: false },
      { name: 'ingress-2', group: '', group_source: 'default', file_group: '', drained: false },
    ],
  },
  override: {}, can_revert: false, by: '', at: 0,
}, over);

function els() {
  const mk = (tag) => new FakeNode(tag);
  const e = { sub: mk('p'), error: mk('div'), notice: mk('div'), tuning: mk('div'), bridges: mk('div'), actions: mk('div'), preview: mk('section'), previewbody: mk('div') };
  e.error.hidden = e.notice.hidden = e.preview.hidden = true;
  return e;
}

(async () => {
  // overrideOf sends only the fields that are set
  {
    assert.deepStrictEqual(sel.overrideOf(sel.draftOf(state())), {});
    const d = sel.draftOf(state({ override: { tolerance: 0, drain: ['ingress-2'], order: ['ingress-2'] } }));
    assert.deepStrictEqual(sel.overrideOf(d), { tolerance: 0, order: ['ingress-1', 'ingress-2'], drain: ['ingress-2'] });
  }

  // loadShowsSources
  {
    fetches.length = 0;
    fetchReply = () => json(state());
    const e = els();
    const ctrl = sel.mount(e);
    await ctrl.load();
    assert.deepStrictEqual(fetches.map((f) => f.method + ' ' + f.url), ['GET /enroll/selection/state']);
    const text = e.tuning.textContent;
    assert.ok(/In use: 120 \(source: enroll.json\)/.test(text), text);
    assert.ok(/In use: url-test \(source: default\)/.test(text), text);
    assert.ok(/source: enroll.json/.test(e.bridges.textContent));
    assert.ok(/source: default/.test(e.bridges.textContent));
    assert.strictEqual(buttonNamed(e.actions, 'Apply').disabled, true, 'apply needs a preview');
    assert.strictEqual(buttonNamed(e.actions, 'Revert last change').disabled, true, 'nothing to revert');
  }

  // changePreviewApplyRevert: the acceptance path
  {
    fetches.length = 0;
    const applied = state({ override: { tolerance: 300 }, can_revert: true, by: 'admin@',
      effective: Object.assign({}, state().effective, { tolerance: v(300, 'ui') }) });
    fetchReply = (url) => {
      if (url === '/enroll/selection/preview') { return json({ changes: ['tolerance: 120 (file) -> 300 (ui)'], check: 'ok', singbox: [{ type: 'urltest', tag: 'auto' }], clash: [] }); }
      if (url === '/enroll/selection/apply') { return json(applied); }
      if (url === '/enroll/selection/revert') { return json(state()); }
      return json(state());
    };
    const e = els();
    const ctrl = sel.mount(e);
    await ctrl.load();
    const tol = byAttr(e.tuning, 'data-key', 'tolerance');
    tol.value = '300'; fire(tol, 'input');
    buttonNamed(e.actions, 'Preview').click();
    await flush();
    const pv = fetches.filter((f) => f.url === '/enroll/selection/preview');
    assert.strictEqual(pv.length, 1);
    assert.deepStrictEqual(JSON.parse(pv[0].body), { override: { tolerance: 300 } });
    assert.strictEqual(e.preview.hidden, false);
    assert.ok(/tolerance: 120 \(file\) -> 300 \(ui\)/.test(e.previewbody.textContent));
    assert.ok(/sing-box check: ok/.test(e.previewbody.textContent));
    assert.ok(/"urltest"/.test(e.previewbody.textContent));
    // a change after the preview needs a new preview
    tol.value = '301'; fire(tol, 'input');
    assert.strictEqual(buttonNamed(e.actions, 'Apply').disabled, true);
    tol.value = '300'; fire(tol, 'input');
    buttonNamed(e.actions, 'Preview').click();
    await flush();
    assert.strictEqual(buttonNamed(e.actions, 'Apply').disabled, false);
    buttonNamed(e.actions, 'Apply').click();
    await flush();
    const ap = fetches.filter((f) => f.url === '/enroll/selection/apply');
    assert.strictEqual(ap.length, 1);
    assert.deepStrictEqual(JSON.parse(ap[0].body), { override: { tolerance: 300 } });
    assert.ok(/Applied/.test(e.notice.textContent));
    assert.ok(/In use: 300 \(source: console\)/.test(e.tuning.textContent));
    assert.ok(/Last change by admin@/.test(e.sub.textContent));
    assert.strictEqual(buttonNamed(e.actions, 'Revert last change').disabled, false);
    buttonNamed(e.actions, 'Revert last change').click();
    await flush();
    assert.strictEqual(fetches.filter((f) => f.url === '/enroll/selection/revert' && f.method === 'POST').length, 1);
    assert.ok(/Reverted/.test(e.notice.textContent));
    assert.ok(/In use: 120 \(source: enroll.json\)/.test(e.tuning.textContent));
  }

  // orderGroupDrain
  {
    fetches.length = 0;
    fetchReply = (url) => (url === '/enroll/selection/preview' ? json({ changes: [], check: 'skipped', singbox: [], clash: [] }) : json(state()));
    const e = els();
    const ctrl = sel.mount(e);
    await ctrl.load();
    findAll(e.bridges, (n) => n.tag === 'button' && n.textContent === 'Down')[0].click();
    const g = byAttr(e.bridges, 'data-group', 'ingress-2');
    g.value = 'group-b'; fire(g, 'input');
    const cb = byAttr(e.bridges, 'data-drain', 'ingress-1');
    cb.checked = true; fire(cb, 'change');
    buttonNamed(e.actions, 'Preview').click();
    await flush();
    const body = JSON.parse(fetches.filter((f) => f.url === '/enroll/selection/preview')[0].body);
    assert.deepStrictEqual(body, { override: { groups: { 'ingress-2': 'group-b' }, order: ['ingress-2', 'ingress-1'], drain: ['ingress-1'] } });
    assert.ok(/No change/.test(e.previewbody.textContent));
    assert.ok(/Order source: console/.test(e.bridges.textContent));
  }

  // refusalShowsTheReason
  {
    fetchReply = (url) => (url === '/enroll/selection/preview'
      ? json({ detail: [{ type: 'value_error', loc: ['body', 'override'], msg: 'urltest.tolerance must be an integer 0-1000 (ms)' }] }, 422)
      : json(state()));
    const e = els();
    const ctrl = sel.mount(e);
    await ctrl.load();
    buttonNamed(e.actions, 'Preview').click();
    await flush();
    assert.strictEqual(e.error.hidden, false);
    assert.ok(/tolerance must be an integer/.test(e.error.textContent), e.error.textContent);
    assert.strictEqual(buttonNamed(e.actions, 'Apply').disabled, true);
  }

  // network text goes in as text only
  {
    fetchReply = () => json(state({ by: '<img src=x onerror=alert(1)>' }));
    const e = els();
    await sel.mount(e).load();
    assert.ok(e.sub.textContent.includes('<img'), 'shown as text');
  }

  console.log('selection selftest ok');
})().catch((e) => { console.error(e); process.exit(1); });
