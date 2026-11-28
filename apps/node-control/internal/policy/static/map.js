// headlamp console: the policy map (#429, ADR 0005 section 5). Needs /_ui/hl.js first (the shell loads it) and the
// vendored Cytoscape.js core (static/vendor/cytoscape.min.js). It draws GET /policy/api/map as a graph and as a table
// of the same edges: a screen reader cannot read the canvas, and the table also serves a narrow screen and a truncated
// graph. Everything that came from the network goes in through textContent or Cytoscape data, never as markup.
// The idea of a policy map comes from tailscale-network-topology-mapper by SimplyMinimal
// (github.com/SimplyMinimal/tailscale-network-topology-mapper, Apache-2.0). This file copies no code from it.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var KINDS = ['group', 'tag', 'user', 'host', 'cidr', 'autogroup', 'internet', 'any', 'unknown'];
  var KIND_LABEL = { group: 'Group', tag: 'Tag', user: 'User', host: 'Host', cidr: 'CIDR', autogroup: 'Autogroup',
    internet: 'Internet (exit node)', any: 'Any (*)', unknown: 'Unknown' };
  var SHAPE = { group: 'round-rectangle', tag: 'diamond', user: 'ellipse', host: 'rectangle', cidr: 'hexagon',
    autogroup: 'round-octagon', internet: 'star', any: 'star', unknown: 'ellipse' };
  var LAYOUTS = {
    cose: { name: 'cose', animate: false, nodeDimensionsIncludeLabels: true, randomize: true },
    breadthfirst: { name: 'breadthfirst', directed: true, spacingFactor: 1.25, animate: false }
  };
  var COLUMNS = [['from', 'Source'], ['to', 'Destination'], ['access', 'Access'], ['rules', 'Rules']];
  // The colour names map.css defines as --map-<name>, from the hl.css tokens.
  var COLOURS = KINDS.concat(['edge', 'label', 'hi', 'bg']);
  var NARROW = '(max-width: 760px)';

  function label(v, n) { return hl.plain(v, n || 200); }
  function kindOf(k) { return KINDS.indexOf(k) >= 0 ? k : 'unknown'; }
  function list(v) { return hl.arr(v).map(function (x) { return label(x, 100); }).join(', '); }

  // access is the short text of an edge's access: protocol and ports, apps, and SSH users.
  function access(e) {
    e = hl.obj(e);
    var parts = [];
    if (e.proto || e.ports) { parts.push((e.proto ? label(e.proto, 20) + ':' : '') + (e.ports ? label(e.ports, 200) : '*')); }
    if (hl.arr(e.apps).length) { parts.push('apps ' + list(e.apps)); }
    if (hl.arr(e.ssh_users).length) { parts.push('as ' + list(e.ssh_users)); }
    if (e.ssh_action) { parts.push(label(e.ssh_action, 20)); }
    return parts.join(' ') || 'any';
  }

  function refText(r) {
    r = hl.obj(r);
    return label(r.section, 20) + '[' + Number(r.index) + '], line ' + Number(r.line);
  }

  function refsText(e) { return hl.arr(hl.obj(e).refs).map(refText).join('; '); }

  // visible is the part of the graph that the page shows, the same for the graph and the table: the nodes whose kind
  // is not hidden, and the edges with both ends among them. Each item keeps its index in the answer.
  function visible(graph, hidden) {
    graph = hl.obj(graph);
    hidden = hidden || {};
    var byID = {};
    var nodes = [];
    hl.arr(graph.nodes).forEach(function (n, i) {
      n = hl.obj(n);
      var id = label(n.id, 300);
      var kind = kindOf(n.kind);
      if (!id || hidden[kind] || byID[id]) { return; }
      var item = { idx: i, id: id, kind: kind, label: label(n.label || n.id, 300), node: n };
      byID[id] = item;
      nodes.push(item);
    });
    var edges = [];
    hl.arr(graph.edges).forEach(function (e, i) {
      e = hl.obj(e);
      var from = byID[label(e.from, 300)];
      var to = byID[label(e.to, 300)];
      if (!from || !to) { return; }
      edges.push({ idx: i, from: from, to: to, access: access(e), rules: refsText(e), edge: e,
        ssh: e.proto === 'ssh' || hl.arr(e.ssh_users).length > 0 });
    });
    return { nodes: nodes, edges: edges, byID: byID };
  }

  // elements turns the visible part into Cytoscape elements. Ids are n<index> and e<index>, so no name from the
  // policy is ever parsed as a Cytoscape selector.
  function elements(v) {
    var out = [];
    v.nodes.forEach(function (n) {
      var cls = 'k-' + n.kind + (n.node.unused ? ' unused' : '');
      out.push({ group: 'nodes', data: { id: 'n' + n.idx, key: n.id, label: n.label, kind: n.kind }, classes: cls });
    });
    v.edges.forEach(function (e) {
      out.push({ group: 'edges', data: { id: 'e' + e.idx, source: 'n' + e.from.idx, target: 'n' + e.to.idx },
        classes: e.ssh ? 'ssh' : '' });
    });
    return out;
  }

  function nodeMatches(n, q) {
    return n.id.toLowerCase().indexOf(q) >= 0 || n.label.toLowerCase().indexOf(q) >= 0;
  }

  // edgeMatches: the search finds an edge by its ends, protocol, ports, apps and SSH users.
  function edgeMatches(e, q) {
    return nodeMatches(e.from, q) || nodeMatches(e.to, q) || e.access.toLowerCase().indexOf(q) >= 0;
  }

  // rows are the table rows of the visible edges that match the search, sorted by a column.
  function rows(v, q, sortKey, desc) {
    q = String(q || '').trim().toLowerCase();
    var out = v.edges.filter(function (e) { return !q || edgeMatches(e, q); }).map(function (e) {
      var first = hl.arr(e.edge.refs)[0];
      return { id: 'e' + e.idx, from: e.from.label, to: e.to.label, access: e.access, rules: e.rules,
        line: first ? Number(hl.obj(first).line) || 0 : 0 };
    });
    var key = sortKey || 'from';
    out.sort(function (a, b) {
      var c = key === 'rules' ? a.line - b.line : String(a[key]).localeCompare(String(b[key]));
      if (c === 0) { c = a.from.localeCompare(b.from) || a.to.localeCompare(b.to) || a.access.localeCompare(b.access); }
      return desc ? -c : c;
    });
    return out;
  }

  function nodeTip(n, v) {
    var d = n.node;
    var out = 0;
    var into = 0;
    v.edges.forEach(function (e) { if (e.from === n) { out++; } if (e.to === n) { into++; } });
    var lines = [n.label, 'Kind: ' + KIND_LABEL[n.kind]];
    if (hl.arr(d.rule_types).length) { lines.push('Used in: ' + list(d.rule_types)); }
    if (d.address) { lines.push('Address: ' + label(d.address, 100)); }
    if (hl.arr(d.members).length) { lines.push('Members: ' + list(d.members.slice(0, 20)) + (d.members.length > 20 ? ', …' : '')); }
    if (hl.arr(d.owners).length) { lines.push('Owners: ' + list(d.owners.slice(0, 20)) + (d.owners.length > 20 ? ', …' : '')); }
    if (hl.arr(d.approves).length) { lines.push('Approves: ' + list(d.approves.slice(0, 20))); }
    if (d.unused) { lines.push('No rule uses it.'); }
    lines.push('Paths: ' + out + ' out, ' + into + ' in');
    return lines.join('\n');
  }

  function edgeTip(e) {
    var d = e.edge;
    var lines = [e.from.label + ' → ' + e.to.label, 'Access: ' + e.access];
    if (hl.arr(d.via).length) { lines.push('Via: ' + list(d.via)); }
    if (hl.arr(d.posture).length) { lines.push('Posture: ' + list(d.posture)); }
    lines.push('Rules: ' + (e.rules || 'none'));
    return lines.join('\n');
  }

  // sheet is the Cytoscape style for a set of colours (name → CSS colour). The canvas cannot read CSS variables, so
  // the page reads them (readColours) and calls this again when the theme changes.
  function sheet(c) {
    var s = [
      { selector: 'node', style: { 'label': 'data(label)', 'color': c.label, 'font-size': 10, 'text-valign': 'bottom',
        'text-margin-y': 4, 'text-wrap': 'ellipsis', 'text-max-width': 140, 'width': 18, 'height': 18,
        'background-color': c.unknown, 'border-width': 0, 'border-color': c.hi } },
      { selector: 'node.unused', style: { 'border-width': 2, 'border-style': 'dashed', 'border-color': c.label, 'background-opacity': 0.4 } },
      { selector: 'edge', style: { 'width': 1.5, 'curve-style': 'bezier', 'line-color': c.edge, 'target-arrow-color': c.edge,
        'target-arrow-shape': 'triangle', 'arrow-scale': 0.8 } },
      { selector: 'edge.ssh', style: { 'line-style': 'dashed' } }
    ];
    KINDS.forEach(function (k) {
      s.push({ selector: 'node.k-' + k, style: { 'background-color': c[k], 'shape': SHAPE[k] } });
    });
    s.push({ selector: 'node.match, node.hi', style: { 'border-width': 3, 'border-style': 'solid', 'border-color': c.hi, 'color': c.hi } });
    s.push({ selector: 'edge.match, edge.hi', style: { 'width': 2.5, 'line-color': c.hi, 'target-arrow-color': c.hi } });
    s.push({ selector: '.dim', style: { 'opacity': 0.15 } });
    return s;
  }

  // readColours reads --map-<name> from el (map.css sets them from the hl.css tokens of the current theme).
  function readColours(el) {
    var cs = window.getComputedStyle ? window.getComputedStyle(el) : null;
    var out = {};
    COLOURS.forEach(function (k) {
      var v = cs ? String(cs.getPropertyValue('--map-' + k) || '').trim() : '';
      out[k] = v || '#888888';
    });
    return out;
  }

  // mount wires the page's elements to the data. deps (all optional): cytoscape (the library), load() for the graph
  // answer, colours() for the colour set, narrow() true on a narrow screen, onTheme(fn) to hear of a theme change.
  function mount(els, deps) {
    deps = deps || {};
    var cytoscape = deps.cytoscape !== undefined ? deps.cytoscape : window.cytoscape;
    var load = deps.load || function () { return hl.api('GET', '/policy/api/map'); };
    var colours = deps.colours || function () { return readColours(els.root); };
    var narrow = deps.narrow || function () { return !!(window.matchMedia && window.matchMedia(NARROW).matches); };
    var st = {
      graph: null, view: null, hidden: {}, query: '', layout: LAYOUTS[hl.queryParam('layout')] ? hl.queryParam('layout') : 'cose',
      mode: narrow() ? 'table' : 'graph', sortKey: 'from', desc: false, selected: null, cy: null, chips: {}, laidOut: false
    };
    var ctrl = { state: st };

    function setError(text) { els.error.textContent = text || ''; els.error.hidden = !text; }

    function cyID(id) { return st.cy ? st.cy.getElementById(id) : null; }

    // highlight puts the classes for the search and the selection on the graph: a selection (a node with its
    // neighbours, or an edge with its ends) wins over the search; the rest is dimmed.
    function highlight() {
      var cy = st.cy;
      if (!cy) { return; }
      var q = st.query.trim().toLowerCase();
      cy.batch(function () {
        cy.elements().removeClass('match hi dim');
        var sel = st.selected ? cyID(st.selected) : null;
        if (sel && sel.length) {
          var keep = sel.isNode() ? sel.closedNeighborhood() : sel.union(sel.connectedNodes());
          keep.addClass('hi');
          cy.elements().difference(keep).addClass('dim');
          return;
        }
        st.selected = null;
        if (!q) { return; }
        var hits = cy.collection();
        st.view.nodes.forEach(function (n) { if (nodeMatches(n, q)) { hits = hits.union(cyID('n' + n.idx)); } });
        st.view.edges.forEach(function (e) { if (edgeMatches(e, q)) { hits = hits.union(cyID('e' + e.idx)); } });
        hits.addClass('match');
        cy.elements().difference(hits).addClass('dim');
      });
    }

    function runLayout() {
      if (!st.cy || st.mode !== 'graph') { return; }
      st.cy.layout(LAYOUTS[st.layout]).run();
      st.cy.fit(undefined, 24);
      st.laidOut = true;
    }

    function hideTip() { els.tip.hidden = true; els.tip.textContent = ''; }

    function showTip(target, pos) {
      var id = target.id();
      var text = '';
      var i = Number(id.slice(1));
      if (id.charAt(0) === 'n') {
        var n = st.view.byID[label(hl.obj(hl.arr(st.graph.nodes)[i]).id, 300)];
        if (n) { text = nodeTip(n, st.view); }
      } else {
        st.view.edges.forEach(function (e) { if (e.idx === i) { text = edgeTip(e); } });
      }
      if (!text) { hideTip(); return; }
      els.tip.textContent = text;
      els.tip.hidden = false;
      // CSSOM, not a style attribute: PageCSP allows it.
      var x = (pos && isFinite(pos.x) ? pos.x : 0) + (els.cy.offsetLeft || 0) + 12;
      var y = (pos && isFinite(pos.y) ? pos.y : 0) + (els.cy.offsetTop || 0) + 12;
      els.tip.style.left = Math.round(x) + 'px';
      els.tip.style.top = Math.round(y) + 'px';
    }

    function startGraph() {
      if (st.cy || !cytoscape) { return; }
      st.cy = cytoscape({ container: els.cy, elements: [], style: sheet(colours()), minZoom: 0.05, maxZoom: 4,
        boxSelectionEnabled: true });
      st.cy.on('tap', 'node, edge', function (ev) {
        st.selected = ev.target.id();
        highlight();
        showTip(ev.target, ev.renderedPosition);
      });
      st.cy.on('tap', function (ev) {
        if (ev.target === st.cy) { st.selected = null; highlight(); hideTip(); }
      });
      st.cy.on('mouseover', 'node, edge', function (ev) { showTip(ev.target, ev.renderedPosition); });
      st.cy.on('mouseout', 'node, edge', function () { if (!st.selected) { hideTip(); } });
      st.cy.on('pan zoom', function () { if (!st.selected) { hideTip(); } });
    }

    function drawGraph() {
      if (!st.cy) { return; }
      var cy = st.cy;
      cy.batch(function () {
        cy.elements().remove();
        cy.add(elements(st.view));
      });
      st.selected = null;
      hideTip();
      st.laidOut = false;
      highlight();
      runLayout();
    }

    function drawHeads() {
      hl.clear(els.heads);
      COLUMNS.forEach(function (c) {
        var th = hl.el('th');
        th.setAttribute('scope', 'col');
        if (st.sortKey === c[0]) { th.setAttribute('aria-sort', st.desc ? 'descending' : 'ascending'); }
        var b = hl.el('button', 'mapth', c[1]);
        b.setAttribute('type', 'button');
        b.addEventListener('click', function () { ctrl.sort(c[0]); });
        th.appendChild(b);
        els.heads.appendChild(th);
      });
    }

    function drawTable() {
      drawHeads();
      hl.clear(els.rows);
      var rs = rows(st.view, st.query, st.sortKey, st.desc);
      rs.forEach(function (r) {
        var tr = hl.el('tr');
        tr.setAttribute('data-edge', r.id);
        tr.appendChild(hl.el('td', null, r.from));
        tr.appendChild(hl.el('td', null, r.to));
        tr.appendChild(hl.el('td', 'mono', r.access));
        tr.appendChild(hl.el('td', 't2 small', r.rules));
        els.rows.appendChild(tr);
      });
      if (!rs.length) {
        var tr = hl.el('tr');
        var td = hl.el('td', 't3', st.view.edges.length ? 'No path matches the search.' : 'The policy gives no paths.');
        td.setAttribute('colspan', String(COLUMNS.length));
        tr.appendChild(td);
        els.rows.appendChild(tr);
      }
      return rs.length;
    }

    function drawCount() {
      var v = st.view;
      var text = v.nodes.length + ' selectors, ' + v.edges.length + ' paths';
      if (st.query.trim()) { text += ', ' + rows(v, st.query).length + ' match the search'; }
      els.count.textContent = text;
    }

    function drawKinds() {
      var present = {};
      hl.arr(st.graph.nodes).forEach(function (n) { present[kindOf(hl.obj(n).kind)] = true; });
      hl.clear(els.kinds);
      st.chips = {};
      KINDS.forEach(function (k) {
        if (!present[k]) { return; }
        var b = hl.el('button', 'chip' + (st.hidden[k] ? '' : ' on'));
        b.setAttribute('type', 'button');
        b.setAttribute('aria-pressed', st.hidden[k] ? 'false' : 'true');
        b.appendChild(hl.el('span', 'sw k-' + k));
        b.appendChild(hl.el('span', null, KIND_LABEL[k]));
        b.addEventListener('click', function () { ctrl.toggleKind(k); });
        st.chips[k] = b;
        els.kinds.appendChild(b);
      });
    }

    function drawLegend() {
      hl.clear(els.legend);
      KINDS.forEach(function (k) {
        var li = hl.el('li');
        li.appendChild(hl.el('span', 'sw k-' + k));
        li.appendChild(hl.el('span', null, KIND_LABEL[k]));
        els.legend.appendChild(li);
      });
      [['edge', 'Path: source can reach destination'], ['edge dashed', 'SSH path'], ['unused', 'Dashed border: no rule uses it']].forEach(function (x) {
        var li = hl.el('li');
        li.appendChild(hl.el('span', 'sw ' + x[0]));
        li.appendChild(hl.el('span', null, x[1]));
        els.legend.appendChild(li);
      });
    }

    function drawMode() {
      var graph = st.mode === 'graph' && !!st.cy;
      els.graphbox.hidden = !graph;
      els.tablebox.hidden = graph;
      els.showgraph.setAttribute('aria-pressed', graph ? 'true' : 'false');
      els.showtable.setAttribute('aria-pressed', graph ? 'false' : 'true');
      els.showgraph.className = graph ? 'on' : '';
      els.showtable.className = graph ? '' : 'on';
      els.showgraph.disabled = !st.cy;
      els.layout.disabled = !graph;
      els.fit.disabled = !graph;
      if (graph && st.cy) {
        st.cy.resize();
        if (!st.laidOut) { runLayout(); }
      }
    }

    function refresh(full) {
      st.view = visible(st.graph, st.hidden);
      if (full) { drawGraph(); }
      drawTable();
      drawCount();
    }

    ctrl.search = function (q) {
      st.query = String(q || '');
      st.selected = null;
      hideTip();
      highlight();
      drawTable();
      drawCount();
    };

    ctrl.setLayout = function (name) {
      if (!LAYOUTS[name]) { return; }
      st.layout = name;
      els.layout.value = name;
      hl.setQueryParam('layout', name === 'cose' ? '' : name);
      runLayout();
    };

    ctrl.toggleKind = function (k) {
      if (st.hidden[k]) { delete st.hidden[k]; } else { st.hidden[k] = true; }
      if (st.chips[k]) {
        st.chips[k].className = 'chip' + (st.hidden[k] ? '' : ' on');
        st.chips[k].setAttribute('aria-pressed', st.hidden[k] ? 'false' : 'true');
      }
      refresh(true);
    };

    ctrl.setView = function (mode) {
      st.mode = mode === 'table' || !st.cy ? 'table' : 'graph';
      drawMode();
    };

    ctrl.sort = function (key) {
      if (st.sortKey === key) { st.desc = !st.desc; } else { st.sortKey = key; st.desc = false; }
      drawTable();
    };

    ctrl.select = function (id) { st.selected = id || null; highlight(); };
    ctrl.fit = function () { if (st.cy) { st.cy.fit(undefined, 24); } };
    ctrl.restyle = function () { if (st.cy) { st.cy.style(sheet(colours())); } };
    ctrl.rows = function () { return rows(st.view, st.query, st.sortKey, st.desc); };
    ctrl.elements = function () { return elements(st.view); };

    ctrl.load = function () {
      setError('');
      return Promise.resolve().then(load).then(function (g) {
        st.graph = hl.obj(g);
        els.truncated.hidden = !st.graph.truncated;
        hl.clear(els.warnings);
        hl.arr(st.graph.warnings).slice(0, 50).forEach(function (w) { els.warnings.appendChild(hl.el('li', null, label(w, 300))); });
        els.sub.textContent = st.graph.updated_at ? 'Live policy, version ' + label(st.graph.updated_at, 80) + '.' : 'Live policy.';
        try { startGraph(); } catch (e) { st.cy = null; }
        if (!st.cy) { setError('The graph could not start. The table shows the same paths.'); st.mode = 'table'; }
        drawKinds();
        refresh(true);
        drawMode();
        els.root.setAttribute('data-map-state', 'ready');
      }, function (err) {
        setError('Map: ' + label(err && err.message, 200));
        els.root.setAttribute('data-map-state', 'error');
      });
    };

    els.search.addEventListener('input', function () { ctrl.search(els.search.value); });
    els.layout.addEventListener('change', function () { ctrl.setLayout(els.layout.value); });
    els.showgraph.addEventListener('click', function () { ctrl.setView('graph'); });
    els.showtable.addEventListener('click', function () { ctrl.setView('table'); });
    els.fit.addEventListener('click', function () { ctrl.fit(); });
    if (deps.onTheme) { deps.onTheme(function () { ctrl.restyle(); }); }

    els.layout.value = st.layout;
    drawLegend();
    return ctrl;
  }

  // onTheme calls fn after the theme changes: the shell's choice (data-hl-theme on the root) or the system setting.
  function onTheme(fn) {
    var later = function () { setTimeout(fn, 0); }; // after the style recalculation
    if (typeof MutationObserver === 'function') {
      new MutationObserver(later).observe(document.documentElement, { attributes: true, attributeFilter: ['data-hl-theme'] });
    }
    if (window.matchMedia) {
      var mq = window.matchMedia('(prefers-color-scheme: light)');
      if (mq.addEventListener) { mq.addEventListener('change', later); }
    }
  }

  function init() {
    var root = document.getElementById('policymap');
    if (!root) { return; }
    var $ = function (id) { return document.getElementById(id); };
    var ctrl = mount({
      root: root, sub: $('sub'), error: $('error'), truncated: $('truncated'), warnings: $('warnings'), search: $('search'),
      layout: $('layout'), showgraph: $('showgraph'), showtable: $('showtable'), fit: $('fit'), count: $('count'),
      kinds: $('kinds'), graphbox: $('graphbox'), cy: $('cy'), tip: $('tip'), legend: $('legend'), tablebox: $('tablebox'),
      heads: $('heads'), rows: $('rows')
    }, { onTheme: onTheme });
    ctrl.load();
  }

  hl.policymap = { mount: mount, visible: visible, elements: elements, rows: rows, access: access, refText: refText,
    sheet: sheet, readColours: readColours, nodeTip: nodeTip, edgeTip: edgeTip, init: init };
  init();
})();
