// Run with: node tasks_page_selftest.js <hl.js> <tasks.js> <sessions.js>. Exits non-zero on a failed check.
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

const replaced = [];
global.document = {
  createElement: (t) => new FakeNode(t), createElementNS: (_, t) => new FakeNode(t), createTextNode: (s) => { const n = new FakeNode('#text'); n._text = s; return n; },
  getElementById: () => null, addEventListener() {}, hidden: false, activeElement: null,
};
global.window = {
  location: { search: '', href: 'http://localhost/tasks' },
  history: { replaceState(_a, _b, url) { replaced.push(url); global.window.location.search = url.includes('?') ? url.slice(url.indexOf('?')) : ''; global.window.location.href = 'http://localhost' + url; } },
  addEventListener() {},
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

require(path.resolve(process.argv[2]));
require(path.resolve(process.argv[3]));
require(path.resolve(process.argv[4]));
const hl = global.window.hl;
const tasks = hl.tasks;
const sessions = hl.sessions;
assert.ok(tasks && sessions, 'hl.tasks and hl.sessions are defined');

const NOW = Date.parse('2026-01-01T12:00:00Z');
const iso = (secAgo) => new Date(NOW - secAgo * 1000).toISOString();
const ahead = (sec) => new Date(NOW + sec * 1000).toISOString();
const mk = (tag) => { const n = new FakeNode(tag); n.isRoot = true; return n; };

// ----- pure helpers that survive from the old page
assert.strictEqual(hl.relTime(iso(300), NOW), '5 min ago');
assert.strictEqual(hl.relTime(ahead(180), NOW), 'in 3 min');
assert.strictEqual(hl.relTime('garbage', NOW), '');

assert.strictEqual(tasks.fmtDuration(250), '250 ms');
assert.strictEqual(tasks.fmtDuration(1500), '1.5 s');
assert.strictEqual(tasks.fmtDuration(125000), '2 min 5 s');
assert.strictEqual(tasks.fmtDuration(null), '');

assert.strictEqual(tasks.fmtValue(12.5), '12.5');
assert.strictEqual(tasks.fmtValue('<b>x</b>'), '<b>x</b>');
assert.strictEqual(tasks.fmtValue(null), 'null');
assert.strictEqual(tasks.fmtValue({ a: 1 }), '{"a":1}');
assert.ok(tasks.fmtValue({ a: 'y'.repeat(500) }).length <= 203);
assert.ok(tasks.fmtValue('z'.repeat(5000)).length <= 203, 'a long string from a runner is cut');
assert.ok(!/[‮\u0000]/.test(tasks.fmtValue('a‮b\u0000c')), 'control and bidi characters are dropped');

assert.strictEqual(tasks.targetText({ node: 'n1' }), 'node n1');
assert.strictEqual(tasks.targetText({ tags: ['tag:a', 'tag:b'], online: true }), 'tags tag:a, tag:b (online only)');

// ----- fixtures
const task = (id, extra) => Object.assign({ id, script: id + '.js', target: { node: 'runner-1' }, every: '30m0s', running: false, running_run: null,
  last_success: iso(2520), last_result: { ok: true }, streak: 0, attempt: 0, next_run: ahead(1080), stale: false }, extra);
const list = () => [
  task('balance-check'),
  task('login-check', { streak: 3, last_success: iso(4200), next_run: ahead(1140) }),
  task('portal-up', { last_success: iso(360) }),
  task('renewal-date', { stale: true, last_success: iso(90000) }),
  task('fresh', { last_success: null, last_result: null, next_run: null }),
];
const run = (id, state, secAgo, extra) => Object.assign({ id, task: 'login-check', node: 'runner-1', attempt: 1, state, reason: '', started: iso(secAgo), ended: iso(secAgo - 30),
  duration_ms: 30000, result: {}, script_sha: 'abcdef0123456789', notify_outcome: '' }, extra);
const runsFor = () => [
  run('r3', 'failed', 660, { reason: 'Timeout waiting for the sign-in form after 30 s' }),
  run('r2', 'failed', 1860, { reason: 'Timeout waiting for the sign-in form after 30 s' }),
  run('r1', 'succeeded', 4200, { duration_ms: 8200, notify_outcome: 'alert=sent' }),
];

function tasksEls() {
  const els = { sub: mk('p'), error: mk('div'), notice: mk('div'), stale: mk('div'), empty: mk('div'), emptytext: mk('p'), cols: mk('div'),
    rows: mk('tbody'), panel: mk('div') };
  els.error.hidden = els.notice.hidden = els.stale.hidden = els.empty.hidden = els.cols.hidden = true;
  return els;
}
const rowsOf = (els) => els.rows.children;
const rowFor = (els, id) => rowsOf(els).find((tr) => tr.children[0] && tr.children[0].textContent === id);
const names = (els) => rowsOf(els).map((tr) => tr.children[0].textContent);
const setSearch = (s) => { global.window.location.search = s; global.window.location.href = 'http://localhost/tasks' + s; };

(async () => {
  // failingSortsFirst: failing, then overdue, then the rest by name; every state has a shape and a word
  {
    const rows = tasks.taskRows(list());
    assert.deepStrictEqual(rows.map((r) => r.id), ['login-check', 'renewal-date', 'balance-check', 'fresh', 'portal-up']);
    const by = Object.fromEntries(rows.map((r) => [r.id, r]));
    assert.strictEqual(by['login-check'].word, 'Failing, 3 in a row');
    assert.strictEqual(by['login-check'].level, 'warn');
    assert.strictEqual(by['renewal-date'].word, 'Overdue');
    assert.strictEqual(by['renewal-date'].level, 'warn');
    assert.strictEqual(by['balance-check'].word, 'Passing');
    assert.strictEqual(by['balance-check'].level, 'ok');
    assert.strictEqual(by['fresh'].word, 'No success yet');
    assert.strictEqual(by['fresh'].level, 'off');
    assert.strictEqual(tasks.taskRows([task('x', { running: true })])[0].word, 'Running');
    assert.strictEqual(tasks.taskRows([task('x', { running: true, streak: 1 })])[0].word, 'Failing, 1 in a row (running)');
    assert.deepStrictEqual(tasks.taskRows(null), []);
    assert.deepStrictEqual(tasks.taskRows([{ id: 5 }, null]), []);

    fetchReply = (url) => (url.includes('/runs') ? json(runsFor()) : json(list()));
    const els = tasksEls();
    const ctrl = tasks.mount(els, { now: () => NOW });
    ctrl.update(list());
    assert.deepStrictEqual(names(els), ['login-check', 'renewal-date', 'balance-check', 'fresh', 'portal-up']);
    const st = findAll(rowFor(els, 'login-check'), (n) => hasClass(n, 'st'))[0];
    assert.ok(hasClass(st, 'warn') && findAll(st, (n) => hasClass(n, 'g') && hasClass(n, 'warn')).length === 1);
    assert.strictEqual(st.textContent, 'Failing, 3 in a row');
    assert.ok(!els.cols.hidden && els.empty.hidden);
    assert.strictEqual(els.sub.textContent, '2 of 5 passing', 'the same count the Overview line gives; a task with no success yet is not passing');
  }

  // selectedInURL: ?sel= picks the task, a click writes it back, an unknown id falls back to the first row
  {
    fetches.length = 0;
    replaced.length = 0;
    setSearch('?sel=balance-check');
    fetchReply = (url) => (url.includes('/runs') ? json(runsFor()) : json(list()));
    const els = tasksEls();
    const ctrl = tasks.mount(els, { now: () => NOW });
    ctrl.update(list());
    await flush();
    assert.ok(hasClass(rowFor(els, 'balance-check'), 'sel'));
    assert.ok(!hasClass(rowFor(els, 'login-check'), 'sel'));
    assert.strictEqual(findAll(els.panel, (n) => n.tag === 'h2')[0].textContent, 'balance-check');
    assert.strictEqual(sent('GET', '/tasks/balance-check/runs').length >= 1, true, 'the runs of the selected task are loaded');
    buttonNamed(rowFor(els, 'renewal-date'), 'renewal-date').click();
    assert.ok(hasClass(rowFor(els, 'renewal-date'), 'sel'));
    assert.strictEqual(replaced[replaced.length - 1], '/tasks?sel=renewal-date', 'the selection is in the URL');
    assert.strictEqual(findAll(els.panel, (n) => n.tag === 'h2')[0].textContent, 'renewal-date');
    setSearch('?sel=nope');
    const els2 = tasksEls();
    tasks.mount(els2, { now: () => NOW }).update(list());
    assert.ok(hasClass(rowFor(els2, 'login-check'), 'sel'), 'an unknown id falls back to the first row');
    setSearch('');
  }

  // lastErrorShown + recentRunsTable
  {
    fetchReply = (url) => (url.includes('/runs') ? json(runsFor()) : json(list()));
    const els = tasksEls();
    const ctrl = tasks.mount(els, { now: () => NOW });
    ctrl.update(list());
    await flush();
    const text = els.panel.textContent;
    assert.ok(text.includes('Timeout waiting for the sign-in form after 30 s'), 'the last failure reason is shown');
    const note = findAll(els.panel, (n) => hasClass(n, 'note'))[0];
    assert.ok(note && note.textContent.includes('10 min ago'), note && note.textContent);
    const trs = findAll(els.panel, (n) => n.tag === 'tr' && n.parentNode && n.parentNode.tag === 'tbody');
    assert.strictEqual(trs.length, 3, 'three recent runs');
    assert.ok(trs[0].textContent.includes('Failed') && trs[0].textContent.includes('30.0 s'));
    assert.ok(trs[2].textContent.includes('Passed') && trs[2].textContent.includes('8.2 s'));
    assert.ok(text.includes('login-check.js') && text.includes('node runner-1'));
    // a healthy task shows no error note
    ctrl.select('balance-check');
    await flush();
    assert.strictEqual(findAll(els.panel, (n) => hasClass(n, 'note') && n.textContent.includes('Timeout')).length, 0);
    // a running run offers Cancel, which calls the unchanged API
    ctrl.select('login-check');
    fetchReply = (url, o) => {
      if (o && o.method === 'DELETE') { return empty(204); }
      return url.includes('/runs') ? json([run('r9', 'running', 20, { ended: null, duration_ms: null })]) : json(list());
    };
    ctrl.update(list());
    await flush();
    const cancel = buttonNamed(els.panel, 'Cancel');
    assert.ok(cancel, 'a running run can be cancelled');
    cancel.click();
    await flush();
    assert.strictEqual(sent('DELETE', '/runs/r9').length, 1);
  }

  // an unsafe or hostile string from a runner stays text
  {
    fetchReply = (url) => (url.includes('/runs') ? json([run('r1', 'failed', 100, { reason: '<img src=x onerror=alert(1)>' })]) : json(list()));
    const els = tasksEls();
    const ctrl = tasks.mount(els, { now: () => NOW });
    ctrl.update([task('login-check', { streak: 1, last_result: { msg: '<b>bold</b>' + 'q'.repeat(900) } })]);
    await flush();
    assert.ok(els.panel.textContent.includes('<img src=x onerror=alert(1)>'));
    assert.strictEqual(findAll(els.panel, (n) => n.tag === 'img' || n.tag === 'b').length, 0);
    assert.ok(els.panel.textContent.length < 1500, 'a result value is bounded');
  }

  // runNowBusyState
  {
    let release;
    fetches.length = 0;
    fetchReply = (url, o) => {
      if (o && o.method === 'POST') { return { hold: true }; }
      return url.includes('/runs') ? json(runsFor()) : json(list());
    };
    const realFetch = global.fetch;
    global.fetch = (url, o) => {
      if (o && o.method === 'POST') {
        fetches.push({ url, method: 'POST', body: o.body });
        return new Promise((res) => { release = () => res({ ok: true, status: 202, text: () => Promise.resolve('{"run_id":"abcdef1234567890"}') }); });
      }
      return realFetch(url, o);
    };
    const els = tasksEls();
    const ctrl = tasks.mount(els, { now: () => NOW });
    ctrl.update(list());
    await flush();
    const btns = buttonsNamed(rowFor(els, 'balance-check'), 'Run now');
    assert.strictEqual(btns.length, 1);
    btns[0].click();
    assert.strictEqual(sent('POST', '/tasks/balance-check/run').length, 1);
    assert.ok(buttonNamed(rowFor(els, 'balance-check'), 'Starting'), 'the button says Starting while the request is out');
    assert.ok(buttonNamed(rowFor(els, 'balance-check'), 'Starting').disabled);
    assert.ok(buttonsNamed(rowFor(els, 'login-check'), 'Run now')[0].disabled === false, 'other tasks stay available');
    release();
    await flush();
    assert.ok(buttonsNamed(rowFor(els, 'balance-check'), 'Run now').length === 1 && !buttonsNamed(rowFor(els, 'balance-check'), 'Run now')[0].disabled);
    assert.ok(!els.notice.hidden && els.notice.textContent.includes('Started balance-check') && els.notice.textContent.includes('abcdef12'), els.notice.textContent);
    assert.ok(sent('GET', '/tasks.json').length >= 1, 'the list is reloaded');
    // a running task cannot be started again
    ctrl.update([task('balance-check', { running: true })]);
    const b = findAll(rowFor(els, 'balance-check'), (n) => n.tag === 'button' && n.textContent === 'Run now')[0];
    assert.ok(b.disabled);
    // a refusal is said in words
    ctrl.update(list());
    global.fetch = (url, o) => {
      if (o && o.method === 'POST') { return Promise.resolve({ ok: false, status: 409, text: () => Promise.resolve('{"error":"task is already running"}') }); }
      return realFetch(url, o);
    };
    buttonsNamed(rowFor(els, 'portal-up'), 'Run now')[0].click();
    await flush();
    assert.ok(!els.error.hidden && els.error.textContent.includes('Run now failed for portal-up: task is already running'), els.error.textContent);
    global.fetch = realFetch;
  }

  // stale data cue after a failed poll, and the empty and loading states
  {
    fetchReply = () => json(list());
    const els = tasksEls();
    const ctrl = tasks.mount(els, { now: () => NOW });
    assert.ok(els.empty.hidden && els.cols.hidden, 'nothing is shown before the first load');
    ctrl.update(list());
    fetchReply = () => json({ error: 'degraded' }, 503);
    await ctrl.reload();
    assert.ok(!els.stale.hidden && /degraded/.test(els.stale.textContent) && /last data/.test(els.stale.textContent), els.stale.textContent);
    assert.ok(/may be out of date/.test(els.sub.textContent));
    assert.strictEqual(rowsOf(els).length, 5, 'the old rows stay');
    fetchReply = () => json(list());
    await ctrl.reload();
    assert.ok(els.stale.hidden);
    const e2 = tasksEls();
    const c2 = tasks.mount(e2, { now: () => NOW });
    c2.update([]);
    assert.ok(!e2.empty.hidden && e2.cols.hidden);
    assert.ok(/task configuration/.test(e2.emptytext.textContent));
    const e3 = tasksEls();
    const c3 = tasks.mount(e3, { now: () => NOW });
    fetchReply = () => new Error('offline');
    await c3.reload();
    assert.ok(/Could not load/.test(e3.sub.textContent));
  }

  // focus survives a refresh
  {
    fetchReply = (url) => (url.includes('/runs') ? json(runsFor()) : json(list()));
    const els = tasksEls();
    const ctrl = tasks.mount(els, { now: () => NOW });
    ctrl.update(list());
    const b = buttonsNamed(rowFor(els, 'portal-up'), 'Run now')[0];
    b.focus();
    ctrl.update(list().map((t) => (t.id === 'portal-up' ? Object.assign(t, { last_success: iso(100) }) : t)));
    assert.strictEqual(global.document.activeElement.getAttribute('data-fk'), b.getAttribute('data-fk'));
    assert.ok(global.document.activeElement.isConnected);
  }

  // ----- sessions
  const node = (id, extra) => Object.assign({ id, name: id, available: true, session: null }, extra);
  const running = (id, secLeft, extra) => node(id, Object.assign({ session: { id: 's1', state: 'running', expires_at: ahead(secLeft), started_at: iso(60) },
    url: 'http://' + id + ':6080/vnc.html?autoconnect=1&path=websockify%3Ftoken%3DSECRETTOKEN' }, extra));
  const nodes = () => [node('exit-a', { available: false }), node('runner-2'), running('runner-1', 2520)];
  const sessEls = () => {
    const els = { sub: mk('p'), error: mk('div'), notice: mk('div'), stale: mk('div'), empty: mk('div'), emptytext: mk('p'), tablewrap: mk('div'), rows: mk('tbody') };
    els.error.hidden = els.notice.hidden = els.stale.hidden = els.empty.hidden = els.tablewrap.hidden = true;
    return els;
  };

  // sessionTimeLeft
  assert.strictEqual(sessions.timeLeft(ahead(2520), NOW), '42 min');
  assert.strictEqual(sessions.timeLeft(ahead(3900), NOW), '1 h 5 min');
  assert.strictEqual(sessions.timeLeft(ahead(45), NOW), '45 s');
  assert.strictEqual(sessions.timeLeft(iso(5), NOW), 'Ending now');
  assert.strictEqual(sessions.timeLeft('', NOW), '');
  assert.strictEqual(sessions.timeLeft('garbage', NOW), '');

  // openLinkIsSafeUrl: only a plain http link becomes an href; the token goes nowhere else
  assert.strictEqual(sessions.safeUrl('http://runner-1:6080/vnc.html'), 'http://runner-1:6080/vnc.html');
  assert.strictEqual(sessions.safeUrl('javascript:alert(1)'), '');
  assert.strictEqual(sessions.safeUrl('https://x.example.net/'), '');
  assert.strictEqual(sessions.safeUrl(undefined), '');

  {
    replaced.length = 0;
    const els = sessEls();
    const ctrl = sessions.mount(els, { now: () => NOW });
    ctrl.update({ nodes: nodes() });
    const r1 = els.rows.children.find((tr) => tr.children[0].textContent === 'runner-1');
    const open = findAll(r1, (n) => n.tag === 'a')[0];
    assert.ok(open && open.textContent === 'Open');
    assert.ok(open.getAttribute('href').startsWith('http://runner-1:6080/vnc.html'));
    assert.strictEqual(open.getAttribute('rel'), 'noopener noreferrer');
    assert.strictEqual(open.getAttribute('target'), '_blank');
    // the token is in that href and nowhere else
    const leaks = walk(els.rows).concat([els.sub, els.error, els.notice, els.stale]).filter((n) =>
      JSON.stringify([n._text, Object.keys(n.attrs).filter((k) => k !== 'href').map((k) => n.attrs[k])]).includes('SECRETTOKEN'));
    assert.strictEqual(leaks.length, 0, 'the token appears outside the Open href');
    assert.strictEqual(replaced.length, 0, 'the page URL is untouched');
    // a hostile url is not linked
    ctrl.update({ nodes: [running('runner-1', 600, { url: 'javascript:alert(1)' })] });
    assert.strictEqual(findAll(els.rows, (n) => n.tag === 'a').length, 0);
    assert.ok(/Running/.test(els.rows.textContent));
    ctrl.update({ nodes: nodes() });

    // states, time left and the sort (running first, then idle, then unavailable)
    assert.deepStrictEqual(els.rows.children.map((tr) => tr.children[0].textContent), ['runner-1', 'runner-2', 'exit-a']);
    assert.strictEqual(els.rows.children[0].children[1].textContent, 'Running');
    assert.strictEqual(els.rows.children[0].children[2].textContent, '42 min');
    assert.strictEqual(els.rows.children[1].children[1].textContent, 'No session');
    assert.strictEqual(els.rows.children[1].children[2].textContent, 'n/a');
    assert.strictEqual(els.rows.children[2].children[1].textContent, 'Unavailable');
    assert.strictEqual(els.sub.textContent, '1 running. Sessions end by themselves when their time runs out.');
    // the time left moves with the clock without rebuilding the row
    const rowBefore = els.rows.children[0];
    ctrl.tick(NOW + 60000);
    assert.strictEqual(els.rows.children[0], rowBefore);
    assert.strictEqual(els.rows.children[0].children[2].textContent, '41 min');
  }

  // sessionUnavailableHasNoStart
  {
    const els = sessEls();
    const ctrl = sessions.mount(els, { now: () => NOW });
    ctrl.update({ nodes: nodes() });
    const ex = els.rows.children.find((tr) => tr.children[0].textContent === 'exit-a');
    assert.strictEqual(findAll(ex, (n) => n.tag === 'button' || n.tag === 'select').length, 0);
    assert.ok(ex.textContent.includes('No runner on this node'));
    const r2 = els.rows.children.find((tr) => tr.children[0].textContent === 'runner-2');
    assert.ok(buttonNamed(r2, 'Start') && findAll(r2, (n) => n.tag === 'select').length === 1);
    assert.strictEqual(findAll(r2, (n) => n.tag === 'select')[0].getAttribute('aria-label'), 'Session length for runner-2');
    const r1 = els.rows.children.find((tr) => tr.children[0].textContent === 'runner-1');
    assert.ok(!buttonNamed(r1, 'Start') && buttonNamed(r1, 'Stop'));
    // start sends the chosen length, and the choice survives a refresh
    const sel = findAll(r2, (n) => n.tag === 'select')[0];
    sel.value = '14400';
    fire(sel, 'change');
    ctrl.update({ nodes: nodes() });
    fetches.length = 0;
    fetchReply = (url, o) => (o && o.method === 'POST' ? json({ id: 's2' }, 201) : json({ nodes: nodes() }));
    buttonNamed(els.rows.children.find((tr) => tr.children[0].textContent === 'runner-2'), 'Start').click();
    await flush();
    assert.strictEqual(sent('POST', '/sessions/runner-2/start').length, 1);
    assert.strictEqual(JSON.parse(sent('POST', '/sessions/runner-2/start')[0].body).ttl_seconds, 14400);
    assert.ok(els.notice.textContent.includes('Started a session on runner-2'));
  }

  // Stop asks first, naming the node and what happens
  {
    const els = sessEls();
    const ctrl = sessions.mount(els, { now: () => NOW });
    ctrl.update({ nodes: nodes() });
    fetches.length = 0;
    fetchReply = (url, o) => (o && o.method === 'POST' ? empty(204) : json({ nodes: nodes() }));
    buttonNamed(els.rows.children[0], 'Stop').click();
    assert.strictEqual(sent('POST', '/stop').length, 0, 'nothing is sent before the confirmation');
    const panel = findAll(els.rows, (n) => hasClass(n, 'confirm'))[0];
    assert.ok(panel, 'a confirmation opens');
    assert.ok(panel.textContent.includes('runner-1') && /ends|end/.test(panel.textContent));
    buttonNamed(panel, 'Keep it running').click();
    assert.strictEqual(findAll(els.rows, (n) => hasClass(n, 'confirm')).length, 0);
    assert.strictEqual(sent('POST', '/stop').length, 0);
    buttonNamed(els.rows.children[0], 'Stop').click();
    buttonNamed(findAll(els.rows, (n) => hasClass(n, 'confirm'))[0], 'Stop session on runner-1').click();
    await flush();
    assert.strictEqual(sent('POST', '/sessions/runner-1/stop').length, 1);
    assert.ok(els.notice.textContent.includes('Stopped the session on runner-1'));
    // a failure is spelled out
    fetchReply = (url, o) => (o && o.method === 'POST' ? json({ error: 'node unreachable' }, 503) : json({ nodes: nodes() }));
    buttonNamed(els.rows.children[0], 'Stop').click();
    buttonNamed(findAll(els.rows, (n) => hasClass(n, 'confirm'))[0], 'Stop session on runner-1').click();
    await flush();
    assert.ok(!els.error.hidden && els.error.textContent.includes('Stop failed on runner-1: node unreachable'));
  }

  // emptyStates + stale cue for sessions
  {
    const els = sessEls();
    const ctrl = sessions.mount(els, { now: () => NOW });
    assert.ok(els.empty.hidden && els.tablewrap.hidden);
    ctrl.update({ nodes: [] });
    assert.ok(!els.empty.hidden && els.tablewrap.hidden && /sessions tag/.test(els.emptytext.textContent));
    ctrl.update({ nodes: nodes() });
    assert.ok(els.empty.hidden && !els.tablewrap.hidden);
    fetchReply = () => json({ error: 'node list unavailable' }, 503);
    await ctrl.reload();
    assert.ok(!els.stale.hidden && /last data/.test(els.stale.textContent));
    assert.ok(/may be out of date/.test(els.sub.textContent));
    assert.strictEqual(els.rows.children.length, 3);
  }
  console.log('ok');
})().catch((e) => { console.error(e); process.exit(1); });
