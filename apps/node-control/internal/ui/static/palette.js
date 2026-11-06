// headlamp console: the command palette (Cmd/Ctrl-K). Needs hl.js first. It lists the pages, the top-bar actions and,
// once open, the resources the modules offer for search. Nothing is requested before it opens, and only /_nav.json
// and /_search.json are ever requested. Everything that came from the network goes in through textContent only.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var NAV_URL = '/_nav.json';
  var SEARCH_URL = '/_search.json';
  var MAX_SHOWN = 50;
  var TITLE = 'Search or run a command';

  function arr(v) { return Array.isArray(v) ? v : []; }

  function localPath(p) {
    return typeof p === 'string' && p.charAt(0) === '/' && p.charAt(1) !== '/' && p.indexOf('\\') < 0;
  }

  // entries merges the three sources into one list: actions first, then pages, then resources. Anything without a
  // label or without a local path is dropped, so a hostile item can neither show up empty nor leave the console.
  function entries(actions, nav, search) {
    var out = [];
    function add(group, kind, label, path, icon) {
      label = hl.plain(label, 100);
      if (!label || !localPath(path)) { return; }
      out.push({ group: group, kind: kind, label: label, path: path, icon: typeof icon === 'string' ? icon : '' });
    }
    arr(actions).forEach(function (a) { if (a) { add(0, 'Action', a.label, a.path, a.icon); } });
    arr(nav).forEach(function (n) { if (n) { add(1, 'Page', n.label, n.path, n.icon); } });
    arr(search).forEach(function (s) { if (s) { add(2, hl.plain(s.kind, 40) || 'Item', s.label, s.path, ''); } });
    return out;
  }

  // filter: with no query, the actions and pages; otherwise every entry that holds all the words of the query,
  // best match first (label starts with it, a word of the label starts with it, label holds it, kind holds it).
  function filter(list, query) {
    var terms = String(query || '').toLowerCase().split(/\s+/).filter(function (t) { return t; });
    if (!terms.length) {
      return list.filter(function (e) { return e.group < 2; }).slice(0, MAX_SHOWN);
    }
    var scored = [];
    list.forEach(function (e, i) {
      var label = e.label.toLowerCase();
      var hay = label + ' ' + e.kind.toLowerCase();
      for (var k = 0; k < terms.length; k++) { if (hay.indexOf(terms[k]) < 0) { return; } }
      var first = terms[0];
      var score;
      if (label.indexOf(first) === 0) { score = 0; }
      else if (new RegExp('[\\s._/:-]' + first.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).test(label)) { score = 1; }
      else if (label.indexOf(first) >= 0) { score = 2; }
      else { score = 3; }
      scored.push({ e: e, score: score, i: i });
    });
    scored.sort(function (a, b) { return a.score - b.score || a.e.group - b.e.group || a.i - b.i; });
    return scored.slice(0, MAX_SHOWN).map(function (s) { return s.e; });
  }

  function clear(node) {
    while (node.firstChild) { node.removeChild(node.firstChild); }
  }

  function isEditable(t) {
    if (!t) { return false; }
    var tag = String(t.tagName || '').toUpperCase();
    return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || !!t.isContentEditable;
  }

  // create builds the dialog and returns its controller. opts: { host, api, navigate, actions }.
  function create(opts) {
    opts = opts || {};
    var api = opts.api || hl.api;
    var navigate = opts.navigate || function (p) { window.location.assign(p); };
    var actions = arr(opts.actions);

    var dialog = hl.el('dialog', 'palette');
    dialog.setAttribute('aria-label', TITLE);
    var head = hl.el('div', 'palhead');
    var ic = hl.icon('search');
    if (ic) { head.appendChild(ic); }
    var input = hl.el('input', 'palinput');
    input.setAttribute('type', 'text');
    input.setAttribute('role', 'combobox');
    input.setAttribute('aria-label', TITLE);
    input.setAttribute('aria-controls', 'palette-list');
    input.setAttribute('aria-expanded', 'true');
    input.setAttribute('aria-autocomplete', 'list');
    input.setAttribute('autocomplete', 'off');
    input.setAttribute('spellcheck', 'false');
    input.setAttribute('placeholder', TITLE);
    head.appendChild(input);
    var list = hl.el('ul', 'pallist');
    list.setAttribute('id', 'palette-list');
    list.setAttribute('role', 'listbox');
    var note = hl.el('p', 'palnote t2 small');
    note.setAttribute('aria-live', 'polite');
    var hint = hl.el('p', 'palhint t3 small', 'Up and down to move, Enter to open, Esc to close');
    dialog.appendChild(head);
    dialog.appendChild(list);
    dialog.appendChild(note);
    dialog.appendChild(hint);
    (opts.host || document.body).appendChild(dialog);

    var st = { nav: [], search: [], navFailed: false, searchFailed: false, loading: false, all: [], shown: [], active: 0, token: 0 };

    function setActive(i) {
      if (!st.shown.length) { st.active = 0; input.removeAttribute('aria-activedescendant'); return; }
      st.active = (i + st.shown.length) % st.shown.length;
      var kids = list.children || [];
      for (var k = 0; k < kids.length; k++) {
        var on = k === st.active;
        kids[k].className = on ? 'palitem on' : 'palitem';
        kids[k].setAttribute('aria-selected', on ? 'true' : 'false');
      }
      input.setAttribute('aria-activedescendant', 'palette-opt-' + st.active);
    }

    function go(e) {
      if (!e || !localPath(e.path)) { return; }
      ctl.close();
      navigate(e.path);
    }

    function render() {
      st.all = entries(actions, st.nav, st.search);
      st.shown = filter(st.all, input.value);
      clear(list);
      st.shown.forEach(function (e, i) {
        var li = hl.el('li', 'palitem');
        li.setAttribute('id', 'palette-opt-' + i);
        li.setAttribute('role', 'option');
        var inner = hl.el('span', 'row nowrap');
        var eic = e.icon ? hl.icon(e.icon) : null;
        if (eic) { inner.appendChild(eic); }
        inner.appendChild(hl.el('span', null, e.label));
        li.appendChild(inner);
        li.appendChild(hl.el('span', 'palkind t3 small', e.kind));
        li.addEventListener('mousemove', function () { if (st.active !== i) { setActive(i); } });
        li.addEventListener('click', function () { go(e); });
        list.appendChild(li);
      });
      var msgs = [];
      if (st.loading) { msgs.push('Loading.'); }
      if (st.navFailed) { msgs.push('The pages could not be loaded.'); }
      if (st.searchFailed) { msgs.push('Search results are not available right now.'); }
      if (!st.shown.length && !st.loading) { msgs.push(input.value.trim() ? 'Nothing found.' : 'Nothing to show.'); }
      note.textContent = msgs.join(' ');
      setActive(Math.min(st.active, Math.max(0, st.shown.length - 1)));
    }

    input.addEventListener('input', function () { st.active = 0; render(); });
    input.addEventListener('keydown', function (ev) {
      var key = ev && ev.key;
      // Keys that belong to an input method (Enter picking a candidate, Esc dropping it) are not ours; keyCode 229
      // is what some browsers report for the Enter that ends a composition.
      if (ev && (ev.isComposing || ev.keyCode === 229)) { return; }
      if (key === 'ArrowDown') { ev.preventDefault(); setActive(st.active + 1); }
      else if (key === 'ArrowUp') { ev.preventDefault(); setActive(st.active - 1); }
      else if (key === 'Enter') { ev.preventDefault(); go(st.shown[st.active]); }
      else if (key === 'Escape') { ev.preventDefault(); ctl.close(); }
    });
    // A click on the backdrop lands on the dialog itself.
    dialog.addEventListener('click', function (ev) { if (ev && ev.target === dialog) { ctl.close(); } });

    function load() {
      var token = ++st.token;
      st.nav = []; st.search = []; st.navFailed = false; st.searchFailed = false; st.loading = true;
      var pending = 2;
      function done() {
        pending -= 1;
        if (token !== st.token || pending !== 0) { return; }
        st.loading = false; // first, so a list that cannot be drawn never stays "Loading."
        try { render(); } catch (e) { /* the list stays as it was */ }
      }
      api('GET', NAV_URL).then(function (data) {
        if (token === st.token) { st.nav = arr(data); render(); }
      }, function () { if (token === st.token) { st.navFailed = true; } }).then(done, done);
      api('GET', SEARCH_URL).then(function (data) {
        if (token === st.token) { st.search = arr(data && data.items); render(); }
      }, function () { if (token === st.token) { st.searchFailed = true; } }).then(done, done);
    }

    var ctl = {
      state: st,
      dialog: dialog,
      input: input,
      list: list,
      note: note,
      isOpen: function () { return !!dialog.open; },
      owns: function (t) { return t === input; },
      open: function () {
        if (dialog.open) { return; }
        input.value = '';
        st.active = 0;
        dialog.showModal();
        load();
        render();
        input.focus();
      },
      close: function () {
        st.token += 1; // an answer that arrives later is dropped
        if (dialog.open) { dialog.close(); }
      },
      toggle: function () { if (dialog.open) { ctl.close(); } else { ctl.open(); } },
      render: render,
    };
    return ctl;
  }

  // onShortcut: Cmd/Ctrl-K opens or closes the palette. While typing in a field of the page it is left alone; the
  // palette's own field is the exception.
  function onShortcut(ctl, ev) {
    if (!ev || !(ev.metaKey || ev.ctrlKey) || ev.altKey || ev.shiftKey) { return false; }
    if (String(ev.key).toLowerCase() !== 'k') { return false; }
    if (isEditable(ev.target) && !ctl.owns(ev.target)) { return false; }
    ev.preventDefault();
    ctl.toggle();
    return true;
  }

  function init() {
    var btn = document.querySelector('[data-palette]');
    if (!btn) { return; }
    var actions = [];
    try { actions = arr(JSON.parse(btn.getAttribute('data-actions') || '[]')); } catch (e) { actions = []; }
    var ctl = create({ host: btn.closest ? btn.closest('.hl') : null, actions: actions });
    btn.addEventListener('click', function () { ctl.toggle(); });
    document.addEventListener('keydown', function (ev) { onShortcut(ctl, ev); });
    var keys = btn.querySelector('[data-palette-keys]');
    var platform = (typeof navigator !== 'undefined' && (navigator.platform || navigator.userAgent)) || '';
    if (keys && platform && !/Mac|iPhone|iPad/.test(platform)) { keys.textContent = 'Ctrl K'; }
  }

  hl.palette = { entries: entries, filter: filter, create: create, onShortcut: onShortcut, init: init };
  init();
})();
