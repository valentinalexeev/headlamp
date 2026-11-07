// Run with: node serve_selftest.js <path to hl.js> <path to serve.js>. Exits non-zero on a failed check.
const assert = require('assert');
const path = require('path');

// A just-enough DOM, as in the funnel self-test: elements keep children and text, record listeners, refuse innerHTML.
class FakeNode {
  constructor(tag) {
    this.tag = tag; this.children = []; this.parentNode = null; this.attrs = {}; this.listeners = {};
    this.className = ''; this._text = ''; this.hidden = false; this.disabled = false; this.value = '';
  }
  get textContent() { return this._text + this.children.map((c) => c.textContent).join(''); }
  set textContent(v) { this._text = String(v); this.children = []; }
  get firstChild() { return this.children[0] || null; }
  get isConnected() { let n = this; while (n.parentNode) { n = n.parentNode; } return n.isRoot === true; }
  appendChild(c) { if (c.parentNode) { c.parentNode.removeChild(c); } c.parentNode = this; this.children.push(c); return c; }
  removeChild(c) { const i = this.children.indexOf(c); if (i >= 0) { this.children.splice(i, 1); } c.parentNode = null; return c; }
  remove() { if (this.parentNode) { this.parentNode.removeChild(this); } }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  removeAttribute(k) { delete this.attrs[k]; }
  addEventListener(t, f) { (this.listeners[t] = this.listeners[t] || []).push(f); }
  focus() { this.focused = true; global.document.activeElement = this; }
  set innerHTML(v) { throw new Error('innerHTML must not be used'); }
  set outerHTML(v) { throw new Error('outerHTML must not be used'); }
  click() { if (this.disabled) { return; } fire(this, 'click'); }
}
const fire = (n, type, ev) => (n.listeners[type] || []).forEach((f) => f(Object.assign({ preventDefault() {}, stopPropagation() {}, target: n }, ev)));
const walk = (n, out = []) => { out.push(n); n.children.forEach((c) => walk(c, out)); return out; };
const findAll = (root, pred) => walk(root).filter(pred);
const buttonNamed = (root, text) => findAll(root, (n) => n.tag === 'button' && n.textContent === text)[0];
const textOf = (root) => root.textContent;

global.document = {
  createElement: (t) => new FakeNode(t), createElementNS: (_, t) => new FakeNode(t), createTextNode: (s) => { const n = new FakeNode('#text'); n._text = s; return n; },
  getElementById: () => null, addEventListener() {}, hidden: false, activeElement: null,
};
global.window = { location: { search: '', href: 'http://localhost/serve' }, history: { replaceState() {} }, addEventListener() {} };
global.setInterval = () => 0;
const fetches = [];
let fetchReply = () => ({ ok: true, status: 200, body: '{}' });
global.fetch = (url, opts) => {
  fetches.push({ url, method: (opts && opts.method) || 'GET', body: opts && opts.body });
  const r = fetchReply(url, opts);
  return Promise.resolve({ ok: r.ok, status: r.status, text: () => Promise.resolve(r.body) });
};
const json = (v, status) => ({ ok: (status || 200) < 400, status: status || 200, body: JSON.stringify(v) });
const empty = () => ({ ok: true, status: 204, body: '' });
const flush = async () => { for (let i = 0; i < 8; i++) { await new Promise((r) => setImmediate(r)); } };
const sent = (method, part) => fetches.filter((f) => f.method === method && f.url.includes(part));

require(path.resolve(process.argv[2]));
require(path.resolve(process.argv[3]));
const serve = global.window.hl.serve;
assert.ok(serve, 'hl.serve is defined');

const NOW = Date.parse('2026-01-01T12:00:00Z');
const DAY = 24 * 3600 * 1000;
const at = (days) => new Date(NOW + days * DAY).toISOString();
const SECRET = 'hskey-auth-FAKE-SECRET-0001';
function state(over) {
  return Object.assign({
    zone: 'svc.example.net', ca_fingerprint: 'ab:cd', dns_server: '100.64.0.1', dns_refreshed: at(0), can_admin: true, key_ttl_hours: 2160,
    services: [
      { id: 'g1', name: 'grafana.svc.example.net', namespace: 'mon', created: at(-10), keys: [{ id: 11, expires: at(80) }],
        nodes: [{ name: 'grafana-a', online: true, ips: ['100.64.0.20'], key_id: 11 }] },
      { id: 'a1', name: '*.apps.svc.example.net', namespace: 'k8s', created: at(-10), keys: [{ id: 22, expires: at(90) }, { id: 21, expires: at(5) }],
        nodes: [{ name: 'ingress', online: true, ips: ['100.64.0.21'], key_id: 21 }] },
      { id: 'o1', name: 'old.svc.example.net', namespace: 'lab', created: at(-90), keys: [{ id: 31, expires: at(-1) }], nodes: [] },
      { id: 's1', name: 'soon.svc.example.net', namespace: 'lab', created: at(-80), keys: [{ id: 41, expires: at(3) }],
        nodes: [{ name: 'soon-a', online: false, ips: ['100.64.0.30'], key_id: 41 }] },
    ],
  }, over);
}
function els() {
  const root = new FakeNode('div'); root.isRoot = true;
  const e = {};
  ['sub', 'error', 'notice', 'dnsnote', 'zone', 'panel', 'empty', 'emptytext', 'tablewrap', 'rows', 'addslot', 'side'].forEach((k) => { e[k] = root.appendChild(new FakeNode(k === 'rows' ? 'tbody' : 'div')); });
  e.root = root;
  return e;
}
const rowFor = (e, name) => e.rows.children.find((tr) => tr.children[0] && tr.children[0].textContent.startsWith(name));

(async () => {
  // rows: state words, wildcard, rotation and expiry
  {
    const rows = serve.serviceRows(state(), NOW);
    const by = Object.fromEntries(rows.map((r) => [r.id, r]));
    assert.strictEqual(by.g1.level, 'ok');
    assert.strictEqual(by.g1.word, 'Online');
    assert.ok(by.a1.wildcard);
    assert.strictEqual(by.a1.word, 'Online, rotating', 'two keys, the new one is fine');
    assert.ok(by.a1.keys[1].soon, 'the old key is near expiry');
    assert.strictEqual(by.o1.level, 'crit');
    assert.strictEqual(by.o1.word, 'Key expired');
    assert.strictEqual(by.s1.word, 'Offline', 'offline outranks a key that expires soon');
    assert.strictEqual(serve.serviceRows({}, NOW).length, 0);
  }

  // the header: zone, fingerprint, split line with the node-control address, DNS error
  {
    const e = els();
    const ctrl = serve.mount(e, { now: () => NOW });
    ctrl.update(state({ dns_error: 'headscale: timeout' }));
    const z = textOf(e.zone);
    assert.ok(z.includes('svc.example.net') && z.includes('ab:cd'));
    assert.ok(z.includes('split:') && z.includes('- 100.64.0.1'), 'the split line names node-control');
    assert.ok(!e.dnsnote.hidden && textOf(e.dnsnote).includes('headscale: timeout'));
    assert.ok(textOf(e.sub).startsWith('4 services, 2 online.'));
  }

  // viewer: no admin control anywhere
  {
    const e = els();
    const ctrl = serve.mount(e, { now: () => NOW });
    ctrl.update(state({ can_admin: false }));
    for (const b of ['Add service', 'Add key', 'Delete', 'Retire old key']) {
      assert.ok(!buttonNamed(e.root, b), 'a viewer sees no ' + b);
    }
    assert.ok(rowFor(e, 'grafana.svc.example.net'), 'a viewer sees the services');
  }

  // admin: add a service, see the key once with the sidecar env, Done clears it
  {
    fetches.length = 0;
    fetchReply = (url, opts) => {
      if (opts && opts.method === 'POST' && url === '/serve/services') {
        return json({ service: { id: 'n1', name: 'new.svc.example.net' }, auth_key: SECRET }, 201);
      }
      return json(state());
    };
    const e = els();
    const ctrl = serve.mount(e, { now: () => NOW });
    ctrl.update(state());
    buttonNamed(e.root, 'Add service').click();
    const form = findAll(e.panel, (n) => n.tag === 'form')[0];
    const inputs = findAll(form, (n) => n.tag === 'input');
    inputs[0].value = 'New'; inputs[1].value = 'BAD NS';
    fire(form, 'submit');
    assert.ok(textOf(form).includes('namespace is 1 to 32'), 'the namespace is checked before sending');
    assert.strictEqual(sent('POST', '/serve/services').length, 0);
    inputs[1].value = 'lab';
    fire(form, 'submit');
    await flush();
    const body = JSON.parse(sent('POST', '/serve/services')[0].body);
    assert.deepStrictEqual(body, { name: 'new', namespace: 'lab' });
    const side = textOf(e.side);
    assert.ok(side.includes(SECRET), 'the key is shown');
    assert.ok(side.includes('TS_AUTHKEY=' + SECRET) && side.includes('FUNNEL_SERVE_URL=http://100.64.0.1'), 'with the sidecar env');
    assert.ok(!textOf(e.rows).includes(SECRET) && !textOf(e.notice).includes(SECRET), 'and nowhere else');
    // a reload does not bring it back or lose it
    await ctrl.reload();
    assert.ok(textOf(e.side).includes(SECRET));
    buttonNamed(e.side, 'Done').click();
    assert.ok(!textOf(e.root).includes(SECRET), 'Done removes the key from the page');
  }

  // rotation: Add key shows the new key without env; Retire on the old key asks first
  {
    fetches.length = 0;
    fetchReply = (url, opts) => {
      const m = opts && opts.method;
      if (m === 'POST' && url.endsWith('/keys')) { return json({ service: {}, auth_key: SECRET }, 201); }
      if (m === 'DELETE') { return empty(); }
      return json(state());
    };
    const e = els();
    const ctrl = serve.mount(e, { now: () => NOW });
    ctrl.update(state());
    const g = rowFor(e, 'grafana.svc.example.net');
    buttonNamed(g, 'Add key').click();
    await flush();
    assert.strictEqual(sent('POST', '/serve/services/g1/keys').length, 1);
    assert.ok(textOf(e.side).includes(SECRET) && !textOf(e.side).includes('TS_AUTHKEY'), 'a rotation shows the key, no env');
    const a = rowFor(e, '*.apps.svc.example.net');
    assert.ok(!buttonNamed(a, 'Add key'), 'two keys: retire one first');
    buttonNamed(a, 'Retire old key').click();
    assert.strictEqual(sent('DELETE', '/keys/').length, 0, 'nothing before the confirm');
    assert.ok(textOf(e.rows).includes('Retire key #21?'));
    buttonNamed(e.rows, 'Retire key').click();
    await flush();
    assert.strictEqual(sent('DELETE', '/serve/services/a1/keys/21').length, 1);
  }

  // delete: only after the exact name is typed
  {
    fetches.length = 0;
    fetchReply = (url, opts) => (opts && opts.method === 'DELETE' ? empty() : json(state()));
    const e = els();
    const ctrl = serve.mount(e, { now: () => NOW });
    ctrl.update(state());
    buttonNamed(rowFor(e, 'old.svc.example.net'), 'Delete').click();
    const go = buttonNamed(e.rows, 'Delete service');
    assert.ok(go.disabled, 'disabled until the name is typed');
    const input = findAll(e.rows, (n) => n.tag === 'input')[0];
    input.value = 'old'; fire(input, 'input');
    assert.ok(go.disabled);
    go.click();
    assert.strictEqual(sent('DELETE', '/serve/services/o1').length, 0);
    input.value = 'old.svc.example.net'; fire(input, 'input');
    assert.ok(!go.disabled);
    go.click();
    await flush();
    assert.strictEqual(sent('DELETE', '/serve/services/o1').length, 1);
  }

  // empty state
  {
    const e = els();
    const ctrl = serve.mount(e, { now: () => NOW });
    ctrl.update(state({ services: [] }));
    assert.ok(e.tablewrap.hidden && !e.empty.hidden);
    assert.ok(textOf(e.emptytext).includes('Add a service'));
  }

  console.log('serve self-test: ok');
})().catch((e) => { console.error(e); process.exit(1); });
