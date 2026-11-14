// Run with: node dns_selftest.js <path to hl.js> <path to dns.js>. Exits non-zero on a failed check.
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
global.window = { location: { search: '', href: 'http://localhost/dns' }, history: { replaceState() {} }, addEventListener() {} };
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
const dns = global.window.hl.dns;
assert.ok(dns, 'hl.dns is defined');

const NOW = Date.parse('2026-01-01T12:00:00Z');
function state(over) {
  return Object.assign({
    server: 'tcp://100.64.0.9:53', listening: true, base_domain: 'vpn.example.com', serve_zone: 'svc.example.com', can_admin: true,
    refreshed: new Date(NOW).toISOString(), extra_records: ['other.example.com'],
    extra_records_file: '/dns/extra-records.json', extra_records_file_written: new Date(NOW - 60000).toISOString(),
    zones: [{ name: 'corp.example', created: new Date(NOW).toISOString() }],
    split: { 'corp.example': ['100.64.0.9'], 'ruled.example.com': ['100.64.0.9'] },
    records: [
      { name: 'plain.example.com', type: 'A', addresses: ['100.64.0.5'], answer: ['100.64.0.5'], in_file: true },
      { name: 'ruled.example.com', type: 'A', tag: 'tag:llm', answer: ['100.64.0.41'], in_file: false,
        rules: [{ users: ['alice@example.com'], tags: ['tag:dev'], addresses: ['100.64.0.6'], answer: ['100.64.0.6'] }, { tags: ['tag:x'], answer: [] }] },
      { name: 'app.corp.example', type: 'CNAME', target: 'plain.example.com', answer: ['plain.example.com'], zone: 'corp.example' },
      { name: '_http._tcp.corp.example', type: 'SRV', target: 'app.corp.example', port: 80, priority: 1, weight: 2, answer: ['1 2 80 app.corp.example'], zone: 'corp.example' },
    ],
  }, over);
}
function els() {
  const root = new FakeNode('div'); root.isRoot = true;
  const e = {};
  ['sub', 'error', 'notice', 'health', 'server', 'panel', 'zones', 'zoneswrap', 'zonesempty', 'empty', 'emptytext', 'tablewrap', 'rows', 'config', 'addslot']
    .forEach((k) => { e[k] = root.appendChild(new FakeNode(k === 'rows' || k === 'zones' ? 'tbody' : 'div')); });
  e.root = root;
  return e;
}
const rowFor = (body, name) => body.children.find((tr) => tr.children[0] && tr.children[0].textContent.startsWith(name));

(async () => {
  // the one-line data form, both ways
  {
    assert.deepStrictEqual(dns.parseData('A', '100.64.0.5, 100.64.0.6'), { addresses: ['100.64.0.5', '100.64.0.6'] });
    assert.deepStrictEqual(dns.parseData('A', 'node:NAS'), { node: 'nas' });
    assert.deepStrictEqual(dns.parseData('A', 'tag:llm'), { tag: 'tag:llm' });
    assert.deepStrictEqual(dns.parseData('CNAME', 'App.Example.com'), { target: 'app.example.com' });
    assert.strictEqual(dns.parseData('TXT', 'x'.repeat(300)).text.length, 2, 'long TXT is cut into 255-byte strings');
    assert.deepStrictEqual(dns.parseData('SRV', '10 5 8080 app.example.com'), { priority: 10, weight: 5, port: 8080, target: 'app.example.com' });
    assert.ok(dns.parseData('SRV', '10 5 app.example.com').error);
    assert.ok(dns.parseData('SRV', '10 5 70000 app.example.com').error);
    assert.deepStrictEqual(dns.parseData('A', ''), {});
    assert.strictEqual(dns.dataText('SRV', { priority: 10, weight: 5, port: 8080, target: 'a.example.com' }), '10 5 8080 a.example.com');
    assert.strictEqual(dns.dataText('A', { node: 'nas' }), 'node:nas');
    const r = dns.parseRules('A', 'alice@example.com, tag:dev = 100.64.0.6\n\ntag:x =');
    assert.deepStrictEqual(r.rules[0], { users: ['alice@example.com'], tags: ['tag:dev'], addresses: ['100.64.0.6'], type: 'A' });
    assert.deepStrictEqual(r.rules[1], { users: [], tags: ['tag:x'], type: 'A' }, 'an empty data hides the name');
    assert.ok(dns.parseRules('A', 'no equals sign').error);
  }

  // rows and the split block
  {
    const rows = dns.recordRows(state());
    assert.strictEqual(rows.length, 4);
    assert.strictEqual(rows[1].rulesText, 'alice@example.com, tag:dev = 100.64.0.6\ntag:x = ');
    assert.ok(rows[0].inFile && !rows[1].inFile);
    const y = dns.splitLines(state());
    assert.ok(y.startsWith('dns:\n  nameservers:\n    split:\n      corp.example:\n        - 100.64.0.9'), y);
    assert.strictEqual(dns.splitLines({}), '');
  }

  // the page: zones, records, rules, served by, file state, split lines
  {
    const e = els();
    const ctrl = dns.mount(e, { now: () => NOW });
    ctrl.update(state());
    assert.ok(rowFor(e.zones, 'corp.example'), 'the zone row');
    assert.strictEqual(rowFor(e.zones, 'corp.example').children[1].textContent, '2', 'records in the zone');
    const ruled = rowFor(e.rows, 'ruled.example.com');
    assert.ok(ruled.children[4].textContent.includes('1. alice@example.com, tag:dev: 100.64.0.6'));
    assert.ok(ruled.children[4].textContent.includes('2. tag:x: no answer'));
    assert.ok(ruled.children[5].textContent.includes('node-control'));
    assert.ok(rowFor(e.rows, 'plain.example.com').children[5].textContent.includes('headscale file'));
    assert.ok(rowFor(e.rows, 'plain.example.com').children[4].textContent.includes('everyone'));
    const cfg = textOf(e.config);
    assert.ok(cfg.includes('/dns/extra-records.json, written 1 min') || cfg.includes('/dns/extra-records.json, written'), cfg);
    assert.ok(cfg.includes('ruled.example.com:'), 'the split block');
    assert.ok(textOf(e.server).includes('other.example.com'));
    assert.ok(textOf(e.sub).startsWith('4 records, 1 zone.'));
    assert.ok(e.health.hidden);
    ctrl.update(state({ extra_records_file_error: 'extra records file: permission denied' }));
    assert.ok(!e.health.hidden && textOf(e.health).includes('permission denied'));
    ctrl.update(state({ extra_records_file: '' }));
    assert.ok(textOf(e.config).includes('Not set'));
  }

  // viewer: no admin control anywhere
  {
    const e = els();
    const ctrl = dns.mount(e, { now: () => NOW });
    ctrl.update(state({ can_admin: false }));
    for (const b of ['Add record', 'Add zone', 'Edit', 'Delete']) {
      assert.ok(!buttonNamed(e.root, b), 'a viewer sees no ' + b);
    }
  }

  // admin: add a record with a rule, then delete a zone
  {
    fetches.length = 0;
    fetchReply = (url, opts) => {
      if (opts && opts.method === 'POST') { return json({}, 201); }
      if (opts && opts.method === 'DELETE') { return empty(); }
      return json(state());
    };
    const e = els();
    const ctrl = dns.mount(e, { now: () => NOW });
    ctrl.update(state());
    buttonNamed(e.root, 'Add record').click();
    const form = findAll(e.panel, (n) => n.tag === 'form')[0];
    const inputs = findAll(form, (n) => n.tag === 'input');
    const sel = findAll(form, (n) => n.tag === 'select')[0];
    const ta = findAll(form, (n) => n.tag === 'textarea')[0];
    fire(form, 'submit');
    await flush();
    assert.strictEqual(sent('POST', '/dns/records').length, 0, 'no name: nothing sent');
    inputs[0].value = 'New.Example.com'; sel.value = 'A'; inputs[1].value = '100.64.0.5'; ta.value = 'tag:dev = node:nas';
    fire(form, 'submit');
    await flush();
    const p = sent('POST', '/dns/records');
    assert.strictEqual(p.length, 1);
    assert.deepStrictEqual(JSON.parse(p[0].body), { type: 'A', note: '', addresses: ['100.64.0.5'], rules: [{ users: [], tags: ['tag:dev'], node: 'nas', type: 'A' }], name: 'new.example.com' });
    assert.ok(textOf(e.notice).includes('Added new.example.com'));

    // edit keeps the name in the path
    buttonNamed(rowFor(e.rows, 'ruled.example.com'), 'Edit').click();
    const f2 = findAll(e.panel, (n) => n.tag === 'form')[0];
    assert.strictEqual(findAll(f2, (n) => n.tag === 'textarea')[0].value, 'alice@example.com, tag:dev = 100.64.0.6\ntag:x = ');
    fire(f2, 'submit');
    await flush();
    const put = sent('PUT', '/dns/records/ruled.example.com');
    assert.strictEqual(put.length, 1);
    assert.strictEqual(JSON.parse(put[0].body).tag, 'tag:llm');

    buttonNamed(rowFor(e.zones, 'corp.example'), 'Delete').click();
    buttonNamed(e.root, 'Delete zone').click();
    await flush();
    assert.strictEqual(sent('DELETE', '/dns/zones/corp.example').length, 1);
  }

  console.log('dns self-test: ok');
})().catch((err) => { console.error(err); process.exit(1); });
