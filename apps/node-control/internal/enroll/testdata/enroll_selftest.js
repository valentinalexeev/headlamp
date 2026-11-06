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
  // a tag name only, which is all the page asks for
  querySelector(tag) { return walk(this).slice(1).find((n) => n.tag === tag) || null; }
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
const createReqs = () => fetches.filter((f) => f.method === 'POST' && f.url === '/enroll/devices');

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
    // a reload that fails after a refusal does not replace the refusal
    const els3 = approveEls();
    const ctrl3 = approve.mount(els3, { go() {} });
    fetchReply = () => json({ ingresses: [bridge()] });
    await ctrl3.load('bridge-3');
    fetchReply = (url, opts) => (opts && opts.method === 'POST' ? json({ detail: 'the node that publishes this bridge is not the one that was reviewed' }, 409) : json({ detail: 'down' }, 502));
    buttonNamed(els3.actions, 'Approve bridge-3').click();
    await flush();
    assert.ok(/not the one that was reviewed/.test(els3.error.textContent), els3.error.textContent);
    assert.ok(!/Could not read/.test(els3.error.textContent));
    assert.strictEqual(fieldValue(els3, 'Node'), '21', 'the review keeps what it had');
  }

  // changedIdentityShowsCritical
  {
    fetches.length = 0;
    const went = [];
    fetchReply = (url, opts) => (opts && opts.method === 'POST' ? json({ ok: true }) : json({ ingresses: [bridge({ approval: 'changed', approved: true, detail: 'another node publishes these parameters', approved_fingerprint: '1111aaaa', approved_node_id: 18, approved_machine_key_prefix: 'mkey:4f1a9c02e7' })] }));
    const els = approveEls();
    const ctrl = approve.mount(els, { go: (u) => went.push(u) });
    await ctrl.load('bridge-3');
    assert.strictEqual(els.changed.hidden, false, 'critical note');
    assert.ok(/changed its host identity/.test(els.changed.textContent));
    assert.ok(/treat it as compromised/.test(els.changed.textContent));
    assert.ok(/another node publishes/.test(els.changed.textContent));
    assert.ok(/changed values are listed next to the approved ones/.test(els.changed.textContent));
    assert.ok(/Approved: 1111aaaa/.test(els.changed.textContent) && /Now: 7e3b91c40fa2d86e15b7c3409a1d62ef/.test(els.changed.textContent), 'fingerprints side by side');
    assert.ok(/Approved: 18/.test(els.changed.textContent) && /Now: 21/.test(els.changed.textContent), 'node ids side by side');
    assert.ok(!/Machine key/.test(els.changed.textContent.replace(/An approval keeps[^]*$/, '')), 'a value that did not change is not listed');
    assert.ok(/public key and short id are shown only as they are now/.test(els.changed.textContent));
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

  // changedIdentityNamesWhatDiffers: the sentence says which values differ, not always the fingerprint
  {
    const sentence = async (extra) => {
      fetchReply = () => json({ ingresses: [bridge(Object.assign({ approval: 'changed', approved: true, approved_fingerprint: '7e3b91c40fa2d86e15b7c3409a1d62ef', approved_node_id: 21, approved_machine_key_prefix: 'mkey:4f1a9c02e7' }, extra))] });
      const els = approveEls();
      await approve.mount(els, {}).load('bridge-3');
      return els.changed.textContent;
    };
    const FP = { approved_fingerprint: '1111aaaa' }, NODE = { approved_node_id: 18 }, MK = { approved_machine_key_prefix: 'mkey:0000000000' };
    assert.ok(/bridge-3 was approved with a different node\. /.test(await sentence(NODE)));
    assert.ok(/bridge-3 was approved with a different machine key\. /.test(await sentence(MK)));
    assert.ok(/bridge-3 was approved with a different fingerprint\. /.test(await sentence(FP)));
    assert.ok(/a different node and machine key\. /.test(await sentence(Object.assign({}, NODE, MK))));
    assert.ok(/a different fingerprint, node and machine key\. /.test(await sentence(Object.assign({}, FP, NODE, MK))));
    assert.ok(!/different fingerprint/.test(await sentence(NODE)), 'the fingerprint did not change');
    // an approval from before node pinning has nothing to compare: nothing is claimed to differ or to be listed
    const old = await sentence({ approved_node_id: 0, approved_machine_key_prefix: '', detail: 'approval predates node pinning, approve again' });
    assert.ok(/bridge-3 no longer matches what was approved\. /.test(old), old);
    assert.ok(!/was approved with a different/.test(old) && !/listed next to/.test(old), old);
    assert.ok(/treat it as compromised/.test(old));
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


  if (!devices) { console.log('enroll self-test ok (approval only)'); return; }

  // ---------------------------------------------------------------- Devices
  const NOW = Date.parse('2026-11-01T12:00:00Z');
  const sec = (ms) => Math.floor(ms / 1000);
  const DAYMS = 86400000;
  const dev = (id, name, extra) => Object.assign({
    id, name, owner: 'sam', client: 'karing', expires_at: sec(NOW + 60 * DAYMS), revoked: false, traffic: { up: 1024, down: 2048 },
    enrolled_at: sec(NOW - 5 * DAYMS), enrol_count: 1, last_active: sec(NOW - 3600000),
  }, extra);
  const D = {
    active: dev(1, 'phone-1'),
    never: dev(2, 'tablet-3', { enrol_count: 0, enrolled_at: 0, last_active: 0, traffic: null }),
    expiring: dev(3, 'phone-4', { expires_at: sec(NOW + 4 * DAYMS) }),
    expired: dev(4, 'laptop-2', { expires_at: sec(NOW - DAYMS) }),
    revoked: dev(5, 'phone-3', { revoked: true }),
  };
  const mk = (tag) => { const n = new FakeNode(tag); n.isRoot = true; return n; };
  function pageEls() {
    const els = { sub: mk('p'), chips: mk('div'), error: mk('div'), notice: mk('div'), syncnote: mk('section'), empty: mk('div'), tablewrap: mk('div'),
      rows: mk('tbody'), foot: mk('p'), traffic: mk('th'), drawer: mk('div') };
    ['error', 'notice', 'syncnote', 'empty', 'chips'].forEach((k) => { els[k].hidden = true; });
    return els;
  }
  const polls = [];
  const copied = [];
  const laters = [];
  function setup(opts) {
    opts = opts || {};
    global.window.location.search = opts.search || '';
    global.window.location.href = 'http://localhost/enroll' + (opts.search || '');
    polls.length = 0; copied.length = 0; laters.length = 0; fetches.length = 0; replaced.length = 0;
    const els = pageEls();
    const ctrl = devices.mount(els, {
      now: () => NOW,
      poll: (fn, ms) => { const p = { fn, ms, stopped: false, stop() { p.stopped = true; } }; polls.push(p); return p; },
      copy: (text) => { copied.push(text); return Promise.resolve(opts.copyFails ? false : true); },
      later: (fn) => laters.push(fn),
    });
    ctrl.loadDevices = () => Promise.resolve();
    return { els, ctrl };
  }
  const update = (ctrl, list, extra) => ctrl.update(Object.assign({ devices: list, sync: { pending: false, state: 'ok', since: null }, ingresses: [], pending_ingresses: [], stats_partial: false }, extra));
  const rowTexts = (els) => findAll(els.rows, (n) => n.tag === 'tr').map((tr) => tr.children.map((c) => c.textContent));
  const rowOf = (els, name) => findAll(els.rows, (n) => n.tag === 'tr' && n.children[0] && n.children[0].textContent === name)[0];
  const rowBtn = (els, name, label) => buttonNamed(rowOf(els, name), label);
  const chip = (els, label) => findAll(els.chips, (n) => n.tag === 'button' && n.textContent === label)[0];
  const drawerNode = (els) => findAll(els.drawer, (n) => n.tag === 'aside')[0];
  const nowIn = (n, days) => sec(NOW + days * DAYMS);

  // actionsMatchState
  {
    const labels = (d) => devices.actionsFor(d, NOW).map((a) => a.label);
    assert.deepStrictEqual(labels(D.revoked), ['Delete']);
    assert.deepStrictEqual(labels(D.expired), ['Extend', 'Revoke']);
    assert.deepStrictEqual(labels(D.never), ['New link', 'Extend', 'Revoke']);
    assert.deepStrictEqual(labels(D.active), ['Extend', 'Revoke']);
    assert.deepStrictEqual(labels(D.expiring), ['Extend', 'Revoke']);
    assert.ok(devices.actionsFor(D.revoked, NOW)[0].danger && devices.actionsFor(D.active, NOW)[1].danger);
    assert.ok(devices.actionsFor(D.never, NOW).every((a) => typeof a.icon === 'string' && a.icon));
    const word = (d) => devices.stateOf(d, NOW).word;
    assert.strictEqual(word(D.active), 'Active');
    assert.strictEqual(word(D.never), 'Link not used yet');
    assert.strictEqual(word(D.expiring), 'Expires in 4 days');
    assert.strictEqual(word(D.expired), 'Expired');
    assert.strictEqual(word(D.revoked), 'Revoked');
    assert.strictEqual(devices.stateOf(D.never, NOW).level, 'warn');
    assert.strictEqual(devices.stateOf(D.active, NOW).level, 'ok');
    assert.strictEqual(devices.stateOf(D.revoked, NOW).level, 'off');
    assert.strictEqual(word(dev(9, 'x', { expires_at: sec(NOW + 3600000) })), 'Expires today');
    assert.strictEqual(word(dev(9, 'x', { expires_at: sec(NOW + 20 * 3600000) })), 'Expires today');
    assert.strictEqual(word(dev(9, 'x', { expires_at: sec(NOW + 30 * 3600000) })), 'Expires in 2 days');
  }

  // revokedHiddenByDefault, filters, filterInURL, summary
  {
    const { els, ctrl } = setup();
    update(ctrl, Object.values(D), { stats_partial: false });
    const names = () => rowTexts(els).map((r) => r[0]).sort();
    assert.deepStrictEqual(names(), ['laptop-2', 'phone-1', 'phone-4', 'tablet-3'], 'revoked is hidden by default');
    assert.strictEqual(els.sub.textContent, '3 active, 2 seen today');
    assert.strictEqual(els.traffic.textContent, 'Traffic');
    const first = rowTexts(els)[0];
    assert.strictEqual(first[0], 'laptop-2', 'the expired device is listed first');
    const r1 = rowTexts(els).find((r) => r[0] === 'phone-1');
    assert.deepStrictEqual(r1.slice(1, 4), ['sam', 'karing', 'Active']);
    assert.strictEqual(r1[5], '3.0 KB');
    assert.strictEqual(rowTexts(els).find((r) => r[0] === 'tablet-3')[4], 'Never');
    assert.strictEqual(rowTexts(els).find((r) => r[0] === 'tablet-3')[5], '-');
    chip(els, 'Revoked').click();
    assert.deepStrictEqual(names(), ['phone-3']);
    assert.strictEqual(rowTexts(els)[0][6], 'n/a');
    assert.ok(/filter=revoked/.test(replaced[replaced.length - 1]), 'filter is in the URL: ' + replaced);
    chip(els, 'Expiring soon').click();
    assert.deepStrictEqual(names(), ['phone-4']);
    chip(els, 'Never used').click();
    assert.deepStrictEqual(names(), ['tablet-3']);
    chip(els, 'All').click();
    assert.strictEqual(names().length, 5);
    chip(els, 'Active').click();
    assert.ok(!/filter=/.test(replaced[replaced.length - 1]), 'the default filter leaves the URL: ' + replaced[replaced.length - 1]);
    assert.strictEqual(chip(els, 'Active').getAttribute('aria-pressed'), 'true');
    assert.strictEqual(chip(els, 'All').getAttribute('aria-pressed'), 'false');
    update(ctrl, [D.active], { stats_partial: true });
    assert.strictEqual(els.traffic.textContent, 'Traffic*');
    assert.ok(els.traffic.getAttribute('title'));
    chip(els, 'Never used').click();
    assert.ok(/No devices match/.test(els.rows.textContent));
  }
  {
    const { els } = (() => { const x = setup({ search: '?filter=expiring' }); update(x.ctrl, Object.values(D)); return x; })();
    assert.strictEqual(chip(els, 'Expiring soon').getAttribute('aria-pressed'), 'true');
    assert.deepStrictEqual(rowTexts(els).map((r) => r[0]), ['phone-4'], 'the link from Attention opens the filter');
    const bad = setup({ search: '?filter=nonsense' });
    update(bad.ctrl, Object.values(D));
    assert.strictEqual(chip(bad.els, 'Active').getAttribute('aria-pressed'), 'true');
  }

  // emptyState
  {
    const { els, ctrl } = setup();
    update(ctrl, []);
    assert.strictEqual(els.empty.hidden, false);
    assert.strictEqual(els.tablewrap.hidden, true);
    assert.strictEqual(els.chips.hidden, true);
    buttonNamed(els.empty, 'Enrol device').click();
    assert.ok(drawerNode(els), 'the empty state opens the panel');
    update(ctrl, [D.active]);
    assert.strictEqual(els.empty.hidden, true);
    assert.strictEqual(els.tablewrap.hidden, false);
  }

  // enrolParamOpensPanel
  {
    const { els } = setup({ search: '?enrol=1' });
    const aside = drawerNode(els);
    assert.ok(aside, 'the panel is open on arrival');
    assert.strictEqual(aside.getAttribute('role'), 'dialog');
    assert.strictEqual(document.activeElement.tag, 'input', 'focus starts on the first field');
    // Tab stays inside the panel: from the last control it wraps to the first, and back
    const ctl = findAll(aside, (n) => ['button', 'input', 'select'].includes(n.tag));
    document.activeElement = ctl[ctl.length - 1];
    fire(aside, 'keydown', { key: 'Tab' });
    assert.strictEqual(document.activeElement, ctl[0]);
    fire(aside, 'keydown', { key: 'Tab', shiftKey: true });
    assert.strictEqual(document.activeElement, ctl[ctl.length - 1]);
    const none = setup();
    assert.ok(!drawerNode(none.els));
  }

  // enrolRequiresNameAndOwner, expiryDefaults90Days
  {
    const { els, ctrl } = setup({ search: '?enrol=1' });
    update(ctrl, []);
    const aside = drawerNode(els);
    const form = findAll(aside, (n) => n.tag === 'form')[0];
    const inputs = findAll(aside, (n) => n.tag === 'input');
    const [name, owner, expires] = inputs;
    assert.strictEqual(expires.getAttribute('type'), 'date');
    // 90 days
    const diff = (devices.endOfDay(expires.value) * 1000 - NOW) / DAYMS;
    assert.ok(diff > 89 && diff < 91.1, 'default expiry is about 90 days out: ' + diff);
    assert.strictEqual(expires.value, devices.defaultExpiry(NOW));
    assert.ok(/9[01] days from today/.test(aside.textContent), aside.textContent);
    assert.ok(findAll(aside, (n) => n.tag === 'select').length === 1);
    fetches.length = 0;
    fire(form, 'submit');
    assert.strictEqual(createReqs().length, 0, 'no request without a name and an owner');
    assert.ok(/Name is required/.test(aside.textContent) && /Owner is required/.test(aside.textContent));
    assert.strictEqual(name.getAttribute('aria-invalid'), 'true');
    assert.strictEqual(document.activeElement, name, 'focus goes to the first problem');
    name.value = 'tablet-4';
    fire(form, 'submit');
    assert.strictEqual(createReqs().length, 0);
    assert.ok(!/Name is required/.test(aside.textContent) && /Owner is required/.test(aside.textContent));
    assert.strictEqual(document.activeElement, owner);
    owner.value = '  ';
    fire(form, 'submit');
    assert.strictEqual(createReqs().length, 0, 'blank is not an owner');
    owner.value = 'jo';
    expires.value = '2020-01-01';
    fire(form, 'submit');
    assert.strictEqual(createReqs().length, 0, 'a past expiry is refused');
    assert.ok(/future/.test(aside.textContent));
  }

  // clientDefaultsToLastUsed (storage works, is empty, holds junk, throws)
  {
    const store = { enrollClient: 'clash' };
    const saved = [];
    global.window.localStorage = { getItem: (k) => (k in store ? store[k] : null), setItem: (k, v) => { saved.push([k, v]); store[k] = v; } };
    let x = setup({ search: '?enrol=1' });
    let select = findAll(drawerNode(x.els), (n) => n.tag === 'select')[0];
    assert.strictEqual(select.value, 'clash');
    store.enrollClient = 'bogus';
    x = setup({ search: '?enrol=1' });
    assert.strictEqual(findAll(drawerNode(x.els), (n) => n.tag === 'select')[0].value, 'karing');
    // the choice is stored when a device is made
    fetchReply = (url, opts) => (opts && opts.method === 'POST' && url === '/enroll/devices' ? json({ id: 7, subscription_url: 'https://enrol.example.net/sub/S', sync: { pending: false }, ingresses: [] })
      : json({ url: 'https://enrol.example.net/e/T' }));
    select = findAll(drawerNode(x.els), (n) => n.tag === 'select')[0];
    select.value = 'sing-box';
    const ins = findAll(drawerNode(x.els), (n) => n.tag === 'input');
    ins[0].value = 'a'; ins[1].value = 'b';
    fire(findAll(drawerNode(x.els), (n) => n.tag === 'form')[0], 'submit');
    await flush();
    assert.deepStrictEqual(saved[saved.length - 1], ['enrollClient', 'sing-box']);
    assert.deepStrictEqual(JSON.parse(createReqs()[0].body).client, 'sing-box');
    // storage that throws on every use: defaults apply and creating still works
    delete global.window.localStorage;
    Object.defineProperty(global.window, 'localStorage', { configurable: true, get() { throw new Error('blocked'); } });
    x = setup({ search: '?enrol=1' });
    assert.strictEqual(findAll(drawerNode(x.els), (n) => n.tag === 'select')[0].value, 'karing');
    const ins2 = findAll(drawerNode(x.els), (n) => n.tag === 'input');
    ins2[0].value = 'a'; ins2[1].value = 'b';
    fetches.length = 0;
    fire(findAll(drawerNode(x.els), (n) => n.tag === 'form')[0], 'submit');
    await flush();
    assert.strictEqual(createReqs().length, 1, 'a blocked storage does not stop the flow');
    delete global.window.localStorage;
    global.window.localStorage = { getItem: () => null, setItem() {} };
  }

  // the whole flow: create, link, waiting, enrolled; linkNeverRendersAsAnchor, linkNeverInURL
  {
    const LINK = 'https://enrol.example.net/e/LINKTOKEN9';
    const SUB = 'https://enrol.example.net/sub/SUBSECRET9';
    let calls = [];
    fetchReply = (url, opts) => {
      calls.push((opts && opts.method) + ' ' + url);
      if (url === '/enroll/devices' && opts.method === 'POST') { return json({ id: 7, subscription_url: SUB, sync_pending: false, sync: { pending: false }, ingresses: [] }); }
      if (url === '/enroll/devices/7/links') { return json({ url: LINK }); }
      return json({});
    };
    global.qrcode = () => ({ addData() {}, make() {}, getModuleCount: () => 21, isDark: (r, c) => (r + c) % 2 === 0 });
    const { els, ctrl } = setup({ search: '?enrol=1' });
    update(ctrl, [D.active]);
    const aside = drawerNode(els);
    const [name, owner] = findAll(aside, (n) => n.tag === 'input');
    name.value = 'tablet-4'; owner.value = 'jo';
    fire(findAll(aside, (n) => n.tag === 'form')[0], 'submit');
    await flush();
    const created = JSON.parse(createReqs()[0].body);
    assert.deepStrictEqual(Object.keys(created).sort(), ['client', 'expires_at', 'name', 'owner']);
    assert.strictEqual(created.name, 'tablet-4');
    assert.strictEqual(created.owner, 'jo');
    assert.strictEqual(created.expires_at, devices.endOfDay(devices.defaultExpiry(NOW)));
    assert.deepStrictEqual(JSON.parse(sent('POST', '/enroll/devices/7/links')[0].body), { ttl: 86400, max_uses: 1 });
    // the form and its focused button are gone: focus lands on the copy button, not on the page
    assert.strictEqual(global.document.activeElement, buttonNamed(aside, 'Copy link'), 'focus is on Copy link');
    const secrets = findAll(aside, (n) => hasClass(n, 'secret'));
    assert.deepStrictEqual(secrets.map((n) => n.textContent), [LINK, SUB]);
    const manual = findAll(aside, (n) => n.tag === 'details');
    assert.strictEqual(manual.length, 1);
    assert.deepStrictEqual(findAll(manual[0], (n) => hasClass(n, 'secret')).map((n) => n.textContent), [SUB], 'the subscription URL is only in Manual setup');
    assert.ok(/Manual setup: subscription URL/.test(aside.textContent));
    assert.ok(/tablet-4 created for jo/.test(aside.textContent));
    assert.ok(/Waiting for first use/.test(aside.textContent));
    assert.ok(/Works once, valid for 24 hours/.test(aside.textContent));
    assert.ok(findAll(aside, (n) => n.tag === 'svg').length > 0, 'QR code is drawn');
    // linkNeverRendersAsAnchor
    assert.strictEqual(findAll(aside, (n) => n.tag === 'a').length, 0, 'no anchors in the panel');
    walk(aside).forEach((n) => Object.values(n.attrs).forEach((v) => assert.ok(!v.includes('LINKTOKEN9') && !v.includes('SUBSECRET9'), 'a secret in an attribute')));
    // linkNeverInURL, and not in the table or the notices
    assert.ok(fetches.every((f) => !f.url.includes('LINKTOKEN9') && !f.url.includes('SUBSECRET9')));
    assert.ok(replaced.every((u) => !u.includes('LINKTOKEN9') && !u.includes('SUBSECRET9')));
    [els.rows, els.error, els.notice, els.syncnote, els.sub].forEach((n) => assert.ok(!n.textContent.includes('TOKEN9') && !n.textContent.includes('SECRET9')));
    // the wait polls for the first use
    assert.strictEqual(polls.length, 1);
    assert.strictEqual(polls[0].ms, 4000);
    assert.strictEqual(polls[0].stopped, false);
    // copy
    buttonNamed(aside, 'Copy link').click();
    await flush();
    assert.deepStrictEqual(copied, [LINK]);
    assert.ok(buttonNamed(aside, 'Copied'), 'the button says so');
    laters.forEach((f) => f());
    assert.ok(buttonNamed(aside, 'Copy link'));
    // waitingForFirstUseUpdates: the device shows a first use in the next read
    update(ctrl, [D.active, dev(7, 'tablet-4', { owner: 'jo', enrol_count: 0, enrolled_at: 0, last_active: 0 })]);
    assert.ok(/Waiting for first use/.test(aside.textContent), 'still waiting while unused');
    assert.strictEqual(polls[0].stopped, false);
    update(ctrl, [D.active, dev(7, 'tablet-4', { owner: 'jo', enrol_count: 1, enrolled_at: sec(NOW - 60000), last_active: 0 })]);
    assert.ok(!/Waiting for first use/.test(aside.textContent), 'no longer waiting');
    assert.ok(/Enrolled/.test(aside.textContent));
    assert.strictEqual(polls[0].stopped, true, 'the poll stops once enrolled');
    assert.ok(!findAll(aside, (n) => hasClass(n, 'secret')).some((n) => n.textContent === LINK && !n.parentNode.hidden), 'the used-up link is hidden');
    // another device can be enrolled from the same panel; the finished wait does not poll again
    const held = ctrl.state.drawer;
    assert.strictEqual(held.link, LINK);
    assert.strictEqual(held.subscription, SUB);
    buttonNamed(aside, 'Enrol another device').click();
    assert.ok(findAll(aside, (n) => n.tag === 'form').length === 1 && findAll(aside, (n) => n.tag === 'input')[0].value === '', 'a blank form');
    assert.strictEqual(polls.length, 1);
    assert.strictEqual(held.link, '', 'the blank form does not keep the last link');
    assert.strictEqual(held.subscription, '', 'nor the last subscription URL');
    // Escape closes and removes the param
    fire(aside, 'keydown', { key: 'Escape' });
    assert.ok(!drawerNode(els));
    assert.ok(!/enrol=/.test(replaced[replaced.length - 1] || ''), 'enrol leaves the URL: ' + replaced[replaced.length - 1]);
    delete global.qrcode;
  }

  // closing the panel stops the poll; the create can fail; the link can fail
  {
    fetchReply = (url, opts) => {
      if (url === '/enroll/devices' && opts.method === 'POST') { return json({ id: 8, subscription_url: 'https://enrol.example.net/sub/ONLY', sync: null, ingresses: [] }); }
      return json({ detail: 'link service down' }, 500);
    };
    const { els, ctrl } = setup({ search: '?enrol=1' });
    update(ctrl, []);
    let aside = drawerNode(els);
    let ins = findAll(aside, (n) => n.tag === 'input');
    ins[0].value = 'x'; ins[1].value = 'y';
    fire(findAll(aside, (n) => n.tag === 'form')[0], 'submit');
    await flush();
    assert.ok(/The one-time link could not be made/.test(aside.textContent));
    assert.ok(/link service down/.test(aside.textContent));
    assert.strictEqual(findAll(aside, (n) => n.tag === 'details')[0].getAttribute('open'), '', 'Manual setup is open when there is no link');
    assert.strictEqual(polls.length, 0, 'no poll without a link');
    assert.ok(buttonNamed(aside, 'Copy subscription URL'));
    findAll(aside, (n) => n.tag === 'button' && n.getAttribute('aria-label') === 'Close')[0].click();
    assert.ok(!drawerNode(els));

    fetchReply = () => json({ detail: 'name already used' }, 409);
    const y = setup({ search: '?enrol=1' });
    aside = drawerNode(y.els);
    ins = findAll(aside, (n) => n.tag === 'input');
    ins[0].value = 'x'; ins[1].value = 'y';
    fire(findAll(aside, (n) => n.tag === 'form')[0], 'submit');
    await flush();
    assert.ok(/name already used/.test(aside.textContent));
    assert.strictEqual(buttonNamed(aside, 'Create and get link').disabled, false, 'the form can be sent again');

    // close stops the poll
    fetchReply = (url, opts) => (url === '/enroll/devices' && opts.method === 'POST' ? json({ id: 9, subscription_url: 'https://e.example.net/sub/Q', sync: null, ingresses: [] }) : json({ url: 'https://e.example.net/e/Z' }));
    const z = setup({ search: '?enrol=1' });
    aside = drawerNode(z.els);
    ins = findAll(aside, (n) => n.tag === 'input');
    ins[0].value = 'x'; ins[1].value = 'y';
    fire(findAll(aside, (n) => n.tag === 'form')[0], 'submit');
    await flush();
    assert.strictEqual(polls.length, 1);
    const closed = z.ctrl.state.drawer;
    assert.strictEqual(findAll(closed.aside, (n) => hasClass(n, 'secret')).length, 2);
    fire(findAll(z.els.drawer, (n) => hasClass(n, 'scrim'))[0], 'click');
    assert.ok(!drawerNode(z.els));
    assert.strictEqual(polls[0].stopped, true, 'closing the panel stops the poll');
    // closing the panel forgets what it showed
    assert.strictEqual(closed.link, '', 'the link is gone from the closed panel');
    assert.strictEqual(closed.subscription, '', 'the subscription URL too');
    assert.strictEqual(closed.linkBox, null);
    assert.strictEqual(findAll(closed.aside, (n) => hasClass(n, 'secret')).length, 0, 'no node keeps them');
  }

  // the panel cannot be closed while the device is being made; an exception on the success path shows an error
  {
    let release;
    const gate = new Promise((r) => { release = r; });
    global.fetch = (url, opts) => {
      fetches.push({ url, method: (opts && opts.method) || 'GET', body: opts && opts.body });
      const respond = (r) => ({ ok: r.ok, status: r.status, text: () => Promise.resolve(r.body) });
      if (url === '/enroll/devices' && opts.method === 'POST') { return gate.then(() => respond(json({ id: 11, subscription_url: 'https://e.example.net/sub/K', sync: null, ingresses: [] }))); }
      return Promise.resolve(respond(json({ url: 'https://e.example.net/e/KL' })));
    };
    const { els, ctrl } = setup({ search: '?enrol=1' });
    update(ctrl, []);
    const aside = drawerNode(els);
    const ins = findAll(aside, (n) => n.tag === 'input');
    ins[0].value = 'x'; ins[1].value = 'y';
    fire(findAll(aside, (n) => n.tag === 'form')[0], 'submit');
    fire(aside, 'keydown', { key: 'Escape' });
    fire(findAll(els.drawer, (n) => hasClass(n, 'scrim'))[0], 'click');
    findAll(aside, (n) => n.tag === 'button' && n.getAttribute('aria-label') === 'Close')[0].click();
    assert.ok(drawerNode(els), 'Esc, scrim and Close are ignored while creating');
    release();
    await flush();
    assert.ok(/Copy link/.test(aside.textContent), 'the link arrived in the panel');
    fire(aside, 'keydown', { key: 'Escape' });
    assert.ok(!drawerNode(els), 'closing works again afterwards');

    // an exception after the device was made: an error, and no second device from the form
    global.fetch = (url, opts) => {
      fetches.push({ url, method: (opts && opts.method) || 'GET', body: opts && opts.body });
      const r = url === '/enroll/devices' && opts.method === 'POST' ? json({ id: 12, subscription_url: 'https://e.example.net/sub/K2', sync: null, ingresses: [] }) : json({ url: 'https://e.example.net/e/K2L' });
      return Promise.resolve({ ok: r.ok, status: r.status, text: () => Promise.resolve(r.body) });
    };
    const y = setup({ search: '?enrol=1' });
    y.ctrl.loadDevices = () => { throw new Error('boom'); };
    const aside2 = drawerNode(y.els);
    const ins2 = findAll(aside2, (n) => n.tag === 'input');
    ins2[0].value = 'x'; ins2[1].value = 'y';
    fire(findAll(aside2, (n) => n.tag === 'form')[0], 'submit');
    await flush();
    assert.ok(/The device was created, but this panel could not show its link: boom/.test(aside2.textContent), aside2.textContent);
    assert.strictEqual(buttonNamed(aside2, 'Created').disabled, true, 'the form cannot make a second device');
    fire(aside2, 'keydown', { key: 'Escape' });
    assert.ok(!drawerNode(y.els), 'the panel can be closed after the failure');
    global.fetch = (url, opts) => {
      fetches.push({ url, method: (opts && opts.method) || 'GET', body: opts && opts.body });
      const r = fetchReply(url, opts);
      if (r instanceof Error) { return Promise.reject(r); }
      return Promise.resolve({ ok: r.ok, status: r.status, text: () => Promise.resolve(r.body) });
    };
  }

  // New link opens the panel for an existing device, with no subscription URL
  {
    fetchReply = (url) => (url === '/enroll/devices/2/links' ? json({ url: 'https://e.example.net/e/NL' }) : json({}));
    const { els, ctrl } = setup();
    update(ctrl, [D.never]);
    rowBtn(els, 'tablet-3', 'New link').click();
    await flush();
    const aside = drawerNode(els);
    assert.ok(aside);
    assert.ok(/One-time link for tablet-3/.test(aside.textContent));
    assert.ok(/Waiting for first use/.test(aside.textContent));
    assert.ok(!/Manual setup/.test(aside.textContent));
    assert.deepStrictEqual(findAll(aside, (n) => hasClass(n, 'secret')).map((n) => n.textContent), ['https://e.example.net/e/NL']);
    assert.strictEqual(polls.length, 1);
    assert.ok(/enrol=1/.test(replaced[replaced.length - 1]));
    const held = ctrl.state.drawer;
    fire(aside, 'keydown', { key: 'Escape' });
    assert.strictEqual(held.link, '', 'a link from New link is forgotten on close as well');
  }

  // extendOffers30And90, no prompt()
  {
    global.prompt = () => { throw new Error('prompt() must not be used'); };
    global.confirm = () => { throw new Error('confirm() must not be used'); };
    fetchReply = () => json({ ok: true, sync_pending: false, sync: { pending: false }, ingresses: [] });
    const { els, ctrl } = setup();
    update(ctrl, [D.expiring, D.active]);
    rowBtn(els, 'phone-4', 'Extend').click();
    const panel = findAll(els.rows, (n) => hasClass(n, 'confirm'))[0];
    assert.ok(panel, 'a panel opens under the row');
    assert.ok(/^30 days, to /.test(global.document.activeElement.textContent), 'focus lands on the first extend offer');
    const labels = findAll(panel, (n) => n.tag === 'button').map((b) => b.textContent);
    assert.ok(labels.some((l) => /^30 days, to /.test(l)) && labels.some((l) => /^90 days, to /.test(l)), labels.join('|'));
    const date = findAll(panel, (n) => n.tag === 'input')[0];
    assert.strictEqual(date.getAttribute('type'), 'date', 'a date field as well');
    assert.ok(labels.includes('Set date') && labels.includes('Cancel'));
    // a past date is refused, a future one is sent
    date.value = '2020-01-01';
    buttonNamed(panel, 'Set date').click();
    assert.strictEqual(sent('POST', '/extend').length, 0);
    assert.ok(/future/.test(panel.textContent));
    fetches.length = 0;
    findAll(panel, (n) => n.tag === 'button' && /^30 days/.test(n.textContent))[0].click();
    await flush();
    const body = JSON.parse(sent('POST', '/enroll/devices/3/extend')[0].body);
    assert.strictEqual(body.expires_at, devices.extendTo(D.expiring, 30, NOW));
    assert.ok(body.expires_at > D.expiring.expires_at + 29 * 86400 && body.expires_at < D.expiring.expires_at + 31 * 86400);
    assert.strictEqual(devices.extendTo(D.expired, 30, NOW), devices.extendTo(dev(0, 'z', { expires_at: sec(NOW) }), 30, NOW), 'an expired device is extended from now');
    // 90 and a custom date
    const x = setup();
    update(x.ctrl, [D.active]);
    rowBtn(x.els, 'phone-1', 'Extend').click();
    const p2 = findAll(x.els.rows, (n) => hasClass(n, 'confirm'))[0];
    findAll(p2, (n) => n.tag === 'input')[0].value = '2027-03-01';
    fetches.length = 0;
    buttonNamed(p2, 'Set date').click();
    await flush();
    assert.strictEqual(JSON.parse(sent('POST', '/enroll/devices/1/extend')[0].body).expires_at, devices.endOfDay('2027-03-01'));
    assert.ok(/Extended phone-1/.test(x.els.notice.textContent));
    // Cancel closes it
    const c = setup();
    update(c.ctrl, [D.active]);
    rowBtn(c.els, 'phone-1', 'Extend').click();
    buttonNamed(findAll(c.els.rows, (n) => hasClass(n, 'confirm'))[0], 'Cancel').click();
    assert.strictEqual(findAll(c.els.rows, (n) => hasClass(n, 'confirm')).length, 0);
  }

  // revoke and delete ask first; a pending confirmation survives a poll
  {
    fetchReply = () => json({ ok: true, sync_pending: false, sync: { pending: false }, ingresses: [] });
    const { els, ctrl } = setup();
    update(ctrl, [D.active, D.never, D.revoked]);
    chip(els, 'All').click();
    rowBtn(els, 'phone-1', 'Revoke').click();
    assert.strictEqual(sent('POST', '/revoke').length, 0, 'revoking needs a second answer');
    const panel = findAll(els.rows, (n) => hasClass(n, 'confirm'))[0];
    assert.ok(/Revoke phone-1\?/.test(panel.textContent));
    const cancelBtn = buttonNamed(panel, 'Keep');
    assert.ok(cancelBtn && document.activeElement === cancelBtn, 'focus starts on the safe answer');
    // a poll that brings new data leaves the open confirmation as it is
    update(ctrl, [D.active, D.never, D.revoked, dev(10, 'phone-9')]);
    const after = findAll(els.rows, (n) => hasClass(n, 'confirm'))[0];
    assert.strictEqual(after, panel, 'same panel after a poll');
    assert.strictEqual(document.activeElement, cancelBtn, 'focus is kept');
    buttonNamed(panel, 'Revoke phone-1').click();
    await flush();
    assert.strictEqual(sent('POST', '/enroll/devices/1/revoke').length, 1);
    assert.ok(/Revoked phone-1/.test(els.notice.textContent));
    // the table catches up once the confirmation is gone
    assert.ok(rowOf(els, 'phone-9'), 'the rebuild happened after the answer');
    // delete
    fetches.length = 0;
    rowBtn(els, 'phone-3', 'Delete').click();
    assert.strictEqual(sent('DELETE', '/enroll/devices/5').length, 0);
    const dp = findAll(els.rows, (n) => hasClass(n, 'confirm'))[0];
    assert.ok(/Delete phone-3\?/.test(dp.textContent) && /cannot be undone/.test(dp.textContent));
    buttonNamed(dp, 'Delete phone-3').click();
    await flush();
    assert.strictEqual(sent('DELETE', '/enroll/devices/5').length, 1);
    // an error is shown and the row buttons come back
    fetchReply = () => json({ detail: 'revoke the device before deleting it' }, 409);
    rowBtn(els, 'phone-3', 'Delete').click();
    buttonNamed(findAll(els.rows, (n) => hasClass(n, 'confirm'))[0], 'Delete phone-3').click();
    await flush();
    assert.ok(/Delete failed for phone-3: revoke the device/.test(els.error.textContent));
    assert.strictEqual(els.error.hidden, false);
    assert.strictEqual(rowBtn(els, 'phone-3', 'Delete').disabled, false);
  }

  // outOfSyncNoteNamesBridge
  {
    const { els, ctrl } = setup();
    const since = sec(NOW - 38 * 60000);
    update(ctrl, [D.active], {
      sync: { pending: true, state: 'unreachable', since },
      ingresses: [{ name: 'bridge-2', host: 'b2.example.net', online: true, sync: { pending: true, state: 'unreachable', since } },
        { name: 'bridge-1', host: 'b1.example.net', online: true, sync: { pending: false, state: 'ok', since: null } }],
    });
    assert.strictEqual(els.syncnote.hidden, false);
    const t = els.syncnote.textContent;
    assert.ok(/bridge-2 is out of sync/.test(t), t);
    assert.ok(!/bridge-1/.test(t), 'only the bridge that lags is named');
    assert.ok(/will not take effect on bridge-2 until it catches up/.test(t));
    assert.ok(/Agent unreachable/.test(t));
    assert.ok(/since 38 min ago/.test(t), t);
    assert.ok(/Retrying every 5 minutes\./.test(t), t);
    const link = findAll(els.syncnote, (n) => n.tag === 'a')[0];
    assert.strictEqual(link.getAttribute('href'), '/nodes?sel=bridge-2');
    assert.strictEqual(link.textContent, 'Open bridge');
    // several
    update(ctrl, [D.active], {
      sync: { pending: true, state: 'unreachable', since },
      ingresses: [{ name: 'bridge-1', sync: { pending: true, state: 'rejected', since } }, { name: 'bridge-2', sync: { pending: true, state: 'unreachable', since } }],
    });
    assert.ok(/bridge-1 and bridge-2 are out of sync/.test(els.syncnote.textContent));
    assert.deepStrictEqual(findAll(els.syncnote, (n) => n.tag === 'a').map((a) => a.getAttribute('href')), ['/nodes?sel=bridge-1', '/nodes?sel=bridge-2']);
    // one bridge host with no list
    update(ctrl, [D.active], { sync: { pending: true, state: 'error', since }, ingresses: [] });
    assert.ok(/The bridge host is out of sync/.test(els.syncnote.textContent));
    // cleared
    update(ctrl, [D.active]);
    assert.strictEqual(els.syncnote.hidden, true);
  }

  // text from the network is plain text, and a failed read keeps the last data with a note
  {
    const { els, ctrl } = setup();
    update(ctrl, [dev(1, 'bad‮name\u0007', { owner: 'o'.repeat(300) })]);
    assert.strictEqual(rowTexts(els)[0][0], 'badname');
    assert.strictEqual(rowTexts(els)[0][1].length, 100);
    ctrl.setFailed(true);
    assert.ok(/may be out of date/.test(els.sub.textContent));
    assert.strictEqual(rowTexts(els).length, 1);
    ctrl.setFailed(false);
    const fresh = setup();
    fresh.ctrl.setFailed(true);
    assert.ok(/Could not load the devices/.test(fresh.els.sub.textContent));
  }

  console.log('enroll self-test ok');
})().catch((e) => { console.error(e); process.exit(1); });
