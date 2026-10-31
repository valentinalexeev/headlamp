// Run with: node enroll_selftest.js <path to hl.js> <path to approve.js> [<path to enroll.js>]. Exits non-zero on a failed check.
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
  get isConnected() { let n = this; while (n.parentNode) { n = n.parentNode; } return n === global.document.root || n.isRoot === true; }
  appendChild(c) { if (c.parentNode) { c.parentNode.removeChild(c); } c.parentNode = this; this.children.push(c); return c; }
  removeChild(c) { const i = this.children.indexOf(c); if (i >= 0) { this.children.splice(i, 1); } c.parentNode = null; return c; }
  remove() { if (this.parentNode) { this.parentNode.removeChild(this); } }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  removeAttribute(k) { delete this.attrs[k]; }
  addEventListener(t, f) { (this.listeners[t] = this.listeners[t] || []).push(f); }
  focus() { this.focused = true; global.document.activeElement = this; }
  select() { this.selected = true; }
  set innerHTML(v) { throw new Error('innerHTML must not be used'); }
  set outerHTML(v) { throw new Error('outerHTML must not be used'); }
  click() { if (this.disabled) { return; } fire(this, 'click'); }
}
const fire = (n, type, ev) => (n.listeners[type] || []).forEach((f) => f(Object.assign({ preventDefault() {}, stopPropagation() {}, target: n }, ev)));
const walk = (n, out = []) => { out.push(n); n.children.forEach((c) => walk(c, out)); return out; };
const findAll = (root, pred) => walk(root).filter(pred);
const hasClass = (n, c) => (' ' + n.className + ' ').includes(' ' + c + ' ');
const buttonNamed = (root, text) => findAll(root, (n) => n.tag === 'button' && n.textContent === text)[0];
const textOf = (n) => n.textContent;

const replaced = [];
global.document = {
  createElement: (t) => new FakeNode(t), createElementNS: (_, t) => new FakeNode(t), createTextNode: (s) => { const n = new FakeNode('#text'); n._text = s; return n; },
  getElementById: () => null, addEventListener() {}, hidden: false, activeElement: null,
};
global.window = {
  location: { search: '', href: 'http://localhost/enroll' },
  history: { replaceState(_a, _b, url) { replaced.push(url); global.window.location.search = url.includes('?') ? url.slice(url.indexOf('?')) : ''; global.window.location.href = 'http://localhost' + url; } },
};
global.setInterval = () => 0;
const fetches = [];
let fetchReply = () => ({ ok: true, status: 200, body: '{}' });
global.fetch = (url, opts) => {
  fetches.push({ url, method: (opts && opts.method) || 'GET', body: opts && opts.body });
  const r = fetchReply(url, opts);
  if (r instanceof Error) { return Promise.reject(r); }
  return Promise.resolve({ ok: r.ok, status: r.status, text: () => Promise.resolve(r.body) });
};
const json = (v, status) => ({ ok: (status || 200) < 400, status: status || 200, body: JSON.stringify(v) });
const flush = async () => { for (let i = 0; i < 6; i++) { await new Promise((r) => setImmediate(r)); } };
const sent = (method, urlPart) => fetches.filter((f) => f.method === method && f.url.includes(urlPart));

require(path.resolve(process.argv[2]));
require(path.resolve(process.argv[3]));
const approve = global.window.hl.approve;
assert.ok(approve, 'hl.approve is defined');
if (process.argv[4]) { require(path.resolve(process.argv[4])); }
const devices = global.window.hl.devices;

const bridge = (extra) => Object.assign({
  name: 'bridge-3', host: 'relay3.example.net', source: 'discovered', approval: 'pending', active: false, approved: false,
  fingerprint: '7e3b91c40fa2d86e15b7c3409a1d62ef', public_key_prefix: '9XcT2kQw', short_id_prefix: 'a41f', node_id: 21,
  address: '100.64.0.21', machine_key_prefix: 'mkey:4f1a9c02e7', hostname: 'bridge-3', given_name: 'bridge-3', online: true, sync: null,
}, extra);

function approveEls() {
  const mk = (tag) => new FakeNode(tag);
  const els = { title: mk('h1'), sub: mk('p'), error: mk('div'), changed: mk('section'), info: mk('div'), review: mk('section'), fields: mk('dl'), confirm: mk('div'), actions: mk('div') };
  Object.values(els).forEach((e) => { e.isRoot = true; });
  els.error.hidden = els.changed.hidden = els.info.hidden = els.review.hidden = els.actions.hidden = true;
  return els;
}
const fieldValue = (els, label) => {
  const kids = els.fields.children;
  for (let i = 0; i < kids.length; i += 2) { if (kids[i].textContent === label) { return kids[i + 1].textContent; } }
  return undefined;
};

(async () => {
  // approveShowsAllFields
  {
    fetches.length = 0;
    fetchReply = () => json({ ingresses: [bridge({ name: 'other' }), bridge()], pending_ingresses: ['bridge-3'], discovery: { enabled: true } });
    const els = approveEls();
    const ctrl = approve.mount(els, { go() {} });
    await ctrl.load('bridge-3');
    assert.strictEqual(els.title.textContent, 'Approve bridge-3?');
    assert.ok(/receives device credentials/.test(els.sub.textContent));
    assert.strictEqual(els.review.hidden, false);
    assert.strictEqual(els.changed.hidden, true, 'a first approval is not shown as a changed identity');
    const want = { Node: '21', Name: 'bridge-3', Hostname: 'bridge-3', Address: '100.64.0.21', 'Machine key': 'mkey:4f1a9c02e7…',
      'Public key': '9XcT2kQw…', 'Short ID': 'a41f…', Fingerprint: '7e3b91c40fa2d86e15b7c3409a1d62ef', Host: 'relay3.example.net' };
    Object.keys(want).forEach((k) => assert.strictEqual(fieldValue(els, k), want[k], k));
    assert.strictEqual(els.fields.children.length, 18);
    assert.ok(buttonNamed(els.actions, 'Approve bridge-3'), 'approve button');
    assert.ok(findAll(els.actions, (n) => n.tag === 'a' && n.getAttribute('href') === '/nodes' && n.textContent === 'Not now').length === 1);
    assert.deepStrictEqual(fetches.map((f) => f.method + ' ' + f.url), ['GET /enroll/ingresses']);
  }

  // approveSendsFingerprintAndNode
  {
    fetches.length = 0;
    fetchReply = (url, opts) => (opts && opts.method === 'POST' ? json({ ok: true, sync_pending: false, sync: { pending: false } }) : json({ ingresses: [bridge()] }));
    const els = approveEls();
    const went = [];
    const ctrl = approve.mount(els, { go: (u) => went.push(u) });
    await ctrl.load('bridge-3');
    buttonNamed(els.actions, 'Approve bridge-3').click();
    await flush();
    const posts = sent('POST', '/approve');
    assert.strictEqual(posts.length, 1, 'one approve request');
    assert.strictEqual(posts[0].url, '/enroll/ingresses/bridge-3/approve');
    assert.deepStrictEqual(JSON.parse(posts[0].body), { fingerprint: '7e3b91c40fa2d86e15b7c3409a1d62ef', node_id: 21 });
    assert.deepStrictEqual(went, ['/nodes']);
  }
  {
    fetches.length = 0;
    const went = [];
    // a refusal shows the reason, reads the bridge again and stays on the page
    fetchReply = (url, opts) => (opts && opts.method === 'POST' ? json({ detail: 'the fingerprint does not match; reload and review them' }, 409) : json({ ingresses: [bridge()] }));
    const els = approveEls();
    const ctrl = approve.mount(els, { go: (u) => went.push(u) });
    await ctrl.load('bridge-3');
    fetchReply = (url, opts) => (opts && opts.method === 'POST' ? json({ detail: 'the fingerprint does not match; reload and review them' }, 409) : json({ ingresses: [bridge({ fingerprint: 'bbbb2222' })] }));
    buttonNamed(els.actions, 'Approve bridge-3').click();
    await flush();
    assert.strictEqual(els.error.hidden, false);
    assert.ok(/does not match/.test(els.error.textContent));
    assert.deepStrictEqual(went, []);
    assert.strictEqual(fieldValue(els, 'Fingerprint'), 'bbbb2222', 'the review shows what is there now');
  }

  // changedIdentityShowsCritical
  {
    fetches.length = 0;
    const went = [];
    fetchReply = (url, opts) => (opts && opts.method === 'POST' ? json({ ok: true }) : json({ ingresses: [bridge({ approval: 'changed', approved: true, detail: 'another node publishes these parameters' })] }));
    const els = approveEls();
    const ctrl = approve.mount(els, { go: (u) => went.push(u) });
    await ctrl.load('bridge-3');
    assert.strictEqual(els.changed.hidden, false, 'critical note');
    assert.ok(/changed its host identity/.test(els.changed.textContent));
    assert.ok(/treat it as compromised/.test(els.changed.textContent));
    assert.ok(/another node publishes/.test(els.changed.textContent));
    assert.ok(findAll(els.changed, (n) => hasClass(n, 'crit')).length > 0, 'critical glyph');
    const go = buttonNamed(els.actions, 'Approve bridge-3 anyway');
    assert.ok(go && hasClass(go, 'danger'), 'the button is not the primary action');
    go.click();
    await flush();
    assert.strictEqual(sent('POST', '/approve').length, 0, 'a changed identity needs a second answer');
    const panel = findAll(els.confirm, (n) => hasClass(n, 'confirm'))[0];
    assert.ok(panel, 'confirmation panel');
    assert.ok(/Approve a changed identity/.test(panel.textContent));
    buttonNamed(panel, 'Cancel').click();
    assert.strictEqual(sent('POST', '/approve').length, 0);
    buttonNamed(els.actions, 'Approve bridge-3 anyway').click();
    buttonNamed(findAll(els.confirm, (n) => hasClass(n, 'confirm'))[0], 'Approve bridge-3').click();
    await flush();
    const posts = sent('POST', '/approve');
    assert.strictEqual(posts.length, 1);
    assert.deepStrictEqual(JSON.parse(posts[0].body), { fingerprint: '7e3b91c40fa2d86e15b7c3409a1d62ef', node_id: 21 });
    assert.deepStrictEqual(went, ['/nodes']);
  }

  // not found, approved and not approvable
  {
    fetchReply = () => json({ ingresses: [bridge()] });
    let els = approveEls();
    let ctrl = approve.mount(els, {});
    await ctrl.load('nope');
    assert.strictEqual(els.title.textContent, 'Bridge not found');
    assert.strictEqual(els.info.hidden, false);
    assert.strictEqual(els.review.hidden, true);
    assert.strictEqual(els.actions.hidden, true);
    els = approveEls(); ctrl = approve.mount(els, {});
    await ctrl.load('');
    assert.strictEqual(els.info.hidden, false);

    fetchReply = () => json({ ingresses: [bridge({ approval: 'approved', approved: true, active: true })] });
    els = approveEls(); ctrl = approve.mount(els, {});
    await ctrl.load('bridge-3');
    assert.strictEqual(els.review.hidden, true);
    assert.ok(buttonNamed(els.actions, 'Remove approval'));
    assert.ok(!buttonNamed(els.actions, 'Approve bridge-3'));

    fetchReply = () => json({ ingresses: [bridge({ source: 'configured', approval: 'configured', fingerprint: '' })] });
    els = approveEls(); ctrl = approve.mount(els, {});
    await ctrl.load('bridge-3');
    assert.ok(/cannot be approved here/.test(els.info.textContent));
    assert.strictEqual(els.actions.hidden, true);
  }

  // text from the network is plain text: control and bidi characters are dropped
  {
    fetchReply = () => json({ ingresses: [bridge({ hostname: 'bad‮name\u0007', given_name: 'x'.repeat(200) })] });
    const els = approveEls();
    const ctrl = approve.mount(els, {});
    await ctrl.load('bridge-3');
    assert.strictEqual(fieldValue(els, 'Hostname'), 'badname');
    assert.strictEqual(fieldValue(els, 'Name').length, 63);
  }

  console.log('enroll self-test ok');
})().catch((e) => { console.error(e); process.exit(1); });
