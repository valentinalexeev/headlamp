// Run with: node palette_selftest.js <path to hl.js> <path to palette.js>. Exits non-zero on a failed check.
const assert = require('assert');
const path = require('path');

// A small DOM stand-in: elements with children, attributes and listeners, and a dialog that can open and close.
function node(tag) {
  const n = {
    tag, children: [], className: '', textContent: '', value: '', attrs: {}, listeners: {}, hidden: false, open: false, focused: false,
    appendChild(c) { this.children.push(c); return c; },
    removeChild(c) { this.children.splice(this.children.indexOf(c), 1); },
    get firstChild() { return this.children[0] || null; },
    setAttribute(k, v) { this.attrs[k] = String(v); },
    getAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null; },
    removeAttribute(k) { delete this.attrs[k]; },
    addEventListener(t, f) { (this.listeners[t] = this.listeners[t] || []).push(f); },
    fire(t, ev) { (this.listeners[t] || []).forEach((f) => f(Object.assign({ target: this, preventDefault() { this.prevented = true; } }, ev))); },
    focus() { this.focused = true; },
  };
  if (tag === 'dialog') {
    n.showModal = function () { this.open = true; };
    n.close = function () { this.open = false; };
  }
  return n;
}
function find(n, pred, out = []) { if (pred(n)) out.push(n); n.children.forEach((c) => find(c, pred, out)); return out; }
function text(n) { return n.textContent + n.children.map(text).join(' '); }

const body = node('body');
global.document = { createElement: node, createElementNS: (ns, tag) => node(tag), body, hidden: false, addEventListener() {}, querySelector: () => null };
global.window = { localStorage: { getItem: () => null, setItem() {}, removeItem() {} } };
require(path.resolve(process.argv[2]));
require(path.resolve(process.argv[3]));
const pal = global.window.hl.palette;
assert.ok(pal, 'hl.palette is defined');

const NAV = [
  { label: 'Overview', path: '/', icon: 'overview' },
  { label: 'Nodes', path: '/nodes', icon: 'nodes' },
  { label: 'Devices', path: '/enroll', icon: 'devices' },
  { label: 'Activity', path: '/activity', icon: 'activity' },
];
const ACTIONS = [{ label: 'Enrol device', path: '/enroll?enrol=1', icon: 'plus' }];
const SEARCH = { items: [
  { label: 'exit-a', kind: 'Node', path: '/nodes?sel=exit-a' },
  { label: 'phone-1', kind: 'Device', path: '/enroll' },
  { label: 'wiki.demo', kind: 'Service', path: '/funnel' },
] };

function fixture(over = {}) {
  const calls = [];
  const nav = [];
  const api = async (method, url) => {
    calls.push(method + ' ' + url);
    if (url === '/_nav.json') { if (over.navFails) throw new Error('down'); return over.nav || NAV; }
    if (url === '/_search.json') { if (over.searchFails) throw new Error('down'); return over.search || SEARCH; }
    throw new Error('unexpected ' + url);
  };
  const host = node('div');
  const ctl = pal.create({ host, api, navigate: (p) => nav.push(p), actions: over.actions || ACTIONS });
  return { ctl, calls, nav, host };
}
const flush = () => new Promise((r) => setImmediate(r));
const type = (ctl, v) => { ctl.input.value = v; ctl.input.fire('input', {}); };
const labels = (ctl) => ctl.state.shown.map((e) => e.label);
const key = (ctl, k) => { const ev = {}; ctl.input.fire('keydown', Object.assign(ev, { key: k })); };

(async () => {
  // nothing is requested before the palette opens, or while it is built
  {
    const f = fixture();
    await flush();
    assert.deepStrictEqual(f.calls, []);
    assert.strictEqual(f.ctl.isOpen(), false);
  }

  // findsPagesAndActions
  {
    const f = fixture();
    f.ctl.open(); await flush();
    assert.strictEqual(f.ctl.isOpen(), true);
    assert.ok(f.ctl.input.focused);
    // with no query: the actions, then the pages; resources only appear once something is typed
    assert.deepStrictEqual(labels(f.ctl), ['Enrol device', 'Overview', 'Nodes', 'Devices', 'Activity']);
    type(f.ctl, 'enrol');
    assert.deepStrictEqual(labels(f.ctl), ['Enrol device']);
    assert.strictEqual(f.ctl.state.shown[0].kind, 'Action');
    type(f.ctl, 'dev');
    assert.deepStrictEqual(labels(f.ctl), ['Devices', 'Enrol device', 'phone-1']);
    type(f.ctl, 'ACT');
    assert.deepStrictEqual(labels(f.ctl), ['Activity', 'Enrol device']); // the kind counts: Action
    type(f.ctl, 'zzz');
    assert.deepStrictEqual(labels(f.ctl), []);
    assert.ok(/Nothing found/.test(f.ctl.note.textContent));
    const opts = find(f.ctl.list, (x) => x.attrs.role === 'option');
    assert.strictEqual(opts.length, 0);
  }

  // findsResources
  {
    const f = fixture();
    f.ctl.open(); await flush();
    type(f.ctl, 'phone');
    assert.deepStrictEqual(f.ctl.state.shown.map((e) => [e.label, e.kind, e.path]), [['phone-1', 'Device', '/enroll']]);
    type(f.ctl, 'exit');
    assert.deepStrictEqual(labels(f.ctl), ['exit-a']);
    // several words, in any order, and the kind counts
    type(f.ctl, 'demo wiki');
    assert.deepStrictEqual(labels(f.ctl), ['wiki.demo']);
    type(f.ctl, 'service');
    assert.deepStrictEqual(labels(f.ctl), ['wiki.demo']);
    // the list shows the kind in words beside the label
    assert.ok(text(f.ctl.list).includes('Service'));
  }

  // arrowsAndEnterNavigate
  {
    const f = fixture();
    f.ctl.open(); await flush();
    assert.strictEqual(f.ctl.state.active, 0);
    key(f.ctl, 'ArrowDown'); key(f.ctl, 'ArrowDown');
    assert.strictEqual(f.ctl.state.active, 2);
    const opts = find(f.ctl.list, (x) => x.attrs.role === 'option');
    assert.deepStrictEqual(opts.map((o) => o.attrs['aria-selected']), ['false', 'false', 'true', 'false', 'false']);
    assert.strictEqual(f.ctl.input.getAttribute('aria-activedescendant'), 'palette-opt-2');
    key(f.ctl, 'ArrowUp');
    assert.strictEqual(f.ctl.state.active, 1);
    key(f.ctl, 'ArrowUp'); key(f.ctl, 'ArrowUp'); // wraps
    assert.strictEqual(f.ctl.state.active, 4);
    key(f.ctl, 'ArrowDown');
    assert.strictEqual(f.ctl.state.active, 0);
    key(f.ctl, 'ArrowDown');
    key(f.ctl, 'Enter');
    assert.deepStrictEqual(f.nav, ['/']);
    assert.strictEqual(f.ctl.isOpen(), false);
  }
  {
    // typing narrows the list and Enter opens the best match; a click opens the one clicked
    const f = fixture();
    f.ctl.open(); await flush();
    type(f.ctl, 'exit');
    key(f.ctl, 'Enter');
    assert.deepStrictEqual(f.nav, ['/nodes?sel=exit-a']);
    f.ctl.open(); await flush();
    const opts = find(f.ctl.list, (x) => x.attrs.role === 'option');
    opts[2].fire('click', {});
    assert.deepStrictEqual(f.nav, ['/nodes?sel=exit-a', '/nodes']);
    // Enter with nothing listed does nothing
    f.ctl.open(); await flush();
    type(f.ctl, 'zzz');
    key(f.ctl, 'Enter');
    assert.strictEqual(f.nav.length, 2);
    assert.strictEqual(f.ctl.isOpen(), true);
  }

  // escapeCloses
  {
    const f = fixture();
    f.ctl.open(); await flush();
    key(f.ctl, 'Escape');
    assert.strictEqual(f.ctl.isOpen(), false);
    assert.deepStrictEqual(f.nav, []);
    // a click on the backdrop (the dialog itself) closes it too
    f.ctl.open(); await flush();
    f.ctl.dialog.fire('click', { target: f.ctl.dialog });
    assert.strictEqual(f.ctl.isOpen(), false);
    // an answer that arrives after the palette was closed is dropped, and a reopened palette starts empty
    let release;
    const gate = new Promise((r) => { release = r; });
    const slow = pal.create({ host: node('div'), api: async (m, u) => { await gate; return u === '/_nav.json' ? NAV : SEARCH; }, navigate() {}, actions: [] });
    slow.open(); slow.close(); release(); await flush();
    assert.deepStrictEqual(slow.state.nav, []);
  }

  // shortcutIgnoredWhileTypingExceptInThePalette
  {
    const f = fixture();
    const ev = (target, over) => Object.assign({ key: 'k', ctrlKey: true, target, preventDefault() { this.prevented = true; } }, over);
    const page = ev({ tagName: 'DIV' });
    assert.strictEqual(pal.onShortcut(f.ctl, page), true);
    assert.ok(page.prevented);
    assert.strictEqual(f.ctl.isOpen(), true);
    assert.strictEqual(pal.onShortcut(f.ctl, ev(f.ctl.input)), true); // the palette's own field: closes
    assert.strictEqual(f.ctl.isOpen(), false);
    const field = ev({ tagName: 'INPUT' });
    assert.strictEqual(pal.onShortcut(f.ctl, field), false);
    assert.ok(!field.prevented);
    assert.strictEqual(pal.onShortcut(f.ctl, ev({ tagName: 'TEXTAREA' })), false);
    assert.strictEqual(pal.onShortcut(f.ctl, ev({ tagName: 'DIV', isContentEditable: true })), false);
    assert.strictEqual(f.ctl.isOpen(), false);
    assert.strictEqual(pal.onShortcut(f.ctl, ev({ tagName: 'DIV' }, { ctrlKey: false, metaKey: true })), true); // Cmd-K
    assert.strictEqual(pal.onShortcut(f.ctl, ev({ tagName: 'DIV' }, { key: 'j' })), false);
    assert.strictEqual(pal.onShortcut(f.ctl, ev({ tagName: 'DIV' }, { shiftKey: true })), false);
    assert.strictEqual(pal.onShortcut(f.ctl, ev({ tagName: 'DIV' }, { ctrlKey: false })), false);
  }

  // noSecretsRequested
  {
    const f = fixture();
    f.ctl.open(); await flush();
    type(f.ctl, 'phone'); type(f.ctl, 'phone-1 and more'); key(f.ctl, 'ArrowDown');
    f.ctl.close(); f.ctl.open(); await flush();
    assert.ok(f.calls.length >= 4);
    f.calls.forEach((c) => assert.ok(c === 'GET /_nav.json' || c === 'GET /_search.json', c));
  }

  // hostileLabelIsText
  {
    const hostile = '<img src=x onerror=alert(1)>';
    const f = fixture({
      nav: [{ label: hostile, path: '/nodes' }, { label: 'Safe', path: 'javascript:alert(1)' }, { label: 'Other', path: '//evil.test/x' }, { label: 'Back', path: '/a\\b' }],
      search: { items: [{ label: 'abc‮def\u0000', kind: '<b>Node</b>', path: '/nodes' }, { label: '', kind: 'Node', path: '/nodes' }, { label: 'ok', kind: 'Node', path: 'https://evil.test/' }] },
      actions: [{ label: hostile, path: '/enroll', icon: '<svg onload=x>' }],
    });
    f.ctl.open(); await flush();
    type(f.ctl, 'img');
    const shown = f.ctl.state.shown;
    assert.deepStrictEqual(shown.map((e) => e.label), [hostile, hostile]);
    const rows = find(f.ctl.list, (x) => x.attrs.role === 'option');
    assert.strictEqual(rows.length, 2);
    rows.forEach((r) => {
      const spans = find(r, (x) => x.tag === 'span' && x.textContent === hostile);
      assert.strictEqual(spans.length, 1, 'the label is the text content of one span');
      find(r, () => true).forEach((x) => assert.ok(!/^(img|b|script)$/.test(x.tag), x.tag));
    });
    // an unknown icon name draws nothing; unsafe paths and empty labels never reach the list
    type(f.ctl, '');
    const all = f.ctl.state.all.map((e) => e.label + '|' + e.path);
    assert.deepStrictEqual(all, [hostile + '|/enroll', hostile + '|/nodes', 'abcdef|/nodes']);
    assert.strictEqual(f.ctl.state.all[2].kind, '<b>Node</b>');
    assert.strictEqual(find(f.ctl.list, (x) => x.tag === 'svg').length, 0, 'an unknown icon name draws nothing');
  }

  // worksWithFailedSearch
  {
    const f = fixture({ searchFails: true });
    f.ctl.open(); await flush();
    assert.deepStrictEqual(labels(f.ctl), ['Enrol device', 'Overview', 'Nodes', 'Devices', 'Activity']);
    assert.ok(/Search results are not available/.test(f.ctl.note.textContent));
    type(f.ctl, 'nodes');
    assert.deepStrictEqual(labels(f.ctl), ['Nodes']);
    key(f.ctl, 'Enter');
    assert.deepStrictEqual(f.nav, ['/nodes']);
    // pages that cannot be loaded leave the actions and the resources
    const g = fixture({ navFails: true });
    g.ctl.open(); await flush();
    assert.deepStrictEqual(labels(g.ctl), ['Enrol device']);
    assert.ok(/pages could not be loaded/.test(g.ctl.note.textContent));
    type(g.ctl, 'phone');
    assert.deepStrictEqual(labels(g.ctl), ['phone-1']);
    // both failing still leaves the actions
    const h = fixture({ navFails: true, searchFails: true });
    h.ctl.open(); await flush();
    assert.deepStrictEqual(labels(h.ctl), ['Enrol device']);
  }

  // loadingEndsEvenIfRenderThrows
  {
    const bad = { get label() { throw new Error('boom'); }, kind: 'Node', path: '/nodes' };
    const f = fixture({ search: { items: [bad] } });
    f.ctl.open(); await flush(); await flush();
    assert.strictEqual(f.ctl.state.loading, false);
    // and the palette still works afterwards
    assert.doesNotThrow(() => f.ctl.close());
  }

  // the dialog is labelled and the field is a combobox
  {
    const f = fixture();
    assert.strictEqual(f.ctl.dialog.attrs['aria-label'], 'Search or run a command');
    assert.strictEqual(f.ctl.input.attrs.role, 'combobox');
    assert.strictEqual(f.host.children[0], f.ctl.dialog);
  }
  console.log('palette self-test ok');
})().catch((e) => { console.error(e); process.exit(1); });
