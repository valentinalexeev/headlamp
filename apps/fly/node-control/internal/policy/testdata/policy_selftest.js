// Run with: node policy_selftest.js <path to hl.js> <path to shell.js> <path to policy.js>. Exits non-zero on a failed check.
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
  getElementById: () => null, querySelector: () => null, querySelectorAll: () => [], addEventListener() {}, hidden: false, activeElement: null,
  documentElement: new FakeNode('html'),
};
global.window = {
  location: { search: '', pathname: '/policy', href: 'http://localhost/policy', reload() {} },
  history: { replaceState() {} },
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
const policy = hl.policy;
assert.ok(policy && hl.shell, 'hl.policy and hl.shell are defined');

const NOW = Date.parse('2026-01-01T12:00:00Z');
const APPLIED = '2026-01-01T11:55:00Z';
const appliedSec = Date.parse(APPLIED) / 1000;
const iso = (secAgo) => new Date(NOW - secAgo * 1000).toISOString();
const DIFF = '--- current\n+++ proposed\n@@ -1,2 +1,3 @@\n   "acls": [],\n-  "old": 1,\n+  "tag:ns-demo": ["x"],\n';
const pend = (extra) => Object.assign({ id: 48, actor: 'alice', reason: 'Funnel: add namespace demo', deadline: iso(-250), seconds_left: 250, attempts: 0, overdue: false, applied_at: APPLIED, diff: DIFF }, extra);
const hist = (extra) => [
  { id: 48, at: APPLIED, actor: 'alice', reason: 'Funnel: add namespace demo', state: 'pending', deadline: iso(-250) },
  { id: 47, at: iso(86400), actor: 'alice', reason: 'edit', state: 'confirmed', deadline: iso(86000) },
  { id: 46, at: iso(90000), actor: 'bob', reason: 'edit', state: 'reverted', deadline: iso(89000) },
].concat(extra || []);
const stateOf = (extra) => Object.assign({ policy: '{"acls":[]}', updated_at: '2026-01-01T11:55:00Z', pending: pend(), last_edit: null, history: hist() }, extra);
const quiet = (extra) => stateOf(Object.assign({ pending: null, history: hist().slice(1) }, extra));

function pageEls() {
  const mk = (tag) => { const n = new FakeNode(tag); n.isRoot = true; return n; };
  const els = { sub: mk('p'), error: mk('div'), notice: mk('div'), pending: mk('section'), pendtitle: mk('div'), pendtext: mk('p'), pendbtns: mk('div'),
    diffbox: mk('section'), difftitle: mk('h2'), diffmeta: mk('span'), diff: mk('pre'), lockrow: mk('div'), editlock: mk('button'), lockhint: mk('span'),
    editbox: mk('section'), editor: mk('textarea'), editbtns: mk('div'), message: mk('p'), prevdiff: mk('pre'), warnings: mk('ul'), rows: mk('tbody') };
  [els.error, els.notice, els.pending, els.diffbox, els.lockrow, els.editbox, els.prevdiff].forEach((n) => { n.hidden = true; });
  return els;
}
let loadedAt = appliedSec + 30;
function mountPage(deps) {
  const els = pageEls();
  const ctrl = policy.mount(els, Object.assign({ now: () => NOW, loaded: () => loadedAt, reload: () => { els.reloaded = true; } }, deps));
  return { els, ctrl };
}
const names = (box) => box.children.filter((n) => n.tag === 'button').map((b) => b.textContent);
const historyButtons = (els) => findAll(els.rows, (n) => n.tag === 'button');

(async () => {
  // confirmOnlyOnFreshPage: strictly later than the apply second offers Confirm, which posts the change id
  {
    loadedAt = appliedSec + 1;
    const { els, ctrl } = mountPage();
    ctrl.update(stateOf());
    assert.deepStrictEqual(names(els.pendbtns), ['Confirm change', 'Revert now']);
    assert.ok(els.pendtext.textContent.includes('did not lock you out'));
    fetches.length = 0;
    fetchReply = (url) => (url === '/policy/confirm' ? empty() : json(quiet()));
    buttonNamed(els.pendbtns, 'Confirm change').click();
    await flush();
    assert.strictEqual(sent('POST', '/policy/confirm').length, 1);
    assert.deepStrictEqual(JSON.parse(sent('POST', '/policy/confirm')[0].body), { change_id: 48 });
    assert.ok(sent('GET', '/policy/state').length >= 1, 'state is read again');
    assert.strictEqual(els.pending.hidden, true, 'the pending panel is gone');
    assert.ok(els.notice.textContent.includes('#48 confirmed'));
  }

  // reloadPromptOnOldPage: the same second, an earlier one, or an unknown time never offer Confirm
  for (const [name, loaded] of [['earlier', appliedSec - 5], ['same second', appliedSec], ['unknown', NaN]]) {
    loadedAt = loaded;
    const { els, ctrl } = mountPage();
    ctrl.update(stateOf());
    assert.deepStrictEqual(names(els.pendbtns), ['Reload to confirm', 'Revert now'], name);
    assert.ok(els.pendtext.textContent.includes('loaded before the change'), name);
    buttonNamed(els.pendbtns, 'Reload to confirm').click();
    assert.strictEqual(els.reloaded, true, name);
    fetches.length = 0;
    await ctrl.confirmChange();
    assert.strictEqual(sent('POST', '/policy/confirm').length, 0, name + ': no confirm request from an old page');
  }
  {
    loadedAt = appliedSec + 30;
    const { els, ctrl } = mountPage();
    ctrl.update(stateOf({ pending: pend({ applied_at: '' }) }));
    assert.deepStrictEqual(names(els.pendbtns), ['Reload to confirm', 'Revert now'], 'no applied_at');
  }

  // revertNowAlwaysAvailable: on an old page, a fresh page and a stuck change
  for (const [name, loaded, extra] of [['old', appliedSec - 5, {}], ['fresh', appliedSec + 30, {}], ['stuck', appliedSec - 5, { attempts: 3, overdue: true }]]) {
    loadedAt = loaded;
    const { els, ctrl } = mountPage();
    ctrl.update(stateOf({ pending: pend(extra) }));
    const b = buttonNamed(els.pendbtns, 'Revert now');
    assert.ok(b && !b.disabled, name);
    fetches.length = 0;
    fetchReply = (url) => (url === '/policy/revert-now' ? empty() : json(quiet()));
    b.click();
    await flush();
    assert.deepStrictEqual(JSON.parse(sent('POST', '/policy/revert-now')[0].body), { change_id: 48 }, name);
    assert.ok(els.notice.textContent.includes('#48 reverted'), name);
  }

  // stuckWarningOffersConfirm: the failing revert is said in words and Confirm stays on a fresh page
  {
    loadedAt = appliedSec + 30;
    const { els, ctrl } = mountPage();
    ctrl.update(stateOf({ pending: pend({ attempts: 3, overdue: true, seconds_left: 0 }) }));
    assert.ok(els.pendtext.textContent.includes('automatic revert is failing (3 attempts)'), els.pendtext.textContent);
    assert.ok(els.pendtext.textContent.includes('Confirm keeps the change'));
    assert.ok(els.pendtitle.textContent.includes('neither confirmed nor reverted'));
    assert.ok(hasClass(els.pending, 'crit'));
    assert.ok(findAll(els.pendtitle, (n) => hasClass(n, 'g') && hasClass(n, 'crit')).length === 1, 'a shape, not colour alone');
    assert.deepStrictEqual(names(els.pendbtns), ['Confirm change', 'Revert now']);
  }

  // editDisabledWhilePending: no editor, a disabled Edit policy button and the reason
  {
    loadedAt = appliedSec + 30;
    const { els, ctrl } = mountPage();
    ctrl.update(quiet());
    assert.strictEqual(els.editbox.hidden, false);
    assert.strictEqual(els.lockrow.hidden, true);
    assert.deepStrictEqual(names(els.editbtns), ['Preview', 'Apply', 'Reload']);
    assert.ok(findAll(els.editbtns, (n) => n.tag === 'button').slice(0, 2).every((b) => !b.disabled));
    ctrl.update(stateOf());
    assert.strictEqual(els.editbox.hidden, true);
    assert.strictEqual(els.editor.disabled, true);
    assert.strictEqual(els.lockrow.hidden, false);
    assert.strictEqual(els.editlock.disabled, true);
    assert.strictEqual(els.editlock.textContent, 'Edit policy');
    assert.ok(els.lockhint.textContent.includes('Available after #48 is confirmed or reverted.'));
    assert.ok(findAll(els.editbtns, (n) => n.tag === 'button').slice(0, 2).every((b) => b.disabled), 'Preview and Apply are off');
    fetches.length = 0;
    await ctrl.apply();
    assert.strictEqual(fetches.length, 0, 'no apply request while pending');
    assert.strictEqual(els.diffbox.hidden, false);
    assert.strictEqual(els.difftitle.textContent, 'What changed in #48');
    assert.ok(els.diffmeta.textContent.includes('by alice, Funnel: add namespace demo'));
  }

  // countdownReachesZeroThenPolls
  {
    loadedAt = appliedSec + 30;
    let t = NOW;
    const { els, ctrl } = mountPage({ now: () => t });
    ctrl.update(stateOf({ pending: pend({ seconds_left: 3 }) }));
    const count = () => { const n = findAll(els.pendtitle, (n) => hasClass(n, 'tick'))[0]; return n ? n.textContent : '00:00'; };
    assert.strictEqual(count(), '00:03');
    fetches.length = 0;
    t += 2000;
    ctrl.tick();
    assert.strictEqual(count(), '00:01');
    assert.strictEqual(sent('GET', '/policy/state').length, 0, 'not polling before zero');
    t += 1500;
    fetchReply = () => json(stateOf({ pending: pend({ seconds_left: 3 }) }));
    ctrl.tick();
    assert.strictEqual(count(), '00:00');
    assert.ok(els.pendtitle.textContent.includes('is being reverted'));
    assert.strictEqual(findAll(els.pendtitle, (n) => hasClass(n, 'tick')).length, 0, 'no timer once the deadline has passed');
    assert.strictEqual(sent('GET', '/policy/state').length, 1, 'polls at zero');
    await flush();
    t += 1000;
    fetchReply = () => json(quiet());
    ctrl.tick();
    assert.strictEqual(sent('GET', '/policy/state').length, 1, 'does not poll more often than every few seconds');
    t += 3000;
    ctrl.tick();
    await flush();
    assert.strictEqual(sent('GET', '/policy/state').length, 2);
    assert.strictEqual(els.pending.hidden, true, 'the panel goes when the change is gone');
  }

  // diffRendersAsText: policy text is never parsed as markup
  {
    const { els, ctrl } = mountPage();
    const evil = '--- current\n+++ proposed\n@@ -1 +1 @@\n-<img src=x onerror=1>\n+<b>"x"</b>\n';
    ctrl.update(stateOf({ pending: pend({ diff: evil }) }));
    assert.ok(els.diff.textContent.includes('-<img src=x onerror=1>\n') || els.diff.textContent.includes('-<img src=x onerror=1>'));
    const lines = els.diff.children;
    assert.ok(lines.every((n) => n.tag === 'span'), 'only spans');
    assert.ok(lines.some((n) => hasClass(n, 'add') && n.textContent === '+<b>"x"</b>'));
    assert.ok(lines.some((n) => hasClass(n, 'rem') && n.textContent === '-<img src=x onerror=1>'));
    assert.ok(findAll(els.diff, (n) => n.tag === 'b' || n.tag === 'img').length === 0);
    // the preview result is drawn the same way
    ctrl.update(quiet());
    fetchReply = () => json({ valid: true, error: '', diff: evil, warnings: ['<i>careful</i>'] });
    await ctrl.preview();
    assert.strictEqual(els.message.textContent, 'Valid.');
    assert.strictEqual(els.prevdiff.hidden, false);
    assert.ok(findAll(els.prevdiff, (n) => n.tag === 'b').length === 0);
    assert.strictEqual(els.warnings.children[0].textContent, '<i>careful</i>');
    fetchReply = () => json({ valid: false, error: 'bad rule', diff: '', warnings: [] });
    await ctrl.preview();
    assert.strictEqual(els.message.textContent, 'Invalid: bad rule');
    assert.ok(els.prevdiff.textContent.includes('(no differences)'));
  }

  // historyRevertDisabledWhilePending
  {
    const { els, ctrl } = mountPage();
    ctrl.update(stateOf());
    let bs = historyButtons(els);
    assert.strictEqual(bs.length, 3);
    assert.ok(bs.every((b) => b.disabled && b.getAttribute('title') === 'Confirm or revert the pending change first'));
    ctrl.update(quiet());
    bs = historyButtons(els);
    assert.strictEqual(bs.length, 2);
    assert.ok(bs.every((b) => !b.disabled));
    fetches.length = 0;
    fetchReply = (url) => (url.startsWith('/policy/revert/') ? json({ change_id: 49, deadline: iso(-120) }) : json(stateOf({ pending: pend({ id: 49 }) })));
    bs[0].click();
    await flush();
    assert.strictEqual(sent('POST', '/policy/revert/47').length, 1);
    assert.ok(els.notice.textContent.includes('Started change #49 to undo #47'));
    assert.strictEqual(els.pending.hidden, false);
    // history rows say the state with a shape and a word
    const states = els.rows.children.map((tr) => findAll(tr.children[2], (n) => hasClass(n, 'st'))[0].textContent);
    assert.deepStrictEqual(states, ['Pending', 'Confirmed', 'Reverted']);
    // three columns; who and why on a second line; the button is in the row
    assert.ok(els.rows.children.every((tr) => tr.children.length === 3));
    assert.ok(els.rows.children[1].children[0].textContent.includes('alice, edit'));
    assert.strictEqual(findAll(els.rows.children[1], (n) => n.tag === 'button').length, 1);
  }

  // unparseableAppliedAtNeverOffersConfirm
  {
    loadedAt = appliedSec + 30;
    const { els, ctrl } = mountPage();
    ctrl.update(stateOf({ pending: pend({ applied_at: 'soon' }) }));
    assert.deepStrictEqual(names(els.pendbtns), ['Reload to confirm', 'Revert now']);
  }

  // pastDeadlineWithoutFailedAttemptIsNotStuck: the watcher has not ticked yet
  {
    loadedAt = appliedSec + 30;
    const { els, ctrl } = mountPage();
    ctrl.update(stateOf({ pending: pend({ overdue: true, attempts: 0, seconds_left: 0 }) }));
    assert.ok(els.pendtitle.textContent.includes('is being reverted'));
    assert.ok(!els.pendtext.textContent.includes('failing'));
    assert.ok(!hasClass(els.pending, 'crit'));
    assert.strictEqual(findAll(els.pendtitle, (n) => hasClass(n, 'tick')).length, 0);
    ctrl.update(stateOf({ pending: pend({ overdue: true, attempts: 1, seconds_left: 0 }) }));
    assert.ok(els.pendtext.textContent.includes('failing (1 attempt)'));
    assert.strictEqual(findAll(els.pendtitle, (n) => hasClass(n, 'tick')).length, 0, 'no timer when stuck');
  }

  // leadInNamesTheKeptVersion
  {
    const { els, ctrl } = mountPage();
    ctrl.update(stateOf());
    assert.strictEqual(els.sub.textContent, 'Version 47 confirmed. Change #48 is live but not yet kept.');
  }

  // pollKeepsPanelAndDiffNodes: a click or selection survives a poll of the same state
  {
    loadedAt = appliedSec + 30;
    const { els, ctrl } = mountPage();
    ctrl.update(stateOf());
    const btns = els.pendbtns.children.slice();
    const lines = els.diff.children.slice();
    const confirmBtn = buttonNamed(els.pendbtns, 'Confirm change');
    confirmBtn.focus();
    ctrl.update(stateOf());
    assert.ok(els.pendbtns.children.every((n, i) => n === btns[i]), 'buttons were rebuilt');
    assert.ok(els.diff.children.every((n, i) => n === lines[i]), 'diff was redrawn');
    assert.strictEqual(global.document.activeElement, confirmBtn);
    ctrl.update(stateOf({ pending: pend({ attempts: 2 }) }));
    assert.ok(els.pendtext.textContent.includes('failing'), 'a changed state does rebuild');
    ctrl.update(stateOf({ pending: pend({ attempts: 2, diff: '+x\n' }) }));
    assert.notStrictEqual(els.diff.children[0], lines[0]);
  }

  // pollKeepsHistoryRows: the same history is not rebuilt by a poll; a changed one is
  {
    loadedAt = appliedSec + 30;
    const { els, ctrl } = mountPage();
    ctrl.update(stateOf());
    const rows = els.rows.children.slice();
    assert.ok(rows.length >= 3);
    historyButtons(els)[0].focus();
    ctrl.update(stateOf());
    assert.ok(els.rows.children.every((n, i) => n === rows[i]), 'history rows were rebuilt by a poll');
    ctrl.update(stateOf({ history: hist().concat([{ id: 45, at: iso(95000), actor: 'bob', reason: 'edit', state: 'reverted', deadline: iso(94000) }]) }));
    assert.strictEqual(els.rows.children.length, rows.length + 1, 'a new entry does rebuild');
    ctrl.update(quiet());
    assert.notStrictEqual(els.rows.children[0], rows[0], 'the Revert buttons change with the pending state');
    assert.ok(historyButtons(els).every((b) => !b.disabled));
  }

  // pollDuringAnActionIsIgnored: a read begun while an action is out is neither sent nor drawn
  {
    loadedAt = appliedSec + 30;
    const { els, ctrl } = mountPage();
    ctrl.update(stateOf());
    const realFetch = global.fetch;
    let release;
    const gate = new Promise((r) => { release = r; });
    fetches.length = 0;
    global.fetch = (url, opts) => {
      fetches.push({ url, method: (opts && opts.method) || 'GET' });
      if (url === '/policy/confirm') { return gate.then(() => ({ ok: true, status: 204, text: () => Promise.resolve('') })); }
      return Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve(JSON.stringify(quiet())) });
    };
    buttonNamed(els.pendbtns, 'Confirm change').click();
    await flush();
    const before = els.pendbtns.children.slice();
    await ctrl.reload();
    await flush();
    assert.strictEqual(sent('GET', '/policy/state').length, 0, 'no read while the action is out');
    assert.strictEqual(els.pending.hidden, false);
    assert.ok(els.pendbtns.children.every((n, i) => n === before[i]), 'the panel was redrawn');
    release();
    await flush();
    assert.strictEqual(sent('GET', '/policy/state').length, 1, 'the action reads again itself');
    assert.strictEqual(els.pending.hidden, true);
    global.fetch = realFetch;
  }

  // slowPollCannotRestoreAConfirmedChange
  {
    loadedAt = appliedSec + 30;
    const { els, ctrl } = mountPage();
    ctrl.update(stateOf());
    let release;
    const gate = new Promise((r) => { release = r; });
    fetchReply = (url) => (url === '/policy/state' ? { gate: true } : empty());
    const realFetch = global.fetch;
    let first = true;
    global.fetch = (url, opts) => {
      if (url === '/policy/state' && first) {
        first = false;
        return gate.then(() => ({ ok: true, status: 200, text: () => Promise.resolve(JSON.stringify(stateOf())) }));
      }
      fetchReply = (u) => (u === '/policy/confirm' ? empty() : json(quiet()));
      return realFetch(url, opts);
    };
    const slow = ctrl.reload();
    buttonNamed(els.pendbtns, 'Confirm change').click();
    await flush();
    assert.strictEqual(els.pending.hidden, true, 'confirmed');
    release();
    await slow;
    await flush();
    assert.strictEqual(els.pending.hidden, true, 'the late answer was dropped');
    global.fetch = realFetch;
  }

  // refusalSurvivesALaterGoodPoll but a load error does not
  {
    loadedAt = appliedSec + 30;
    const { els, ctrl } = mountPage();
    ctrl.update(stateOf());
    fetchReply = () => json({ detail: 'down' }, 500);
    await ctrl.reload();
    assert.ok(els.error.textContent.includes('Cannot load the policy'));
    fetchReply = () => json(stateOf());
    await ctrl.reload();
    assert.strictEqual(els.error.hidden, true, 'a load error goes when a read works');
    fetchReply = (url) => (url === '/policy/confirm' ? json({ detail: 'that change is not the pending one' }, 409) : json(stateOf()));
    buttonNamed(els.pendbtns, 'Confirm change').click();
    await flush();
    assert.ok(els.error.textContent.includes('not the pending one'));
    await ctrl.reload();
    assert.ok(els.error.textContent.includes('not the pending one'), 'a good poll keeps the refusal');
    assert.strictEqual(els.error.hidden, false);
  }

  // emptyHistory
  {
    const { els, ctrl } = mountPage();
    ctrl.update(quiet({ history: [] }));
    assert.strictEqual(els.rows.children.length, 1);
    assert.strictEqual(els.rows.children[0].textContent, 'No console changes yet.');
    assert.strictEqual(els.sub.textContent, 'No console changes yet.');
    assert.strictEqual(historyButtons(els).length, 0);
  }

  // refusalShowsItsMessageAndReloads: a server refusal of Confirm
  {
    loadedAt = appliedSec + 30;
    const { els, ctrl } = mountPage();
    ctrl.update(stateOf());
    fetches.length = 0;
    fetchReply = (url) => (url === '/policy/confirm' ? json({ detail: 'that change is not the pending one' }, 409) : json(quiet()));
    buttonNamed(els.pendbtns, 'Confirm change').click();
    await flush();
    assert.ok(els.error.textContent.includes('that change is not the pending one'), els.error.textContent);
    assert.strictEqual(els.error.hidden, false);
    assert.ok(sent('GET', '/policy/state').length >= 1, 'state is read again after a refusal');
    assert.strictEqual(els.pending.hidden, true);
  }

  // applyKeepsEditOnConflictAndPollNeverEatsAnEdit
  {
    const { els, ctrl } = mountPage();
    ctrl.update(quiet());
    els.editor.value = '{"acls":[1]}';
    ctrl.update(quiet({ policy: '{"acls":[2]}', updated_at: '2026-01-01T11:59:00Z' }));
    assert.strictEqual(els.editor.value, '{"acls":[1]}', 'a poll keeps the edit');
    assert.ok(els.message.textContent.includes('changed after you started editing'));
    fetches.length = 0;
    fetchReply = () => json({ detail: 'the policy changed since you loaded it' }, 409);
    await ctrl.apply();
    assert.deepStrictEqual(JSON.parse(sent('POST', '/policy/apply')[0].body), { policy: '{"acls":[1]}', base_updated_at: '2026-01-01T11:55:00Z' });
    assert.strictEqual(els.editor.value, '{"acls":[1]}', 'the edit stays after a conflict');
    assert.ok(els.error.textContent.includes('changed since you loaded it'));
    assert.strictEqual(sent('GET', '/policy/state').length, 0, 'no automatic reload that would drop the edit');
    // Reload discards the edit on purpose
    fetchReply = () => json(quiet({ policy: '{"acls":[2]}', updated_at: '2026-01-01T11:59:00Z' }));
    buttonNamed(els.editbtns, 'Reload').click();
    await flush();
    assert.strictEqual(els.editor.value, '{"acls":[2]}');
    // a good apply loads the new state
    els.editor.value = '{"acls":[3]}';
    fetchReply = (url) => (url === '/policy/apply' ? json({ change_id: 50, deadline: iso(-120) }) : json(stateOf({ pending: pend({ id: 50 }) })));
    await ctrl.apply();
    assert.ok(els.notice.textContent.includes('Applied as change #50'));
    assert.strictEqual(els.pending.hidden, false);
  }

  // headscaleDownShowsAWarningAndKeepsTheLastState
  {
    const { els, ctrl } = mountPage();
    ctrl.update(quiet());
    fetchReply = () => json({ detail: 'headscale is unreachable or failed' }, 502);
    await ctrl.reload();
    assert.ok(els.error.textContent.includes('Headscale is unreachable'));
    assert.ok(els.sub.textContent.includes('may be out of date'));
    fetchReply = () => json(quiet());
    await ctrl.reload();
    assert.strictEqual(els.error.hidden, true);
  }
  console.log('policy self-test ok');
})().catch((e) => { console.error(e); process.exit(1); });
