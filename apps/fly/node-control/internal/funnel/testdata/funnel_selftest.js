// Run with: node funnel_selftest.js <path to hl.js> <path to funnel.js> [<path to config.js>]. Exits non-zero on a failed check.
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
  get isConnected() { let n = this; while (n.parentNode) { n = n.parentNode; } return n.isRoot === true; }
  appendChild(c) { if (c.parentNode) { c.parentNode.removeChild(c); } c.parentNode = this; this.children.push(c); return c; }
  insertBefore(c, ref) {
    if (c.parentNode) { c.parentNode.removeChild(c); }
    const i = ref ? this.children.indexOf(ref) : -1;
    c.parentNode = this;
    if (i < 0) { this.children.push(c); } else { this.children.splice(i, 0, c); }
    return c;
  }
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
const buttonsNamed = (root, text) => findAll(root, (n) => n.tag === 'button' && n.textContent === text);
const linkTo = (root, href) => findAll(root, (n) => n.tag === 'a' && n.getAttribute('href') === href)[0];

const replaced = [];
const pagehide = [];
global.document = {
  createElement: (t) => new FakeNode(t), createElementNS: (_, t) => new FakeNode(t), createTextNode: (s) => { const n = new FakeNode('#text'); n._text = s; return n; },
  getElementById: () => null, addEventListener() {}, hidden: false, activeElement: null,
};
global.window = {
  location: { search: '', href: 'http://localhost/funnel' },
  history: { replaceState(_a, _b, url) { replaced.push(url); global.window.location.search = url.includes('?') ? url.slice(url.indexOf('?')) : ''; global.window.location.href = 'http://localhost' + url; } },
  addEventListener(t, f) { if (t === 'pagehide') { pagehide.push(f); } },
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
const empty = (status) => ({ ok: true, status: status || 204, body: '' });
const flush = async () => { for (let i = 0; i < 8; i++) { await new Promise((r) => setImmediate(r)); } };
const sent = (method, urlPart) => fetches.filter((f) => f.method === method && f.url.includes(urlPart));
const setSearch = (s) => { global.window.location.search = s; };

require(path.resolve(process.argv[2]));
require(path.resolve(process.argv[3]));
const funnel = global.window.hl.funnel;
assert.ok(funnel, 'hl.funnel is defined');
if (process.argv[4]) { require(path.resolve(process.argv[4])); }

const NOW = Date.parse('2026-01-01T12:00:00Z');
const iso = (secAgo) => new Date(NOW - secAgo * 1000).toISOString();
const report = (routes, extra) => Object.assign({ received_at: iso(12), age_seconds: 12, stale: false, report: { applied_version: 'v', snapshot_loaded: true, peer_count: 3, routes, errors: [] } }, extra);
const reg = (ns, service, extra) => Object.assign({ ns, service, target: '100.64.0.21', port: 9000, scheme: 'http', owner_node_id: '4', owner_name: 'web-1', target_online: true }, extra);
const man = (ns, service, extra) => Object.assign({ ns, service, target: 'web-1', port: 9100, scheme: 'http', insecure_skip_verify: false }, extra);
const nsActive = (name) => ({ name, state: 'active', ports: [{ lo: 9000, hi: 9010 }], pending_ports: [], note: '' });
const baseState = (extra) => Object.assign({
  public_domain: 'funnel.example.net', namespaces: [nsActive('demo'), nsActive('lab')],
  routes: [man('lab', 'grafana')],
  blocks: [{ ns: 'lab', service: 'test', reason: 'under repair', actor: 'alice', at: iso(3600) }],
  ingress: report([reg('demo', 'status'), reg('demo', 'docs', { target_online: false }), reg('lab', 'grafana', { owner_node_id: '', owner_name: '', target: '100.64.0.40' }), reg('lab', 'test')]),
}, extra);

function funnelEls() {
  const mk = (tag) => { const n = new FakeNode(tag); n.isRoot = true; return n; };
  const els = { sub: mk('p'), error: mk('div'), notice: mk('div'), reportnote: mk('section'), panel: mk('div'), chips: mk('div'), empty: mk('div'), emptytext: mk('p'),
    tablewrap: mk('div'), rows: mk('tbody'), addslot: mk('span') };
  els.error.hidden = els.notice.hidden = els.reportnote.hidden = els.chips.hidden = els.empty.hidden = els.tablewrap.hidden = true;
  return els;
}
const rowsOf = (els) => els.rows.children.filter((tr) => !tr.children.some((td) => hasClass(td, 'detail')));
const rowFor = (els, name) => rowsOf(els).find((tr) => tr.children[0] && tr.children[0].textContent === name);
const cellText = (tr, i) => tr.children[i].textContent;

(async () => {
  // mergesSourcesAndBlocks: one row per public name, from the report, the store and the blocks
  {
    const rows = funnel.serviceRows(baseState({ blocks: [{ ns: 'lab', service: 'test', reason: 'r' }, { ns: 'lab', service: 'ghost', reason: '' }] }));
    const by = Object.fromEntries(rows.map((r) => [r.name, r]));
    assert.deepStrictEqual(Object.keys(by).sort(), [
      'docs.demo.funnel.example.net', 'ghost.lab.funnel.example.net', 'grafana.lab.funnel.example.net', 'status.demo.funnel.example.net', 'test.lab.funnel.example.net']);
    assert.strictEqual(by['status.demo.funnel.example.net'].source, 'registered');
    assert.strictEqual(by['status.demo.funnel.example.net'].owner, 'web-1');
    assert.strictEqual(by['status.demo.funnel.example.net'].target, '100.64.0.21:9000');
    assert.strictEqual(by['grafana.lab.funnel.example.net'].source, 'manual', 'the store says manual');
    assert.strictEqual(by['grafana.lab.funnel.example.net'].target, 'web-1:9100');
    assert.strictEqual(by['grafana.lab.funnel.example.net'].owner, 'Operator');
    assert.strictEqual(by['test.lab.funnel.example.net'].source, 'registered', 'a block does not change the source');
    assert.strictEqual(by['ghost.lab.funnel.example.net'].source, 'none', 'a block with no route is still listed');
    assert.strictEqual(by['ghost.lab.funnel.example.net'].blocked, true);
    // a manual route the ingress has not pulled yet
    const rows2 = funnel.serviceRows(baseState({ routes: [man('lab', 'grafana'), man('lab', 'new')] }));
    const fresh = rows2.find((r) => r.service === 'new');
    assert.strictEqual(fresh.kind, 'unreported');
    assert.strictEqual(fresh.word, 'Not reported yet');
    // no report at all
    assert.strictEqual(funnel.serviceRows({ public_domain: 'x.example.net', routes: [], blocks: [], ingress: null }).length, 0);
  }

  // blockedShowsOff: shape and word, never colour alone
  {
    const rows = funnel.serviceRows(baseState());
    const t = rows.find((r) => r.service === 'test');
    assert.strictEqual(t.level, 'off');
    assert.strictEqual(t.word, 'Blocked');
    assert.strictEqual(rows.find((r) => r.service === 'docs').level, 'warn');
    assert.strictEqual(rows.find((r) => r.service === 'docs').word, 'Offline');
    assert.strictEqual(rows.find((r) => r.service === 'status').word, 'Online');
    const els = funnelEls();
    const ctrl = funnel.mount(els, { now: () => NOW });
    ctrl.update(baseState());
    const row = rowFor(els, 'test.lab.funnel.example.net');
    const st = findAll(row, (n) => hasClass(n, 'st'))[0];
    assert.ok(hasClass(st, 'off'));
    assert.ok(findAll(st, (n) => hasClass(n, 'g') && hasClass(n, 'off')).length === 1, 'a glyph');
    assert.strictEqual(st.textContent, 'Blocked');
    assert.ok(buttonNamed(row, 'Unblock'), 'a blocked name offers Unblock');
    assert.ok(!buttonNamed(row, 'Block'));
    // offline rows sort first
    assert.strictEqual(cellText(rowsOf(els)[0], 0), 'docs.demo.funnel.example.net');
  }

  // manualCanBeRemoved / registeredCannotBeRemoved
  {
    fetches.length = 0;
    fetchReply = (url, opts) => ((opts && opts.method === 'DELETE') ? empty() : json(baseState({ routes: [] })));
    const els = funnelEls();
    const ctrl = funnel.mount(els, { now: () => NOW });
    ctrl.update(baseState());
    const reg1 = rowFor(els, 'status.demo.funnel.example.net');
    assert.ok(!buttonNamed(reg1, 'Remove'), 'a registered route cannot be removed');
    assert.ok(buttonNamed(reg1, 'Block'));
    const man1 = rowFor(els, 'grafana.lab.funnel.example.net');
    assert.ok(buttonNamed(man1, 'Block') && buttonNamed(man1, 'Remove'));
    buttonNamed(man1, 'Remove').click();
    const panel = findAll(els.rows, (n) => hasClass(n, 'confirm'))[0];
    assert.ok(panel, 'a confirmation opens in the row');
    assert.strictEqual(sent('DELETE', '/funnel/routes').length, 0, 'nothing is deleted before the answer');
    buttonNamed(panel, 'Remove grafana.lab.funnel.example.net').click();
    await flush();
    const del = sent('DELETE', '/funnel/routes');
    assert.strictEqual(del.length, 1);
    assert.strictEqual(del[0].url, '/funnel/routes/lab/grafana');
    assert.ok(/Removed grafana/.test(els.notice.textContent));
    assert.strictEqual(els.notice.hidden, false);
    // the cancel path leaves it alone
    fetches.length = 0;
    ctrl.update(baseState());
    buttonNamed(rowFor(els, 'grafana.lab.funnel.example.net'), 'Remove').click();
    buttonNamed(findAll(els.rows, (n) => hasClass(n, 'confirm'))[0], 'Keep').click();
    await flush();
    assert.strictEqual(sent('DELETE', '/funnel/routes').length, 0);
  }

  // blocking asks for a reason and sends it
  {
    fetches.length = 0;
    fetchReply = (url, opts) => ((opts && opts.method === 'POST') ? empty() : json(baseState()));
    const els = funnelEls();
    const ctrl = funnel.mount(els, { now: () => NOW });
    ctrl.update(baseState());
    buttonNamed(rowFor(els, 'status.demo.funnel.example.net'), 'Block').click();
    const panel = findAll(els.rows, (n) => hasClass(n, 'confirm'))[0];
    const reason = findAll(panel, (n) => n.tag === 'input')[0];
    assert.ok(reason, 'a reason field');
    assert.strictEqual(reason.getAttribute('maxlength'), '500');
    reason.value = ' planned maintenance ';
    buttonNamed(panel, 'Block status.demo.funnel.example.net').click();
    await flush();
    const post = sent('POST', '/funnel/blocks');
    assert.strictEqual(post.length, 1);
    assert.deepStrictEqual(JSON.parse(post[0].body), { ns: 'demo', service: 'status', reason: 'planned maintenance' });
  }

  // a refused mutation is shown, not swallowed
  {
    fetches.length = 0;
    fetchReply = (url, opts) => ((opts && opts.method === 'DELETE') ? json({ detail: 'not found' }, 404) : json(baseState()));
    const els = funnelEls();
    const ctrl = funnel.mount(els, { now: () => NOW });
    ctrl.update(baseState());
    buttonNamed(rowFor(els, 'test.lab.funnel.example.net'), 'Unblock').click();
    await flush();
    assert.strictEqual(els.error.hidden, false);
    assert.ok(/Unblock failed for test\.lab\.funnel\.example\.net: not found/.test(els.error.textContent), els.error.textContent);
  }

  // offlineFilterInURL: read from the URL, written back by the chips
  {
    replaced.length = 0;
    setSearch('?filter=offline');
    const els = funnelEls();
    const ctrl = funnel.mount(els, { now: () => NOW });
    ctrl.update(baseState());
    assert.deepStrictEqual(rowsOf(els).map((tr) => cellText(tr, 0)), ['docs.demo.funnel.example.net']);
    assert.strictEqual(els.chips.hidden, false);
    assert.strictEqual(buttonNamed(els.chips, 'Offline').getAttribute('aria-pressed'), 'true');
    buttonNamed(els.chips, 'Manual').click();
    assert.strictEqual(replaced[replaced.length - 1], '/funnel?filter=manual');
    assert.deepStrictEqual(rowsOf(els).map((tr) => cellText(tr, 0)), ['grafana.lab.funnel.example.net']);
    buttonNamed(els.chips, 'All').click();
    assert.strictEqual(replaced[replaced.length - 1], '/funnel', 'All leaves no parameter');
    assert.strictEqual(rowsOf(els).length, 4);
    buttonNamed(els.chips, 'Blocked').click();
    assert.deepStrictEqual(rowsOf(els).map((tr) => cellText(tr, 0)), ['test.lab.funnel.example.net']);
    setSearch('?filter=nonsense');
    const els2 = funnelEls();
    funnel.mount(els2, { now: () => NOW }).update(baseState());
    assert.strictEqual(rowsOf(els2).length, 4, 'an unknown filter means All');
    setSearch('');
  }

  // staleReportIsLabelled
  {
    const els = funnelEls();
    const ctrl = funnel.mount(els, { now: () => NOW });
    ctrl.update(baseState({ ingress: report([reg('demo', 'status'), reg('demo', 'docs', { target_online: false })], { stale: true, received_at: iso(600), age_seconds: 600 }) }));
    assert.strictEqual(els.reportnote.hidden, false);
    assert.ok(/stale/i.test(els.reportnote.textContent));
    assert.ok(/10 min ago/.test(els.reportnote.textContent), els.reportnote.textContent);
    assert.ok(findAll(els.reportnote, (n) => hasClass(n, 'st') && hasClass(n, 'warn')).length >= 1, 'a shape, not only words');
    assert.ok(/Online \(stale\)/.test(rowFor(els, 'status.demo.funnel.example.net').textContent));
    assert.ok(/Offline \(stale\)/.test(rowFor(els, 'docs.demo.funnel.example.net').textContent));
    assert.ok(/Ingress reported 10 min ago/.test(els.sub.textContent), els.sub.textContent);
    // a fresh report has no note
    const els2 = funnelEls();
    funnel.mount(els2, { now: () => NOW }).update(baseState());
    assert.strictEqual(els2.reportnote.hidden, true);
    assert.ok(/4 public routes\. Ingress reported 12 s ago with no errors\./.test(els2.sub.textContent) || /3 public routes\. Ingress reported 12 s ago with no errors\./.test(els2.sub.textContent), els2.sub.textContent);
  }

  // reportTextIsInert: names, targets and errors from the report are text
  {
    const evil = '<img src=x onerror=alert(1)>';
    const els = funnelEls();
    const ctrl = funnel.mount(els, { now: () => NOW });
    const st = baseState({ routes: [], blocks: [] });
    st.ingress = report([reg('demo', 'ok', { target: evil, owner_name: evil })]);
    st.ingress.report.errors = [evil + '‮', 'second'];
    ctrl.update(st);
    const tags = walk(els.rows).concat(walk(els.reportnote)).map((n) => n.tag);
    assert.ok(!tags.includes('img') && !tags.includes('script'), 'no element came from report text');
    assert.ok(els.reportnote.textContent.includes(evil), 'shown as text');
    assert.ok(!els.reportnote.textContent.includes('‮'), 'bidi override dropped');
    assert.ok(/reports 2 errors/.test(els.reportnote.textContent));
    assert.ok(/with 2 errors/.test(els.sub.textContent));
  }

  // emptyState: no routes at all
  {
    const els = funnelEls();
    const ctrl = funnel.mount(els, { now: () => NOW });
    ctrl.update({ public_domain: 'funnel.example.net', namespaces: [], routes: [], blocks: [], ingress: null });
    assert.strictEqual(els.empty.hidden, false);
    assert.strictEqual(els.tablewrap.hidden, true);
    assert.strictEqual(els.chips.hidden, true);
    assert.ok(/active namespace/.test(els.emptytext.textContent));
    const add = buttonNamed(els.addslot, 'Add route');
    assert.ok(add && add.disabled, 'Add route waits for a namespace');
    assert.ok(/has not reported yet/.test(els.sub.textContent));
    ctrl.update({ public_domain: 'funnel.example.net', namespaces: [nsActive('demo')], routes: [], blocks: [], ingress: report([]) });
    assert.ok(!buttonNamed(els.addslot, 'Add route').disabled);
    assert.ok(/Add a route/.test(els.emptytext.textContent));
    assert.strictEqual(els.empty.hidden, false);
  }

  // adding a manual route
  {
    fetches.length = 0;
    fetchReply = (url, opts) => ((opts && opts.method === 'POST') ? empty() : json(baseState()));
    const els = funnelEls();
    const ctrl = funnel.mount(els, { now: () => NOW });
    ctrl.update(baseState());
    buttonNamed(els.addslot, 'Add route').click();
    const form = findAll(els.panel, (n) => n.tag === 'form')[0];
    assert.ok(form);
    const inputs = findAll(form, (n) => n.tag === 'input' && n.getAttribute('type') === 'text');
    const [service, target, port] = inputs;
    service.value = 'wiki'; target.value = 'web-1'; port.value = 'abc';
    fire(form, 'submit');
    assert.strictEqual(sent('POST', '/funnel/routes').length, 0, 'a bad port is refused locally');
    assert.ok(/Port must be/.test(form.textContent));
    port.value = '9005';
    fire(form, 'submit');
    await flush();
    const post = sent('POST', '/funnel/routes');
    assert.strictEqual(post.length, 1);
    assert.deepStrictEqual(JSON.parse(post[0].body), { ns: 'demo', service: 'wiki', target: 'web-1', port: 9005, scheme: 'http', insecure_skip_verify: false });
    assert.strictEqual(els.panel.children.length, 0, 'the panel closes');
    assert.ok(/Added wiki\.demo/.test(els.notice.textContent));
    // the server's refusal stays in the panel
    fetchReply = (url, opts) => ((opts && opts.method === 'POST') ? json({ detail: 'the port is outside the namespace\'s allowed ports' }, 422) : json(baseState()));
    buttonNamed(els.addslot, 'Add route').click();
    const form2 = findAll(els.panel, (n) => n.tag === 'form')[0];
    const [s2, t2, p2] = findAll(form2, (n) => n.tag === 'input' && n.getAttribute('type') === 'text');
    s2.value = 'wiki'; t2.value = 'web-1'; p2.value = '80';
    fire(form2, 'submit');
    await flush();
    assert.ok(/outside the namespace/.test(form2.textContent));
    assert.strictEqual(els.panel.children.length, 1, 'the panel stays open on a refusal');
  }

  // CONFIG-TESTS
  console.log('funnel self-test: ok');
})().catch((e) => { console.error(e); process.exit(1); });
