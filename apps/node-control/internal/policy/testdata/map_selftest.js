// Run with: node map_selftest.js <path to hl.js> <path to map.js>. Exits non-zero on a failed check.
// The policy map page (#429) against a small DOM stand-in and a fake Cytoscape.js that records what the page asks.
const assert = require('assert');
const path = require('path');

class FakeNode {
  constructor(tag) {
    this.tag = tag; this.children = []; this.parentNode = null; this.attrs = {}; this.listeners = {}; this.style = {};
    this.className = ''; this._text = ''; this.hidden = false; this.disabled = false; this.value = '';
  }
  get textContent() { return this._text + this.children.map((c) => c.textContent).join(''); }
  set textContent(v) { this._text = String(v); this.children = []; }
  get firstChild() { return this.children[0] || null; }
  appendChild(c) { if (c.parentNode) { c.parentNode.removeChild(c); } c.parentNode = this; this.children.push(c); return c; }
  removeChild(c) { const i = this.children.indexOf(c); if (i >= 0) { this.children.splice(i, 1); } c.parentNode = null; return c; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  removeAttribute(k) { delete this.attrs[k]; }
  addEventListener(t, f) { (this.listeners[t] = this.listeners[t] || []).push(f); }
  set innerHTML(v) { throw new Error('innerHTML must not be used'); }
  set outerHTML(v) { throw new Error('outerHTML must not be used'); }
  click() { if (!this.disabled) { fire(this, 'click'); } }
}
const fire = (n, type) => (n.listeners[type] || []).forEach((f) => f({ preventDefault() {}, target: n }));
const walk = (n, out = []) => { out.push(n); n.children.forEach((c) => walk(c, out)); return out; };
const findAll = (root, pred) => walk(root).filter(pred);

global.document = {
  createElement: (t) => new FakeNode(t), createElementNS: (_, t) => new FakeNode(t),
  createTextNode: (s) => { const n = new FakeNode('#text'); n._text = s; return n; },
  getElementById: () => null, querySelector: () => null, querySelectorAll: () => [], addEventListener() {}, hidden: false,
  documentElement: new FakeNode('html'),
};
global.window = {
  location: { search: '', pathname: '/policy/map', href: 'http://localhost/policy/map', reload() {} },
  history: { replaceState(_, __, url) { global.window.location.search = url.includes('?') ? url.slice(url.indexOf('?')) : ''; } },
  addEventListener() {},
};
global.setInterval = () => 0;

// A fake Cytoscape.js: elements are { group, data, classes }, collections wrap arrays of them.
function fakeCytoscape() {
  const made = [];
  function factory(opts) {
    const cy = { opts, els: [], handlers: [], layouts: [], styles: [opts.style], fits: 0, resizes: 0 };
    const coll = (items) => {
      const c = {
        items, length: items.length,
        addClass(cls) { items.forEach((e) => cls.split(' ').forEach((k) => e.classes.add(k))); return c; },
        removeClass(cls) { items.forEach((e) => cls.split(' ').forEach((k) => e.classes.delete(k))); return c; },
        union(o) { return coll([...new Set(items.concat(o.items))]); },
        difference(o) { return coll(items.filter((e) => !o.items.includes(e))); },
        remove() { cy.els = cy.els.filter((e) => !items.includes(e)); return c; },
        isNode() { return items[0].group === 'nodes'; },
        id() { return items[0].data.id; },
        closedNeighborhood() {
          const n = items[0].data.id;
          const es = cy.els.filter((e) => e.group === 'edges' && (e.data.source === n || e.data.target === n));
          const ends = new Set(es.flatMap((e) => [e.data.source, e.data.target]).concat([n]));
          return coll(cy.els.filter((e) => es.includes(e) || (e.group === 'nodes' && ends.has(e.data.id))));
        },
        connectedNodes() { const d = items[0].data; return coll(cy.els.filter((e) => e.data.id === d.source || e.data.id === d.target)); },
      };
      return c;
    };
    cy.coll = coll;
    cy.elements = () => coll(cy.els.slice());
    cy.collection = () => coll([]);
    cy.getElementById = (id) => coll(cy.els.filter((e) => e.data.id === id));
    cy.add = (list) => {
      list.forEach((x) => {
        if (x.group === 'edges') {
          const ids = new Set(cy.els.concat(list).filter((e) => e.group === 'nodes').map((e) => e.data.id));
          if (!ids.has(x.data.source) || !ids.has(x.data.target)) { throw new Error('edge to a missing node ' + x.data.id); }
        }
      });
      list.forEach((x) => cy.els.push({ group: x.group, data: x.data, classes: new Set(String(x.classes || '').split(' ').filter(Boolean)) }));
    };
    cy.batch = (fn) => fn();
    cy.on = (events, sel, fn) => { if (typeof sel === 'function') { fn = sel; sel = null; } events.split(' ').forEach((ev) => cy.handlers.push({ ev, sel, fn })); };
    cy.emit = (ev, target, pos) => cy.handlers.filter((h) => h.ev === ev && (!h.sel || target !== cy)).forEach((h) => h.fn({ target, renderedPosition: pos }));
    cy.layout = (o) => ({ run() { cy.layouts.push(o.name); } });
    cy.fit = () => { cy.fits++; };
    cy.resize = () => { cy.resizes++; };
    cy.style = (s) => { cy.styles.push(s); };
    cy.byKey = (key) => cy.coll(cy.els.filter((e) => e.data.key === key));
    cy.classesOf = (key) => [...cy.els.find((e) => e.data.key === key).classes].sort().join(' ');
    made.push(cy);
    return cy;
  }
  factory.made = made;
  return factory;
}

require(path.resolve(process.argv[2]));
require(path.resolve(process.argv[3]));
const hl = global.window.hl;
const pm = hl.policymap;
assert.ok(pm, 'hl.policymap is defined');

const ref = (section, index, line) => ({ section, index, line });
const GRAPH = {
  nodes: [
    { id: 'group:admins', kind: 'group', label: 'group:admins', rule_types: ['acls', 'ssh'], members: ['alice@example.com'] },
    { id: 'tag:web', kind: 'tag', label: 'tag:web', rule_types: ['acls'], owners: ['group:admins'] },
    { id: 'autogroup:internet', kind: 'internet', label: 'autogroup:internet', rule_types: ['acls'] },
    { id: 'tag:db', kind: 'tag', label: 'tag:db', rule_types: [], unused: true },
    { id: 'host:files', kind: 'host', label: 'files', rule_types: ['acls'], address: '192.0.2.10' },
  ],
  edges: [
    { from: 'group:admins', to: 'tag:web', proto: 'tcp', ports: '443', refs: [ref('acls', 0, 5)] },
    { from: 'group:admins', to: 'autogroup:internet', ports: '*', refs: [ref('acls', 1, 9)] },
    { from: 'group:admins', to: 'tag:web', proto: 'ssh', ports: '22', ssh_users: ['root'], ssh_action: 'check', refs: [ref('ssh', 0, 30)] },
    { from: 'tag:web', to: 'host:files', proto: 'tcp', ports: '8080-8090', via: ['tag:exit'], refs: [ref('grants', 0, 20), ref('acls', 2, 12)] },
    { from: 'group:admins', to: 'ghost', ports: '*', refs: [ref('acls', 3, 14)] },
  ],
  warnings: ['acls[4]: unknown selector form'],
  truncated: false,
  updated_at: 'v7',
};
const COLOURS = { group: '#010101', tag: '#020202', user: '#030303', host: '#040404', cidr: '#050505', autogroup: '#060606',
  internet: '#070707', any: '#080808', unknown: '#090909', edge: '#0a0a0a', label: '#0b0b0b', hi: '#0c0c0c', bg: '#0d0d0d' };

function pageEls() {
  const mk = (tag) => new FakeNode(tag);
  const els = { root: mk('div'), sub: mk('p'), error: mk('div'), truncated: mk('div'), warnings: mk('ul'), search: mk('input'),
    layout: mk('select'), showgraph: mk('button'), showtable: mk('button'), fit: mk('button'), count: mk('span'), kinds: mk('div'),
    graphbox: mk('section'), cy: mk('div'), tip: mk('div'), legend: mk('ul'), tablebox: mk('section'), heads: mk('tr'), rows: mk('tbody') };
  els.error.hidden = true; els.truncated.hidden = true; els.tip.hidden = true; els.tablebox.hidden = true;
  return els;
}
async function mountPage(deps, graph) {
  const els = pageEls();
  const cytoscape = fakeCytoscape();
  let theme = null;
  const ctrl = pm.mount(els, Object.assign({ cytoscape, load: () => Promise.resolve(graph || GRAPH), colours: () => COLOURS,
    narrow: () => false, onTheme: (fn) => { theme = fn; } }, deps));
  await ctrl.load();
  return { els, ctrl, cy: cytoscape.made[0], theme: () => theme };
}
const edgeIDs = (list) => list.filter((x) => x.group === 'edges').map((x) => x.data.id).sort();
const rowIDs = (rows) => rows.map((r) => r.id).sort();
const tableRows = (els) => els.rows.children.filter((tr) => tr.getAttribute('data-edge'));

(async () => {
  // accessAndRefsAreShortText
  {
    assert.strictEqual(pm.access({ proto: 'tcp', ports: '443' }), 'tcp:443');
    assert.strictEqual(pm.access({ ports: '*' }), '*');
    assert.strictEqual(pm.access({ proto: 'ssh', ports: '22', ssh_users: ['root', 'autogroup:nonroot'], ssh_action: 'check' }), 'ssh:22 as root, autogroup:nonroot check');
    assert.strictEqual(pm.access({ apps: ['example.com/cap/web'] }), 'apps example.com/cap/web');
    assert.strictEqual(pm.access({}), 'any');
    assert.strictEqual(pm.refText(ref('acls', 3, 42)), 'acls[3], line 42');
  }

  // tableHasTheSameEdgesAsTheGraph: also with a kind hidden; an edge to a node the answer does not have is in neither
  {
    for (const hidden of [{}, { tag: true }, { internet: true, host: true }, { group: true }]) {
      const v = pm.visible(GRAPH, hidden);
      assert.deepStrictEqual(rowIDs(pm.rows(v, '')), edgeIDs(pm.elements(v)), JSON.stringify(hidden));
    }
    const all = pm.visible(GRAPH, {});
    assert.deepStrictEqual(edgeIDs(pm.elements(all)), ['e0', 'e1', 'e2', 'e3']);
    assert.deepStrictEqual(edgeIDs(pm.elements(pm.visible(GRAPH, { tag: true }))), ['e1']);
    const els = pm.elements(all);
    assert.ok(els.every((x) => /^[ne][0-9]+$/.test(x.data.id)), 'element ids never carry policy names');
    assert.strictEqual(els.find((x) => x.data.key === 'tag:db').classes, 'k-tag unused');
    assert.strictEqual(els.find((x) => x.data.id === 'e2').classes, 'ssh');
  }

  // sheetUsesTheColoursAndAShapePerKind
  {
    const s = pm.sheet(COLOURS);
    const rule = (sel) => s.find((r) => r.selector === sel).style;
    assert.strictEqual(rule('node.k-group')['background-color'], '#010101');
    assert.strictEqual(rule('node.k-tag').shape, 'diamond');
    assert.strictEqual(rule('edge')['line-color'], '#0a0a0a');
    assert.strictEqual(rule('edge.match, edge.hi')['line-color'], '#0c0c0c');
    const prev = global.window.getComputedStyle;
    global.window.getComputedStyle = () => ({ getPropertyValue: (k) => (k === '--map-group' ? ' #123456 ' : '') });
    const c = pm.readColours(new FakeNode('div'));
    global.window.getComputedStyle = prev;
    assert.strictEqual(c.group, '#123456');
    assert.strictEqual(c.tag, '#888888', 'a missing token falls back');
  }

  // loadDrawsGraphTableLegendAndKinds
  {
    const { els, cy } = await mountPage();
    assert.strictEqual(els.root.getAttribute('data-map-state'), 'ready');
    assert.strictEqual(cy.opts.container, els.cy);
    assert.strictEqual(cy.els.filter((e) => e.group === 'nodes').length, 5);
    assert.deepStrictEqual(edgeIDs(cy.els), ['e0', 'e1', 'e2', 'e3']);
    assert.deepStrictEqual(cy.layouts, ['cose'], 'cose is the default layout');
    assert.strictEqual(tableRows(els).length, 4);
    assert.deepStrictEqual(tableRows(els).map((tr) => tr.getAttribute('data-edge')).sort(), edgeIDs(cy.els), 'table = graph');
    assert.strictEqual(els.count.textContent, '5 selectors, 4 paths');
    assert.strictEqual(els.sub.textContent, 'Live policy, version v7.');
    assert.strictEqual(els.truncated.hidden, true);
    assert.strictEqual(els.warnings.children.length, 1);
    assert.ok(els.legend.textContent.includes('Internet (exit node)') && els.legend.textContent.includes('SSH path'));
    assert.deepStrictEqual(els.kinds.children.map((b) => b.textContent), ['Group', 'Tag', 'Host', 'Internet (exit node)'], 'chips only for present kinds');
    assert.strictEqual(els.graphbox.hidden, false);
    assert.strictEqual(els.tablebox.hidden, true);
    assert.strictEqual(els.showgraph.getAttribute('aria-pressed'), 'true');
    assert.deepStrictEqual(cy.styles[0], pm.sheet(COLOURS));
    const rules = tableRows(els).find((tr) => tr.getAttribute('data-edge') === 'e3').children[3].textContent;
    assert.strictEqual(rules, 'grants[0], line 20; acls[2], line 12');
  }

  // layoutSwitchesAndIsKeptInTheURL
  {
    const { els, ctrl, cy } = await mountPage();
    els.layout.value = 'breadthfirst';
    fire(els.layout, 'change');
    assert.deepStrictEqual(cy.layouts, ['cose', 'breadthfirst']);
    assert.strictEqual(global.window.location.search, '?layout=breadthfirst');
    ctrl.setLayout('nonsense');
    assert.deepStrictEqual(cy.layouts, ['cose', 'breadthfirst'], 'an unknown layout is ignored');
    const again = await mountPage();
    assert.deepStrictEqual(again.cy.layouts, ['breadthfirst'], 'the layout from the URL');
    ctrl.setLayout('cose');
    assert.strictEqual(global.window.location.search, '');
  }

  // searchHighlightsMatchesAndFiltersTheTable
  {
    const { els, cy } = await mountPage();
    els.search.value = 'files';
    fire(els.search, 'input');
    assert.ok(cy.classesOf('host:files').includes('match'));
    assert.ok(cy.classesOf('group:admins').includes('dim'));
    assert.ok(cy.getElementById('e3').items[0].classes.has('match'), 'the edge to the match');
    assert.deepStrictEqual(tableRows(els).map((tr) => tr.getAttribute('data-edge')), ['e3']);
    assert.strictEqual(els.count.textContent, '5 selectors, 4 paths, 1 match the search');
    els.search.value = '443';
    fire(els.search, 'input');
    assert.ok(cy.getElementById('e0').items[0].classes.has('match'), 'a port matches');
    assert.ok(cy.classesOf('tag:web').includes('dim'), 'a node matches by name only');
    els.search.value = 'SSH';
    fire(els.search, 'input');
    assert.deepStrictEqual(tableRows(els).map((tr) => tr.getAttribute('data-edge')), ['e2'], 'a protocol matches, any case');
    els.search.value = 'zzz';
    fire(els.search, 'input');
    assert.strictEqual(tableRows(els).length, 0);
    assert.strictEqual(els.rows.textContent, 'No path matches the search.');
    els.search.value = '';
    fire(els.search, 'input');
    assert.ok(cy.els.every((e) => e.classes.size === 0 || [...e.classes].every((k) => !['match', 'dim', 'hi'].includes(k))));
  }

  // tapHighlightsNeighboursAndShowsTheTooltip
  {
    const { els, cy } = await mountPage();
    cy.emit('tap', cy.byKey('tag:web'), { x: 100, y: 50 });
    assert.ok(cy.classesOf('tag:web').includes('hi'));
    assert.ok(cy.classesOf('group:admins').includes('hi'), 'a neighbour');
    assert.ok(cy.classesOf('host:files').includes('hi'), 'a neighbour');
    assert.ok(cy.classesOf('autogroup:internet').includes('dim'), 'not a neighbour');
    assert.ok(cy.getElementById('e1').items[0].classes.has('dim'));
    assert.strictEqual(els.tip.hidden, false);
    assert.ok(els.tip.textContent.includes('Kind: Tag') && els.tip.textContent.includes('Owners: group:admins'));
    assert.ok(els.tip.textContent.includes('Paths: 1 out, 2 in'));
    assert.strictEqual(els.tip.style.left, '112px', 'placed through the CSSOM');
    cy.emit('mouseout', cy.byKey('tag:web'));
    assert.strictEqual(els.tip.hidden, false, 'a tapped tip stays');
    cy.emit('tap', cy);
    assert.strictEqual(els.tip.hidden, true);
    assert.ok(!cy.classesOf('autogroup:internet').includes('dim'), 'a tap on the background clears');
    // an edge: its ends stay, the rest dims, and the tooltip has the rules with line numbers
    cy.emit('tap', cy.getElementById('e3'), { x: 1, y: 1 });
    assert.ok(cy.classesOf('host:files').includes('hi') && cy.classesOf('group:admins').includes('dim'));
    assert.ok(els.tip.textContent.includes('tag:web → files'));
    assert.ok(els.tip.textContent.includes('Via: tag:exit'));
    assert.ok(els.tip.textContent.includes('Rules: grants[0], line 20; acls[2], line 12'));
    cy.emit('tap', cy);
    cy.emit('mouseover', cy.getElementById('e2'), { x: 5, y: 5 });
    assert.ok(els.tip.textContent.includes('Access: ssh:22 as root check'));
    cy.emit('mouseout', cy.getElementById('e2'));
    assert.strictEqual(els.tip.hidden, true, 'a hover tip goes');
  }

  // kindFilterRemovesNodesAndKeepsTableAndGraphTheSame
  {
    const { els, ctrl, cy } = await mountPage();
    els.kinds.children.find((b) => b.textContent === 'Tag').click();
    assert.ok(!cy.els.some((e) => e.data.kind === 'tag'));
    assert.deepStrictEqual(edgeIDs(cy.els), ['e1']);
    assert.deepStrictEqual(rowIDs(ctrl.rows()), ['e1']);
    assert.deepStrictEqual(tableRows(els).map((tr) => tr.getAttribute('data-edge')), ['e1']);
    const chip = els.kinds.children.find((b) => b.textContent === 'Tag');
    assert.strictEqual(chip.getAttribute('aria-pressed'), 'false');
    assert.deepStrictEqual(cy.layouts, ['cose', 'cose'], 'a new layout after a filter');
    chip.click();
    assert.deepStrictEqual(edgeIDs(cy.els), ['e0', 'e1', 'e2', 'e3']);
  }

  // tableViewAndSort
  {
    const { els, cy } = await mountPage();
    els.showtable.click();
    assert.strictEqual(els.graphbox.hidden, true);
    assert.strictEqual(els.tablebox.hidden, false);
    assert.strictEqual(els.showtable.getAttribute('aria-pressed'), 'true');
    assert.strictEqual(els.fit.disabled, true);
    const head = (name) => els.heads.children.find((th) => th.textContent === name);
    head('Rules').children[0].click();
    assert.deepStrictEqual(tableRows(els).map((tr) => tr.getAttribute('data-edge')), ['e0', 'e1', 'e3', 'e2'], 'by first rule line');
    assert.strictEqual(head('Rules').getAttribute('aria-sort'), 'ascending');
    head('Rules').children[0].click();
    assert.strictEqual(head('Rules').getAttribute('aria-sort'), 'descending');
    assert.strictEqual(tableRows(els)[0].getAttribute('data-edge'), 'e2');
    els.showgraph.click();
    assert.strictEqual(els.graphbox.hidden, false);
    assert.ok(cy.resizes > 0, 'the canvas is resized when it comes back');
    els.fit.click();
    assert.ok(cy.fits > 0);
  }

  // themeChangeSetsTheStyleAgain
  {
    let colours = COLOURS;
    const { cy, theme } = await mountPage({ colours: () => colours });
    colours = Object.assign({}, COLOURS, { group: '#fefefe' });
    theme()();
    const last = cy.styles[cy.styles.length - 1];
    assert.strictEqual(last.find((r) => r.selector === 'node.k-group').style['background-color'], '#fefefe');
  }

  // narrowScreenStartsWithTheTable
  {
    const { els, cy } = await mountPage({ narrow: () => true });
    assert.strictEqual(els.tablebox.hidden, false);
    assert.strictEqual(els.graphbox.hidden, true);
    assert.deepStrictEqual(cy.layouts, [], 'no layout on a hidden canvas');
    els.showgraph.click();
    assert.deepStrictEqual(cy.layouts, ['cose'], 'the layout runs when the graph is shown');
  }

  // withoutTheLibraryTheTableStillWorks
  {
    const { els } = await mountPage({ cytoscape: null });
    assert.strictEqual(els.root.getAttribute('data-map-state'), 'ready');
    assert.ok(els.error.textContent.includes('The table shows the same paths'));
    assert.strictEqual(els.tablebox.hidden, false);
    assert.strictEqual(els.showgraph.disabled, true);
    assert.strictEqual(tableRows(els).length, 4);
    els.showgraph.click();
    assert.strictEqual(els.tablebox.hidden, false);
  }

  // truncatedGraphShowsTheNote
  {
    const { els } = await mountPage({}, Object.assign({}, GRAPH, { truncated: true }));
    assert.strictEqual(els.truncated.hidden, false);
  }

  // emptyPolicySaysSo
  {
    const { els, cy } = await mountPage({}, { nodes: [], edges: [], warnings: [], truncated: false, updated_at: '' });
    assert.strictEqual(cy.els.length, 0);
    assert.strictEqual(els.rows.textContent, 'The policy gives no paths.');
    assert.strictEqual(els.sub.textContent, 'Live policy.');
  }

  // loadErrorIsShown
  {
    const err = Object.assign(new Error('headscale is unreachable or failed'), { status: 502 });
    const { els } = await mountPage({ load: () => Promise.reject(err) });
    assert.strictEqual(els.root.getAttribute('data-map-state'), 'error');
    assert.strictEqual(els.error.hidden, false);
    assert.ok(els.error.textContent.includes('headscale is unreachable'));
  }

  // hostileNamesStayText: a label with markup goes in as text (FakeNode throws on innerHTML)
  {
    const g = { nodes: [{ id: 'group:<img src=x>', kind: 'group', label: '<b>x</b>' }, { id: 'tag:a', kind: 'weird' }],
      edges: [{ from: 'group:<img src=x>', to: 'tag:a', ports: '*', refs: [] }], warnings: ['<script>'], truncated: false };
    const { els, cy } = await mountPage({}, g);
    assert.ok(tableRows(els)[0].textContent.includes('<b>x</b>'));
    assert.strictEqual(cy.els.find((e) => e.data.key === 'tag:a').data.kind, 'unknown', 'an unknown kind');
    assert.strictEqual(els.warnings.textContent, '<script>');
  }

  console.log('map self-test ok');
})().catch((e) => { console.error(e); process.exit(1); });
