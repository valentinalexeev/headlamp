// headlamp console: the Devices page and its enrolment panel. Needs /_ui/hl.js first (the shell loads it) and,
// for the QR code, /enroll/qrcode.js. Everything that came from the network goes in through textContent only;
// markup is never built from strings.
//
// Secrets: the one-time link and the subscription URL exist only in the enrolment panel, right after they are
// made. They are never put in a URL, an anchor, storage or a table row, and a rebuilt table cannot show them. The
// panel drops them when it closes and when it moves on to the next device.
(function () {
  'use strict';
  var hl = window.hl;
  if (!hl) { return; }

  var POLL_MS = 15000;
  var WAIT_MS = 4000;
  var DAY = 86400000;
  var EXPIRING_MS = 7 * DAY;
  var SEEN_MS = DAY;
  var QUIET_DAYS = 7; // a device that had traffic and then none for longer than this needs help (#439)
  var QUIET_MS = QUIET_DAYS * DAY;
  var DEFAULT_DAYS = 90;
  var LINK_TTL = 86400;
  var CLIENTS = ['karing', 'sing-box', 'clash', 'shadowrocket', 'stash', 'happ', 'throne']; // the catalog of clients.go
  var CLIENT_KEY = 'enrollClient';
  // #439: "Needs help" is the default, so the page opens on the devices to troubleshoot. The filters of #440 stay.
  var FILTERS = [['help', 'Needs help'], ['active', 'Active'], ['expiring', 'Expiring soon'], ['never', 'Not set up'],
    ['revoked', 'Revoked'], ['all', 'All']];
  var DEFAULT_FILTER = 'help';
  var COLUMNS = 6;
  var SYNC_REASON = {
    unreachable: 'Agent unreachable', unauthorized: 'Agent refused the credentials',
    rejected: 'Agent rejected the credentials', conflict: 'Agent reports a conflict', error: 'Sync ended in an error'
  };
  var ICON = { link: 'link', rotate: 'key', extend: 'extend', revoke: 'ban', delete: 'trash', push: 'activity', support: 'copy' };

  function pad(n) { return n < 10 ? '0' + n : String(n); }
  function ymd(d) { return d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()); }

  // endOfDay: the last second of a YYYY-MM-DD date in the browser's time zone, as unix seconds. NaN for junk.
  function endOfDay(text) {
    var m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(String(text || ''));
    if (!m) { return NaN; }
    var d = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]), 23, 59, 59, 0);
    return isNaN(d.getTime()) ? NaN : Math.floor(d.getTime() / 1000);
  }

  function fmtDate(sec) {
    var d = new Date(sec * 1000);
    if (isNaN(d.getTime())) { return ''; }
    var sameYear = d.getFullYear() === new Date().getFullYear();
    return d.toLocaleDateString(undefined, sameYear ? { month: 'short', day: 'numeric' } : { month: 'short', day: 'numeric', year: 'numeric' });
  }

  // relExpiry (#440): an expiry as days from now ("in 84 days", "today", "3 days ago"); the date goes in the hover
  // title. Counted like the state word: a part of a day ahead is a whole day.
  function relExpiry(sec, nowMs) {
    var ms = sec * 1000;
    if (!(sec > 0) || isNaN(ms)) { return ''; }
    if (ms >= nowMs) {
      var ahead = Math.ceil((ms - nowMs) / DAY);
      return ms - nowMs < DAY ? 'today' : (ahead === 1 ? 'in 1 day' : 'in ' + ahead + ' days');
    }
    var ago = Math.floor((nowMs - ms) / DAY);
    return ago === 0 ? 'today' : (ago === 1 ? '1 day ago' : ago + ' days ago');
  }

  // fullDate: the date with its year, for the hover title of an expiry.
  function fullDate(sec) {
    var d = new Date(sec * 1000);
    return isNaN(d.getTime()) ? '' : d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
  }

  // emailKey: the lower-case form of an owner that is an email address, '' for any other owner.
  function emailKey(s) {
    var v = String(s || '').trim().toLowerCase();
    return /^[^\s@]+@[^\s@]+$/.test(v) ? v : '';
  }

  // sameNames (#440): the lower-case names that two or more of the devices share, with their counts.
  function sameNames(devices) {
    var n = {};
    devices.forEach(function (d) { var k = hl.plain(d.name, 100).trim().toLowerCase(); if (k) { n[k] = (n[k] || 0) + 1; } });
    Object.keys(n).forEach(function (k) { if (n[k] < 2) { delete n[k]; } });
    return n;
  }

  // setUp: an app has the device's profile (#436). A spent one-time link, a profile download (also for a device made
  // on the self-service page, which has no link) or traffic each show it; older data has only the first and the last.
  function setUp(d) { return d.enrol_count > 0 || d.profile_at > 0 || d.last_active > 0; }

  // quietDays: whole days since the device's last traffic, when that is more than QUIET_DAYS ago; 0 otherwise.
  function quietDays(d, nowMs) {
    if (!(d.last_active > 0)) { return 0; }
    var gap = nowMs - d.last_active * 1000;
    return gap > QUIET_MS ? Math.floor(gap / DAY) : 0;
  }

  // stateOf: where a device stands now. level and word are what the row shows; kind, expiring, neverUsed and help
  // are what the filters and the actions go by. A device that is both near its expiry and unused reads as expiring.
  // help (#439): set up but never connected, or quiet for more than QUIET_DAYS after it had traffic.
  function stateOf(d, nowMs) {
    var used = setUp(d);
    var exp = d.expires_at * 1000;
    if (d.revoked) { return { kind: 'revoked', level: 'off', word: 'Revoked', expiring: false, neverUsed: false, help: false }; }
    var quiet = quietDays(d, nowMs);
    var help = used && (!(d.last_active > 0) || quiet > 0);
    if (exp < nowMs) { return { kind: 'expired', level: 'warn', word: 'Expired', expiring: false, neverUsed: false, help: help }; }
    var left = exp - nowMs;
    var expiring = left <= EXPIRING_MS;
    var out = { kind: 'active', level: 'ok', word: 'Active', expiring: expiring, neverUsed: !used, help: help };
    if (expiring) {
      var days = Math.ceil(left / DAY);
      out.level = 'warn';
      out.word = left < DAY ? 'Expires today' : (days === 1 ? 'Expires in 1 day' : 'Expires in ' + days + ' days');
    } else if (!used) {
      out.level = 'warn';
      out.word = 'Not set up';
    } else if (!(d.last_active > 0)) {
      out.word = 'Set up, no traffic yet';
    } else if (quiet > 0) {
      out.level = 'warn';
      out.word = 'No traffic for ' + quiet + ' days';
    }
    return out;
  }

  // actionsFor: only what can do something for the device's state.
  //   revoked:                  Delete (nothing else works on it)
  //   expired:                  Extend, Revoke (a link would not work: the spend refuses an expired device)
  //   active, not set up:       New link (made by an admin only), Extend, Revoke
  //   active, set up:           Rotate credential (a profile may have leaked), Extend, Revoke (a link is not needed)
  // A self-enrolled device ("oidc:" owner) gets no "New link": its person adds the app from the self-service page.
  function actionsFor(d, nowMs) {
    var s = stateOf(d, nowMs);
    var a = function (key, label, danger) { return { key: key, label: label, icon: ICON[key], danger: !!danger }; };
    if (s.kind === 'revoked') { return [a('delete', 'Delete', true)]; }
    var out = [];
    var self = String(d.owner || '').indexOf('oidc:') === 0;
    if (s.kind === 'active' && !setUp(d) && !self) { out.push(a('link', 'New link')); }
    if (s.kind === 'active' && setUp(d)) { out.push(a('rotate', 'Rotate credential')); }
    out.push(a('extend', 'Extend'));
    out.push(a('revoke', 'Revoke', true));
    return out;
  }

  // scrub takes out of a text anything that can be a credential: a URL, a UUID, and a long token (hex, or mixed case
  // with digits, as a link token or a subscription secret is). It is the last step of the support info, so a device
  // name typed with such a value in it cannot carry it out. A plain long name such as "office-laptop-of-the-desk"
  // stays.
  function scrub(text) {
    return String(text)
      .replace(/[a-z][a-z0-9+.-]*:\/\/\S*/gi, '[removed]')
      .replace(/[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}/gi, '[removed]')
      .replace(/[A-Za-z0-9_.-]{16,}/g, function (m) {
        var token = /^[0-9a-f]+$/i.test(m) || (/[0-9]/.test(m) && /[A-Z]/.test(m) && /[a-z]/.test(m));
        return token ? '[removed]' : m;
      });
  }

  // supportInfo (#441): a summary of one device to paste in a chat with its person. It has the state, the bridges and
  // the last traffic, and no secret: no link, subscription URL, UUID, owner identifier, bridge host or device id.
  function supportInfo(d, ingresses, nowMs) {
    var s = stateOf(d, nowMs);
    var when = function (sec) { return sec > 0 ? hl.relTime(new Date(sec * 1000).toISOString(), nowMs) + ' (' + ymd(new Date(sec * 1000)) + ')' : 'never'; };
    var tr = d.traffic && typeof d.traffic === 'object' ? d.traffic : null;
    var lines = [
      'Device: ' + hl.plain(scrub(d.name), 100), // scrubbed before the cut: a cut token would no longer look like one
      'App: ' + (hl.plain(d.client, 40) || 'unknown'),
      'State: ' + s.word,
      'Set up: ' + (setUp(d) ? 'yes' : 'no') + (d.profile_last > 0 ? ', profile last downloaded ' + when(d.profile_last) : ''),
      'Last traffic: ' + when(d.last_active),
      'Traffic: ' + (tr ? 'up ' + (hl.fmtBytes(Number(tr.up) || 0) || '0 B') + ', down ' + (hl.fmtBytes(Number(tr.down) || 0) || '0 B') : 'not reported'),
      'Expires: ' + (s.kind === 'revoked' ? 'n/a' : ymd(new Date(d.expires_at * 1000)))
    ];
    var bridges = hl.arr(ingresses).filter(function (r) { return r && typeof r.name === 'string'; });
    if (!bridges.length) { lines.push('Bridges: none reported'); }
    bridges.forEach(function (r) {
      var sync = r.sync || {};
      var word = sync.pending ? 'update pending' + (SYNC_REASON[sync.state] ? ' (' + SYNC_REASON[sync.state].toLowerCase() + ')' : '') : 'up to date';
      if (r.online === false) { word += ', offline'; }
      lines.push('Bridge ' + hl.plain(r.name, 63) + ': ' + word);
    });
    return scrub(lines.join('\n'));
  }

  function matches(filter, s) {
    switch (filter) {
      case 'help': return s.help;
      case 'expiring': return s.expiring;
      case 'never': return s.neverUsed;
      case 'revoked': return s.kind === 'revoked';
      case 'all': return true;
      default: return s.kind !== 'revoked';
    }
  }

  // plainFilter: a known filter, else the default.
  function plainFilter(f) {
    for (var i = 0; i < FILTERS.length; i++) { if (FILTERS[i][0] === f) { return f; } }
    return DEFAULT_FILTER;
  }

  // lastClient: the client used for the last device made in this browser. A storage that is missing or throws
  // (private windows, blocked site data) gives the first client; a stored value that is not a client is ignored.
  function lastClient() {
    try {
      var v = window.localStorage.getItem(CLIENT_KEY);
      if (CLIENTS.indexOf(v) >= 0) { return v; }
    } catch (e) { /* no storage: use the default */ }
    return CLIENTS[0];
  }
  function rememberClient(c) {
    try { window.localStorage.setItem(CLIENT_KEY, c); } catch (e) { /* nothing to remember with */ }
  }

  // defaultExpiry: the date offered for a new device, 90 days out, as YYYY-MM-DD.
  function defaultExpiry(nowMs) {
    var d = new Date(nowMs);
    d.setDate(d.getDate() + DEFAULT_DAYS);
    return ymd(d);
  }

  // extendTo: the new expiry (unix seconds) for an extension by days: counted from the later of the current
  // expiry and now, to the end of that day.
  function extendTo(d, days, nowMs) {
    var base = Math.max(d.expires_at * 1000, nowMs);
    var t = new Date(base);
    t.setDate(t.getDate() + days);
    t.setHours(23, 59, 59, 0);
    return Math.floor(t.getTime() / 1000);
  }

  function keyed(el, key) { el.setAttribute('data-fk', key); return el; }

  var OIDC = 'oidc:';
  var INVITED = 'i:';
  var INVITE_WORD = { pending: ['ok', 'Waiting'], used: ['off', 'Used'], expired: ['warn', 'Expired'], revoked: ['off', 'Withdrawn'] };

  // groupTree (#287) puts each device and invitation under its person or owner. people and invites are null when
  // self-enrolment is off. A self-enrolled person (owner "oidc:<subject>") is one group, with the devices of that
  // owner and the invitations it used or that name its email. An operator-made owner is one group of its devices.
  // One person is one group (#440): an operator-made owner that is the email of a person joins that person's group,
  // and an invitation for the email of an operator-made owner goes under that owner.
  // A waiting invitation for an email with no person or owner yet goes to the group "Invited, not signed in yet".
  // Used, expired and withdrawn invitations show only with the filter "all", waiting ones under "active" and "all":
  // the other filters are about devices. q is a search text (lower case): a group shows when its label, owner, the
  // person's email, or one of its device names, owner emails or invitation emails contains it.
  // Each group: { key, kind: person|owner|invited, label, person, devices: [device that matches the filter],
  // every: [every device of the group], invites: [invite] }.
  function groupTree(devices, people, invites, filter, nowMs, q) {
    var groups = {};
    var byEmail = {};
    function group(key, kind, label) {
      if (!groups[key]) { groups[key] = { key: key, kind: kind, label: label, person: null, devices: [], every: [], invites: [] }; }
      return groups[key];
    }
    hl.arr(people).forEach(function (p) {
      if (!p || typeof p.owner !== 'string') { return; }
      var g = group('p:' + p.owner, 'person', hl.plain(p.email, 120) || hl.plain(p.owner, 120));
      g.person = p;
      if (p.email) { byEmail[String(p.email).toLowerCase()] = g; }
    });
    var list = hl.arr(devices).filter(function (d) { return d; });
    var ownerOf = function (d) { return typeof d.owner === 'string' ? d.owner : ''; };
    // Self-enrolled devices first, so that every person is known before an operator-made owner looks for one.
    list.forEach(function (d) {
      var owner = ownerOf(d);
      if (owner.indexOf(OIDC) !== 0) { return; }
      var g = group('p:' + owner, 'person', hl.plain(d.owner_email, 120) || hl.plain(owner, 120));
      if (d.owner_email && !byEmail[String(d.owner_email).toLowerCase()]) { byEmail[String(d.owner_email).toLowerCase()] = g; }
    });
    list.forEach(function (d) {
      var owner = ownerOf(d);
      var g;
      if (owner.indexOf(OIDC) === 0) {
        g = groups['p:' + owner];
      } else {
        var mail = emailKey(owner);
        g = (mail && byEmail[mail]) || group('o:' + owner, 'owner', hl.plain(owner, 120) || 'No owner');
        if (mail && !byEmail[mail]) { byEmail[mail] = g; }
      }
      g.every.push(d);
      if (matches(filter, stateOf(d, nowMs))) { g.devices.push(d); }
    });
    hl.arr(invites).forEach(function (inv) {
      if (!inv) { return; }
      if (filter !== 'all' && !(filter === 'active' && inv.state === 'pending')) { return; }
      var email = String(inv.email || '').toLowerCase();
      var g = (typeof inv.used_by === 'string' && inv.used_by && groups['p:' + inv.used_by]) || byEmail[email];
      if (!g) { g = group(INVITED, 'invited', 'Invited, not signed in yet'); }
      g.invites.push(inv);
    });
    var rank = { person: 0, owner: 0, invited: 1 };
    return Object.keys(groups).map(function (k) { return groups[k]; }).filter(function (g) {
      // A group with nothing that matches the filter is left out, except a person under "active" and "all": that row
      // is where the person is locked.
      if (!g.devices.length && !g.invites.length && !(g.person && (filter === 'active' || filter === 'all'))) {
        return false;
      }
      if (!q) { return true; }
      var hay = [g.label, g.person ? g.person.owner + ' ' + (g.person.email || '') : ''].concat(g.devices.map(function (d) {
        return d.name + ' ' + d.owner + ' ' + (d.owner_email || '');
      }),
        g.invites.map(function (i) { return i.email; })).join(' ').toLowerCase();
      return hay.indexOf(q) >= 0;
    }).sort(function (a, b) {
      return (rank[a.kind] - rank[b.kind]) || (a.label.toLowerCase() < b.label.toLowerCase() ? -1 : a.label.toLowerCase() > b.label.toLowerCase() ? 1 : 0);
    });
  }

  // groupCounts: the group line (#439), for example "2 devices · 1 connected today · 1 not set up". It counts every
  // device of the group that is not revoked, whatever the filter, so that a closed group hides nothing that needs
  // attention. With no device connected today it says when one last connected.
  function groupCounts(g, nowMs) {
    var parts = [];
    var live = hl.arr(g.every || g.devices).filter(function (d) { return !d.revoked; });
    var n = live.length;
    var today = 0, never = 0, help = 0, expiring = 0, expired = 0, last = 0;
    live.forEach(function (d) {
      var s = stateOf(d, nowMs);
      var at = Number(d.last_active) || 0;
      last = Math.max(last, at);
      if (at > 0 && at * 1000 >= nowMs - SEEN_MS) { today++; }
      if (s.neverUsed) { never++; }
      if (s.help) { help++; }
      if (s.expiring) { expiring++; }
      if (s.kind === 'expired') { expired++; }
    });
    if (g.kind !== 'invited') { parts.push(n === 1 ? '1 device' : n + ' devices'); }
    if (today) { parts.push(today + ' connected today'); }
    if (never) { parts.push(never + ' not set up'); }
    if (help) { parts.push(help + (help === 1 ? ' needs help' : ' need help')); }
    if (expiring) { parts.push(expiring + ' expiring soon'); }
    if (expired) { parts.push(expired + ' expired'); }
    var waiting = g.invites.filter(function (i) { return i.state === 'pending'; }).length;
    if (waiting) { parts.push(waiting === 1 ? '1 invitation waiting' : waiting + ' invitations waiting'); }
    // The sign-in to the self-service page is not a connection: the group row has it only in its hover title.
    if (n && !today) { parts.push('last connected ' + (last ? hl.relTime(new Date(last * 1000).toISOString(), nowMs) : 'never')); }
    return parts.join(' · ');
  }

  // personState: the state word of a self-enrolled person.
  function personState(p) {
    if (p.locked) { return { level: 'crit', word: 'Locked' + (p.lock_reason ? ': ' + hl.plain(p.lock_reason, 80) : '') }; }
    if (p.check_failed) { return { level: 'warn', word: 'Identity provider unreachable at the last check' }; }
    return { level: 'ok', word: 'Active' };
  }

  // focusKey names the focused control inside box by its data-fk, so a rebuild can give focus back.

  // qrOf draws the link as an SVG inside a white box with a quiet zone, in the browser: the link never goes to
  // another server. Without the library, or for a link too long to encode, the code is just absent.
  function qrOf(text) {
    try {
      if (typeof qrcode !== 'function') { return null; }
      var q = qrcode(0, 'M');
      q.addData(text);
      q.make();
      var n = q.getModuleCount(), m = 4, NS = 'http://www.w3.org/2000/svg';
      var svg = document.createElementNS(NS, 'svg');
      svg.setAttribute('viewBox', '0 0 ' + (n + 2 * m) + ' ' + (n + 2 * m));
      svg.setAttribute('width', '100%');
      svg.setAttribute('height', '100%');
      svg.setAttribute('role', 'img');
      svg.setAttribute('aria-label', 'QR code of the enrolment link');
      svg.setAttribute('shape-rendering', 'crispEdges');
      var bg = document.createElementNS(NS, 'rect');
      bg.setAttribute('width', String(n + 2 * m));
      bg.setAttribute('height', String(n + 2 * m));
      bg.setAttribute('fill', '#fff');
      var path = document.createElementNS(NS, 'path');
      var d = '';
      for (var r = 0; r < n; r++) {
        for (var c = 0; c < n; c++) { if (q.isDark(r, c)) { d += 'M' + (c + m) + ',' + (r + m) + 'h1v1h-1z'; } }
      }
      path.setAttribute('d', d);
      path.setAttribute('fill', '#000');
      svg.appendChild(bg);
      svg.appendChild(path);
      var box = hl.el('div', 'qr');
      box.appendChild(svg);
      return box;
    } catch (e) {
      return null;
    }
  }

  // copyText puts text on the clipboard and says whether it did; selecting it by hand is the fallback.
  function copyText(text) {
    try {
      if (typeof navigator !== 'undefined' && navigator.clipboard && navigator.clipboard.writeText) {
        return navigator.clipboard.writeText(text).then(function () { return true; }, function () { return false; });
      }
    } catch (e) { /* fall through */ }
    return Promise.resolve(false);
  }
  function selectAll(el) {
    try {
      var sel = window.getSelection && window.getSelection();
      if (sel && sel.selectAllChildren) { sel.selectAllChildren(el); }
    } catch (e) { /* the text stays selectable by hand */ }
  }

  // mount wires the page's elements to the data. deps (all optional): now() for the clock, poll(fn, ms) for the
  // wait for first use, copy(text) for the clipboard, later(fn, ms) for the short "Copied" label.
  function mount(els, deps) {
    deps = deps || {};
    var now = deps.now || function () { return Date.now(); };
    var poll = deps.poll || function (fn, ms) { return hl.poll(fn, ms); };
    var copy = deps.copy || copyText;
    var later = deps.later || function (fn, ms) { setTimeout(fn, ms); };
    var st = {
      devices: [], sync: null, ingresses: [], partial: false, bridges: [], loaded: false, failed: false,
      filter: plainFilter(hl.queryParam('filter')), busy: {}, open: null, tableSig: null, chipSig: null,
      chips: {}, drawer: null,
      // #287: people and invitations (null while self-enrolment is off or not read yet), the search text, and the
      // open groups (kept in the URL, so a reload and the 15 s refresh keep them)
      people: null, invites: null, selfOn: false, query: '',
      openGroups: String(hl.queryParam('open') || '').split(',').filter(function (k) { return k && k.length <= 400; }).slice(0, 200)
    };
    var ctrl = { loadDevices: null };

    function setError(text) {
      els.error.textContent = text;
      els.error.hidden = !text;
    }
    function setNotice(text) {
      els.notice.textContent = text;
      els.notice.hidden = !text;
    }

    function find(id) {
      for (var i = 0; i < st.devices.length; i++) { if (st.devices[i].id === id) { return st.devices[i]; } }
      return null;
    }

    // ----- the sync note

    function names(list) {
      if (list.length < 2) { return list.join(''); }
      return list.slice(0, -1).join(', ') + ' and ' + list[list.length - 1];
    }

    function renderSync() {
      var box = els.syncnote;
      hl.clear(box);
      var s = st.sync;
      if (!s || !s.pending) { box.hidden = true; return; }
      var behind = hl.arr(st.ingresses).filter(function (r) { return r && r.sync && r.sync.pending && typeof r.name === 'string'; });
      var list = behind.map(function (r) { return hl.plain(r.name, 63); });
      var title, where;
      if (list.length === 0) {
        title = 'The bridge host is out of sync';
        where = 'the bridge host';
      } else {
        title = names(list) + (list.length === 1 ? ' is' : ' are') + ' out of sync';
        where = names(list);
      }
      box.appendChild(hl.status('crit', title));
      var why = [];
      (list.length ? behind : [{ name: '', sync: s }]).forEach(function (r) {
        var reason = SYNC_REASON[r.sync.state] || '';
        var since = typeof r.sync.since === 'number' ? hl.relTime(new Date(r.sync.since * 1000).toISOString(), now()) : '';
        var bits = [reason, since ? 'since ' + since : ''].filter(Boolean).join(', ');
        if (bits) { why.push(list.length > 1 ? hl.plain(r.name, 63) + ': ' + bits : bits); }
      });
      var p = hl.el('p', 't2 small', 'Revoking or extending a device here will not take effect on ' + where + ' until it catches up.' + (why.length ? ' ' + why.join('; ') + '.' : '') + ' Retrying every 5 minutes.');
      box.appendChild(p);
      var links = hl.el('p', 'small');
      (list.length ? list : ['']).forEach(function (n, i) {
        if (i > 0) { links.appendChild(document.createTextNode(' ')); }
        var a = hl.el('a', null, list.length > 1 ? 'Open ' + n : 'Open bridge');
        a.setAttribute('href', n ? '/nodes?sel=' + encodeURIComponent(n) : '/nodes');
        links.appendChild(a);
      });
      box.appendChild(links);
      var again = hl.el('div', 'rowbtns start');
      var push = keyed(hl.button('Push now', ICON.push, 'sm', pushNow), 'sync:push');
      push.disabled = !!st.busy.push;
      again.appendChild(push);
      box.appendChild(again);
      box.hidden = false;
    }

    // ----- the bridge strip (#438): one line for each bridge, so a user problem that is a bridge problem shows here

    function bridgeLine(b, t) {
      var name = hl.plain(b.name, 63);
      var sync = hl.obj(b.sync);
      var when = function (sec) { return hl.relTime(new Date(sec * 1000).toISOString(), t); };
      var level = 'ok';
      var bits = [];
      if (sync.pending) {
        level = 'crit';
        var reason = SYNC_REASON[sync.state] || 'sync pending';
        bits.push('Pending: ' + reason.charAt(0).toLowerCase() + reason.slice(1) + (typeof sync.since === 'number' ? ' since ' + when(sync.since) : ''));
      } else {
        bits.push(sync.state === 'unknown' ? 'Sync not done yet' : 'In sync');
      }
      var last = typeof b.last_answer === 'number' && b.last_answer > 0 ? when(b.last_answer) : '';
      if (b.answered) {
        bits.push('agent answered ' + (last || 'just now'));
      } else {
        level = 'crit';
        bits.push('agent did not answer' + (last ? ', last answer ' + last : ', no answer since the start'));
      }
      var eg = b.egress && typeof b.egress === 'object' ? b.egress : null;
      if (!eg) {
        bits.push('egress self-test not reported');
      } else if (eg.ok) {
        bits.push('egress self-test OK');
      } else {
        level = 'crit';
        bits.push('egress self-test fails' + (eg.error ? ': ' + hl.plain(eg.error, 200) : ''));
      }
      var pc = b.pathcheck && typeof b.pathcheck === 'object' ? b.pathcheck : null;
      var cls = pc ? hl.plain(pc.class, 32) : '';
      if (!cls) {
        bits.push('path check off');
      } else if (cls === 'ok') {
        bits.push('path check ok');
      } else {
        if (level === 'ok') { level = 'warn'; }
        bits.push('path check ' + cls);
      }
      var p = hl.el('p', 'small');
      p.appendChild(hl.status(level, name));
      p.appendChild(document.createTextNode(' ' + bits.join(', ') + '. '));
      var a = hl.el('a', null, 'Open bridge');
      a.setAttribute('href', '/nodes?sel=' + encodeURIComponent(name));
      p.appendChild(a);
      return p;
    }

    function renderBridges() {
      var box = els.bridges;
      if (!box) { return; }
      hl.clear(box);
      var list = st.bridges.filter(function (b) { return b && typeof b.name === 'string' && b.name; });
      if (!list.length) { box.hidden = true; return; }
      var t = now();
      box.appendChild(hl.el('h2', null, 'Bridges'));
      list.forEach(function (b) { box.appendChild(bridgeLine(b, t)); });
      box.hidden = false;
    }

    // ----- the table

    // ownerLabel: the email of a self-enrolled owner instead of its "oidc:<subject>" identifier.
    function ownerLabel(d) { return hl.plain(d.owner_email, 100) || hl.plain(d.owner, 100); }

    function rowModel(d) {
      var t = now();
      var s = stateOf(d, t);
      var total = d.traffic && typeof d.traffic === 'object' ? (Number(d.traffic.up) || 0) + (Number(d.traffic.down) || 0) : null;
      return {
        id: d.id, name: hl.plain(d.name, 100), owner: ownerLabel(d), ownerId: hl.plain(d.owner, 100), 
        level: s.level, word: s.word, kind: s.kind,
        seen: d.last_active > 0 ? hl.relTime(new Date(d.last_active * 1000).toISOString(), t) : 'Never',
        traffic: total === null ? '-' : (hl.fmtBytes(total) || '-'),
        trafficTitle: d.traffic ? 'Up ' + (hl.fmtBytes(Number(d.traffic.up) || 0) || '0 B') + ', down ' + (hl.fmtBytes(Number(d.traffic.down) || 0) || '0 B') : '',
        exp: s.kind === 'revoked' ? 'n/a' : relExpiry(d.expires_at, t),
        expTitle: s.kind === 'revoked' ? '' : fullDate(d.expires_at),
        actions: actionsFor(d, t).concat(rowExtras()), expiresAt: d.expires_at
      };
    }

    // rowExtras (#441): the fix-it actions of every row. Push now is there while a bridge has an update pending: the
    // push goes to all bridges, so it helps whichever device the operator is looking at.
    function rowExtras() {
      var out = [];
      if (st.sync && st.sync.pending) { out.push({ key: 'push', label: 'Push now', icon: ICON.push, danger: false }); }
      out.push({ key: 'support', label: 'Copy support info', icon: ICON.support, danger: false });
      return out;
    }

    function shownRows() {
      var t = now();
      var rank = { crit: 0, warn: 1, ok: 2, off: 3 };
      return st.devices.filter(function (d) { return matches(st.filter, stateOf(d, t)); }).map(rowModel).sort(function (a, b) {
        return (rank[a.level] - rank[b.level]) || (a.expiresAt - b.expiresAt) || (a.name < b.name ? -1 : a.name > b.name ? 1 : 0);
      });
    }

    function closeOpen() {
      st.open = null;
      render();
    }

    function runAction(d, key, fn) {
      if (st.busy[d.id]) { return; }
      st.busy[d.id] = true;
      setError('');
      setNotice('');
      st.open = null;
      render();
      fn().then(function (res) {
        delete st.busy[d.id];
        if (res && res.notice) { setNotice(res.notice); }
        if (res && res.sync !== undefined) { st.sync = res.sync; st.ingresses = hl.arr(res.ingresses); renderSync(); }
        render();
        if (ctrl.onAction) { ctrl.onAction(); }
      }, function (err) {
        delete st.busy[d.id];
        setError(key + ' failed for ' + hl.plain(d.name, 100) + ': ' + hl.plain(err && err.message, 200));
        render();
        if (ctrl.onAction) { ctrl.onAction(); }
      });
    }

    function doRevoke(d) {
      runAction(d, 'Revoke', function () {
        return hl.api('POST', '/enroll/devices/' + encodeURIComponent(d.id) + '/revoke', {}).then(function (j) {
          return { notice: 'Revoked ' + hl.plain(d.name, 100) + '.', sync: hl.obj(j).sync || null, ingresses: hl.obj(j).ingresses };
        });
      });
    }
    function doDelete(d) {
      runAction(d, 'Delete', function () {
        return hl.api('DELETE', '/enroll/devices/' + encodeURIComponent(d.id), {}).then(function () {
          return { notice: 'Deleted ' + hl.plain(d.name, 100) + '.' };
        });
      });
    }
    function doExtend(d, seconds) {
      runAction(d, 'Extend', function () {
        return hl.api('POST', '/enroll/devices/' + encodeURIComponent(d.id) + '/extend', { expires_at: seconds }).then(function (j) {
          return { notice: 'Extended ' + hl.plain(d.name, 100) + ' to ' + fmtDate(seconds) + '.', sync: hl.obj(j).sync || null, ingresses: hl.obj(j).ingresses };
        });
      });
    }
    function doLink(d) {
      if (st.busy[d.id]) { return; }
      st.busy[d.id] = true;
      setError('');
      render();
      hl.api('POST', '/enroll/devices/' + encodeURIComponent(d.id) + '/links', { ttl: LINK_TTL, max_uses: 1 }).then(function (j) {
        delete st.busy[d.id];
        render();
        openDrawer({ mode: 'link', device: { id: d.id, name: hl.plain(d.name, 100), owner: ownerLabel(d), expires_at: d.expires_at }, link: String(hl.obj(j).url || '') });
      }, function (err) {
        delete st.busy[d.id];
        setError('New link failed for ' + hl.plain(d.name, 100) + ': ' + hl.plain(err && err.message, 200));
        render();
      });
    }

    // doRotate gives the device a new subscription URL. The URL is a secret: it goes to the panel only, never to the
    // notice or the table.
    function doRotate(d) {
      runAction(d, 'Rotate credential', function () {
        return hl.api('POST', '/enroll/devices/' + encodeURIComponent(d.id) + '/rotate-secret', {}).then(function (j) {
          openDrawer({ mode: 'rotate', device: { id: d.id, name: hl.plain(d.name, 100), owner: ownerLabel(d), expires_at: d.expires_at },
            subscription: String(hl.obj(j).subscription_url || '') });
          return { notice: 'Rotated the credential of ' + hl.plain(d.name, 100) + '. The old subscription URL no longer works.' };
        });
      });
    }

    // pushNow sends the credentials to every bridge at once, instead of the next retry.
    function pushNow() {
      if (st.busy.push) { return; }
      st.busy.push = true;
      setError('');
      setNotice('');
      render();
      hl.api('POST', '/enroll/sync', {}).then(function (j) {
        delete st.busy.push;
        j = hl.obj(j);
        if (j.sync !== undefined) { st.sync = j.sync || null; st.ingresses = hl.arr(j.ingresses); renderSync(); }
        setNotice(st.sync && st.sync.pending ? 'Pushed. A bridge is still out of sync: see the note above.' : 'Pushed. Every bridge is in sync.');
        render();
        if (ctrl.onAction) { ctrl.onAction(); }
      }, function (err) {
        delete st.busy.push;
        setError('Push failed: ' + hl.plain(err && err.message, 200));
        render();
      });
    }

    // doSupport puts the support info on the clipboard; without a clipboard the notice shows it for a copy by hand.
    function doSupport(d) {
      var text = supportInfo(d, st.ingresses, now());
      setError('');
      copy(text).then(function (ok) {
        setNotice(ok ? 'Copied the support info of ' + hl.plain(d.name, 100) + '.' : 'Copying is not available here. Copy this by hand:\n' + text);
      });
    }

    function openConfirm(d, key, td) {
      var name = hl.plain(d.name, 100);
      var panel;
      if (key === 'rotate') {
        panel = hl.confirmPanel({
          title: 'Rotate the credential of ' + name + '?', object: name,
          consequence: 'Its subscription URL stops working at once, and the next panel shows the new one: give it to the person to set in the app. Connection keys in a profile that was already downloaded stay valid. If the whole profile leaked, revoke the device and enrol a new one.',
          confirmLabel: 'Rotate', cancelLabel: 'Keep', icon: 'key', into: td,
          onConfirm: function () { doRotate(d); }, onCancel: closeOpen
        });
      } else if (key === 'revoke') {
        panel = hl.confirmPanel({
          title: 'Revoke ' + name + '?', object: name,
          consequence: 'It stops working on every bridge as soon as they sync. A revoked device cannot be switched back on: enrol a new one instead.',
          confirmLabel: 'Revoke ' + name, cancelLabel: 'Keep', icon: 'ban', into: td,
          onConfirm: function () { doRevoke(d); }, onCancel: closeOpen
        });
      } else {
        panel = hl.confirmPanel({
          title: 'Delete ' + name + '?', object: name,
          consequence: 'It is removed for good, with its history. This cannot be undone.',
          confirmLabel: 'Delete ' + name, cancelLabel: 'Keep', icon: 'trash', into: td,
          onConfirm: function () { doDelete(d); }, onCancel: closeOpen
        });
      }
      return panel;
    }

    // The extension panel: 30 or 90 days with the resulting date on the button, or a date of one's own.
    function openExtend(d, td) {
      var t = now();
      var name = hl.plain(d.name, 100);
      var panel = hl.el('div', 'note plain confirm');
      panel.setAttribute('role', 'group');
      panel.setAttribute('aria-label', 'Extend ' + name);
      panel.appendChild(hl.el('h2', null, 'Extend ' + name));
      panel.appendChild(hl.el('p', 't2', (d.expires_at * 1000 < t ? 'Expired ' : 'Expires ') + fmtDate(d.expires_at) + '.'));
      var row = hl.el('div', 'rowbtns');
      [30, 90].forEach(function (days) {
        var target = extendTo(d, days, t);
        var b = hl.button(days + ' days, to ' + fmtDate(target), 'extend', '', function () { doExtend(d, target); });
        keyed(b, 'extend:' + days);
        row.appendChild(b);
      });
      panel.appendChild(row);
      var field = hl.el('label', 'field', 'Or pick a date');
      var input = hl.el('input', 'input');
      input.setAttribute('type', 'date');
      input.setAttribute('aria-label', 'New expiry date');
      input.value = '';
      field.appendChild(input);
      panel.appendChild(field);
      var err = hl.el('p', 't2 small');
      err.hidden = true;
      panel.appendChild(err);
      var go = hl.button('Set date', 'extend', '', function () {
        var sec = endOfDay(input.value);
        if (isNaN(sec) || sec * 1000 <= now()) {
          err.textContent = 'Pick a date in the future.';
          err.hidden = false;
          return;
        }
        doExtend(d, sec);
      });
      var cancel = hl.button('Cancel', null, 'quiet', closeOpen);
      var row2 = hl.el('div', 'rowbtns');
      row2.appendChild(go);
      row2.appendChild(cancel);
      panel.appendChild(row2);
      panel.addEventListener('keydown', function (ev) { if (ev && ev.key === 'Escape') { closeOpen(); } });
      td.appendChild(panel);
      return panel;
    }

    function openPanel(d, key) {
      if (st.busy[d.id]) { return; }
      if (st.open && st.open.id === d.id && st.open.key === key) { return; }
      st.open = { id: d.id, key: key, el: null, person: d.person || null };
      render(true);
      var cell = st.open && st.open.cell;
      if (!cell) { return; }
      var panel = key === 'extend' ? openExtend(d, cell) : key === 'lock' ? openLock(d.person, cell) : openConfirm(d, key, cell);
      st.open.el = panel;
      if (key === 'extend') {
        hl.refocus(panel, 'extend:30');
      }
    }

    // ----- person and invitation actions (#287)

    function personAction(p, path, body) {
      var key = 'p:' + p.owner;
      if (st.busy[key]) { return Promise.resolve(); }
      st.busy[key] = true;
      setError('');
      setNotice('');
      st.open = null;
      render();
      return hl.api('POST', '/enroll/people/' + encodeURIComponent(p.subject) + '/' + path, body || {}).then(function (j) {
        delete st.busy[key];
        j = hl.obj(j);
        var who = hl.plain(p.email, 120) || hl.plain(p.owner, 120);
        if (j.ok === false) { setError(hl.plain(j.detail, 300)); } else { setNotice((path === 'lock' ? 'Locked ' : 'Unlocked ') + who + '.'); }
        return ctrl.loadPeople();
      }, function (e) {
        delete st.busy[key];
        setError(hl.plain(e && e.message, 200) || 'The request failed.');
        render();
      });
    }

    function openLock(p, td) {
      var who = hl.plain(p.email, 120) || hl.plain(p.owner, 120);
      return hl.confirmPanel({
        title: 'Lock ' + who + '?', object: hl.plain(p.owner, 120),
        consequence: 'All relay devices of this person are revoked on every bridge at once, their tailnet devices must sign in again, and they cannot sign in to enrol. Unlock lets them sign in, but revoked devices stay revoked.',
        confirmLabel: 'Lock', cancelLabel: 'Keep', icon: 'ban', into: td,
        onConfirm: function () { personAction(p, 'lock', { reason: 'locked in the console' }); }, onCancel: closeOpen
      });
    }

    function withdrawInvite(inv) {
      var key = 'inv' + inv.id;
      if (st.busy[key]) { return; }
      st.busy[key] = true;
      setError('');
      render();
      hl.api('POST', '/enroll/invites/' + encodeURIComponent(inv.id) + '/revoke', {}).then(function () {
        delete st.busy[key];
        setNotice('Withdrew the invitation for ' + hl.plain(inv.email, 120) + '.');
        return ctrl.loadInvites();
      }, function (e) {
        delete st.busy[key];
        setError(hl.plain(e && e.message, 200) || 'The request failed.');
        render();
      });
    }

    // inviteFor fills the invitation form with the person's email and puts focus on it.
    function inviteFor(email) {
      if (!els.inviteEmail) { return; }
      els.inviteEmail.value = hl.plain(email, 254);
      if (els.inviteSection) { els.inviteSection.hidden = false; }
      if (els.inviteDays && els.inviteDays.focus) { els.inviteDays.focus(); }
    }

    function rowAction(d, a) {
      if (a.key === 'link') { doLink(d); } else if (a.key === 'push') { pushNow(); } else if (a.key === 'support') { doSupport(d); } else { openPanel(d, a.key); }
    }

    // ----- the tree (#287): a group row per person or owner, then its devices and its invitations

    function isOpen(key) {
      if (st.query) { return true; } // a search opens what it found
      return st.openGroups.indexOf(key) >= 0;
    }
    function toggleGroup(key) {
      var i = st.openGroups.indexOf(key);
      if (i >= 0) { st.openGroups.splice(i, 1); } else { st.openGroups.push(key); }
      hl.setQueryParam('open', st.openGroups.length ? st.openGroups.join(',') : null);
      render(true);
    }

    function groupRow(g, t) {
      var open = isOpen(g.key);
      var tr = hl.el('tr', 'grp');
      var head = hl.el('td', 'name');
      var tog = keyed(hl.el('button', 'btn quiet sm tog'), 'grp:' + g.key);
      tog.setAttribute('type', 'button');
      tog.setAttribute('aria-expanded', open ? 'true' : 'false');
      tog.appendChild(hl.el('span', 'caret', open ? '▾' : '▸'));
      tog.appendChild(hl.el('span', null, g.label));
      if (g.person) { tog.setAttribute('title', hl.plain(g.person.owner, 120)); }
      tog.addEventListener('click', function () { toggleGroup(g.key); });
      head.appendChild(tog);
      tr.appendChild(head);
      var stc = hl.el('td');
      if (g.person) { var ps = personState(g.person); stc.appendChild(hl.status(ps.level, ps.word)); }
      tr.appendChild(stc);
      var info = hl.el('td', 't2');
      info.setAttribute('colspan', '3');
      info.textContent = groupCounts(g, t);
      if (g.person) {
        info.setAttribute('title', 'Last sign-in to the self-service page: ' +
          (g.person.last_login ? hl.relTime(new Date(g.person.last_login * 1000).toISOString(), t) : 'never'));
      }
      tr.appendChild(info);
      var act = hl.el('td', 'act');
      var box = hl.el('div', 'rowbtns');
      if (g.person && st.selfOn) {
        var p = g.person;
        if (p.locked) {
          box.appendChild(keyed(hl.button('Unlock', 'extend', 'sm quiet', function () { personAction(p, 'unlock'); }), 'unlock:' + g.key));
        } else {
          box.appendChild(keyed(hl.button('Lock', 'ban', 'sm quiet', function () { openPanel({ id: g.key, person: p }, 'lock'); }), 'lock:' + g.key));
          if (p.email) {
            box.appendChild(keyed(hl.button('Invite device', 'plus', 'sm quiet', function () { inviteFor(p.email); }), 'inv:' + g.key));
          }
        }
      }
      act.appendChild(box);
      tr.appendChild(act);
      return tr;
    }

    // deviceRow: one device under its group. The group row names the owner, so the row has no owner (#440). same is
    // the count of the group's devices with this name (a mark from 2 on). A device that an operator made for the
    // email of a person (in that person's group) says so, because Lock does not revoke it.
    function deviceRow(r, g, same) {
      var d = find(r.id);
      var tr = hl.el('tr', 'leaf' + (st.open && st.open.id === r.id ? ' sel' : ''));
      var nc = hl.el('td', 'name');
      var nb = keyed(hl.el('button', 'linkbtn', r.name), 'drawer:' + r.id);
      nb.setAttribute('type', 'button');
      nb.setAttribute('title', 'Show bridges, traffic and timeline');
      nb.addEventListener('click', function () { openDrawer({ mode: 'device', device: { id: r.id, name: r.name } }); });
      nc.appendChild(nb);
      nc.setAttribute('title', 'Owner ' + r.owner);
      if (same > 1) {
        var dup = hl.el('span', 't3 small', ' · ' + same + ' with this name');
        dup.setAttribute('title', same + ' devices of ' + g.label + ' are named ' + r.name + '. Rename or revoke the one that is not in use.');
        nc.appendChild(dup);
      }
      if (g.kind === 'person' && r.ownerId.indexOf(OIDC) !== 0) {
        var adm = hl.el('span', 't3 small', ' · enrolled by an admin');
        adm.setAttribute('title', 'Owner ' + r.owner + '. Lock does not revoke this device.');
        nc.appendChild(adm);
      }
      tr.appendChild(nc);
      var state = hl.el('td');
      state.appendChild(hl.status(r.level, r.word));
      tr.appendChild(state);
      tr.appendChild(hl.el('td', 't2', r.seen));
      var tc = hl.el('td', 'mono t2', r.traffic);
      if (r.trafficTitle) { tc.setAttribute('title', r.trafficTitle); }
      tr.appendChild(tc);
      var ec = hl.el('td', 't2', r.exp);
      if (r.expTitle) { ec.setAttribute('title', r.expTitle); }
      tr.appendChild(ec);
      var actCell = hl.el('td', 'act');
      var box = hl.el('div', 'rowbtns');
      r.actions.forEach(function (a) {
        var b = keyed(hl.button(a.label, a.icon, 'sm quiet', function () { rowAction(d, a); }), 'act:' + r.id + ':' + a.key);
        b.disabled = !!st.busy[r.id] || (a.key === 'push' && !!st.busy.push);
        box.appendChild(b);
      });
      actCell.appendChild(box);
      tr.appendChild(actCell);
      return tr;
    }

    function inviteRow(inv, t) {
      var key = 'inv' + inv.id;
      var tr = hl.el('tr', 'leaf' + (st.open && st.open.id === key ? ' sel' : ''));
      var who = hl.el('td', 'name');
      who.appendChild(hl.el('span', 't3', '✉ '));
      who.appendChild(hl.el('span', null, hl.plain(inv.email, 120)));
      who.setAttribute('title', inv.created_by ? 'Invitation by ' + hl.plain(inv.created_by, 80) : 'Invitation');
      tr.appendChild(who);
      var w = INVITE_WORD[inv.state] || ['warn', hl.plain(inv.state, 20)];
      var stc = hl.el('td');
      stc.appendChild(hl.status(w[0], w[1]));
      tr.appendChild(stc);
      tr.appendChild(hl.el('td', 't2', ''));
      tr.appendChild(hl.el('td', 't2', ''));
      var ec = hl.el('td', 't2', inv.expires_at ? relExpiry(inv.expires_at, t) : '');
      if (inv.expires_at) { ec.setAttribute('title', fullDate(inv.expires_at)); }
      tr.appendChild(ec);
      var act = hl.el('td', 'act');
      var box = hl.el('div', 'rowbtns');
      if (inv.state === 'pending') {
        var b = keyed(hl.button('Withdraw', 'ban', 'sm quiet', function () { withdrawInvite(inv); }), 'withdraw:' + inv.id);
        b.disabled = !!st.busy[key];
        box.appendChild(b);
      }
      act.appendChild(box);
      tr.appendChild(act);
      return tr;
    }

    function detailRow(id) {
      var dr = hl.el('tr');
      var cell = hl.el('td', 'detail');
      cell.setAttribute('colspan', String(COLUMNS));
      dr.appendChild(cell);
      els.rows.appendChild(dr);
      st.open.cell = cell;
    }

    // render rebuilds the table unless a confirmation or the extension panel is open: that is left as it is until
    // it is answered; closing it renders again. force starts a rebuild for a change the
    // operator made (opening a panel).
    function renderTable(force) {
      var t = now();
      var groups = groupTree(st.devices, st.people, st.invites, st.filter, t, st.query);
      if (st.open && !st.open.person && String(st.open.id).indexOf('inv') !== 0 && !find(st.open.id)) { st.open = null; }
      var sig = JSON.stringify([groups.map(function (g) {
        return [g.key, g.label, g.person, g.invites, g.devices.map(rowModel), isOpen(g.key)];
      }), st.filter, st.query, st.open && [st.open.id, st.open.key], st.busy, st.selfOn]);
      if (!force && st.open && st.open.el) {
        return;
      }
      if (!force && sig === st.tableSig) { return; }
      st.tableSig = sig;
      var fk = hl.focusKey(els.rows);
      hl.clear(els.rows);
      if (st.open) { st.open.cell = null; }
      if (groups.length === 0) {
        var tr0 = hl.el('tr');
        var td0 = hl.el('td', 't2', st.query ? 'Nothing matches this search.' : 'No devices match this filter.');
        td0.setAttribute('colspan', String(COLUMNS));
        tr0.appendChild(td0);
        els.rows.appendChild(tr0);
      }
      var rank = { crit: 0, warn: 1, ok: 2, off: 3 };
      groups.forEach(function (g) {
        els.rows.appendChild(groupRow(g, t));
        if (st.open && st.open.id === g.key) { detailRow(g.key); }
        if (!isOpen(g.key)) { return; }
        var same = sameNames(g.devices);
        g.devices.map(rowModel).sort(function (a, b) {
          return (rank[a.level] - rank[b.level]) || (a.expiresAt - b.expiresAt) || (a.name < b.name ? -1 : a.name > b.name ? 1 : 0);
        }).forEach(function (r) {
          els.rows.appendChild(deviceRow(r, g, same[r.name.trim().toLowerCase()] || 0));
          if (st.open && st.open.id === r.id) { detailRow(r.id); }
        });
        g.invites.forEach(function (inv) { els.rows.appendChild(inviteRow(inv, t)); });
      });
      hl.refocus(els.rows, fk);
    }

    function renderChips() {
      var any = st.devices.length > 0 || hl.arr(st.people).length > 0 || hl.arr(st.invites).length > 0;
      var sig = any ? 'on' : 'off';
      els.chips.hidden = !any;
      if (sig !== st.chipSig) {
        st.chipSig = sig;
        st.chips = {};
        hl.clear(els.chips);
        FILTERS.forEach(function (f) {
          var b = hl.el('button', 'chip', f[1]);
          b.setAttribute('type', 'button');
          b.addEventListener('click', function () { ctrl.setFilter(f[0]); });
          st.chips[f[0]] = b;
          els.chips.appendChild(b);
        });
      }
      Object.keys(st.chips).forEach(function (k) {
        st.chips[k].className = 'chip' + (k === st.filter ? ' on' : '');
        st.chips[k].setAttribute('aria-pressed', k === st.filter ? 'true' : 'false');
      });
    }

    function summary() {
      var t = now();
      var active = 0, seen = 0;
      st.devices.forEach(function (d) {
        if (stateOf(d, t).kind !== 'active') { return; }
        active++;
        if (d.last_active > 0 && d.last_active * 1000 >= t - SEEN_MS) { seen++; }
      });
      var out = active + ' active, ' + seen + ' seen today';
      if (st.people && st.people.length) { out += ', ' + (st.people.length === 1 ? '1 person' : st.people.length + ' people'); }
      var waiting = hl.arr(st.invites).filter(function (i) { return i && i.state === 'pending'; }).length;
      if (waiting) { out += ', ' + (waiting === 1 ? '1 invitation waiting' : waiting + ' invitations waiting'); }
      return out;
    }

    function render(force) {
      var text = st.loaded ? summary() : '';
      if (st.failed) { text = st.loaded ? text + '. The data may be out of date.' : 'Could not load the devices. Retrying.'; }
      els.sub.textContent = text;
      var any = st.devices.length > 0 || hl.arr(st.people).length > 0 || hl.arr(st.invites).length > 0;
      els.empty.hidden = !(st.loaded && !any);
      els.tablewrap.hidden = !any;
      els.foot.hidden = !any;
      if (els.search) { els.search.hidden = !any; }
      renderChips();
      renderTable(force);
    }

    // ----- the enrolment panel

    function closeDrawer() {
      var dr = st.drawer;
      // While the device is being made the panel stays: closing now would lose the link it is about to show.
      if (!dr || dr.creating) { return; }
      if (dr.poller) { dr.poller.stop(); dr.poller = null; }
      dr.root.remove();
      dr.link = '';
      dr.subscription = '';
      dr.linkBox = null;
      hl.clear(dr.body);
      st.drawer = null;
      hl.setQueryParam('enrol', null);
      var back = dr.opener;
      if (back && back.isConnected !== false && back.focus) { back.focus(); }
    }

    function drawerFocusables(dr) {
      var out = [];
      (function walk(n) {
        if (n.tag === 'button' || n.tag === 'input' || n.tag === 'select' || n.tag === 'summary') {
          if (!n.disabled && !n.hidden) { out.push(n); }
        }
        Array.prototype.slice.call(n.children || []).forEach(walk);
      })(dr.aside);
      return out;
    }

    // setWaiting fills the status under the link: waiting for the first use, or enrolled.
    function setWaiting(dr) {
      if (!dr.waitBox) { return; }
      hl.clear(dr.waitBox);
      if (dr.enrolled) {
        dr.waitBox.appendChild(hl.status('ok', 'Enrolled'));
        dr.waitBox.appendChild(hl.el('p', 't3 small', 'The device added its profile' + (dr.enrolledAt ? ' ' + hl.relTime(new Date(dr.enrolledAt * 1000).toISOString(), now()) : '') + '. The link is used up.'));
        if (dr.linkBox) { dr.linkBox.hidden = true; }
        if (dr.poller) { dr.poller.stop(); dr.poller = null; }
        return;
      }
      dr.waitBox.appendChild(hl.status('ok', 'Waiting for first use'));
      dr.waitBox.appendChild(hl.el('p', 't3 small', 'This updates by itself when the phone adds the profile. You can close this panel: the link stays valid.'));
    }

    // checkEnrolled looks for the panel's device in fresh data and notes its first use.
    function checkEnrolled() {
      var dr = st.drawer;
      if (!dr || dr.step !== 'link' || dr.enrolled || !dr.device) { return; }
      var d = find(dr.device.id);
      if (d && setUp(d)) {
        dr.enrolled = true;
        dr.enrolledAt = d.enrolled_at || 0;
        setWaiting(dr);
      }
    }

    function showCopied(btn, label) {
      var span = btn.children[btn.children.length - 1];
      var prev = span.textContent;
      span.textContent = label;
      later(function () { span.textContent = prev; }, 2000);
    }

    // copyButton: the clipboard if there is one, else the text is selected for a copy by hand.
    function copyButton(label, text, shown, note, primary) {
      var b = hl.button(label, 'copy', primary ? 'primary' : '', function () {
        copy(text).then(function (ok) {
          if (ok) { showCopied(b, 'Copied'); return; }
          selectAll(shown);
          note.textContent = 'Copying is not available here. The text is selected: copy it by hand.';
          note.hidden = false;
        });
      });
      return b;
    }

    function secretBox(text, label) {
      var s = hl.el('div', 'secret', text);
      s.setAttribute('aria-label', label);
      return s;
    }

    function buildLink(dr) {
      var body = dr.body;
      hl.clear(body);
      dr.step = 'link';
      var dev = dr.device;
      var head = dr.mode === 'create'
        ? hl.status('ok', dev.name + ' created for ' + dev.owner + ', expires ' + fmtDate(dev.expires_at))
        : hl.status('ok', 'One-time link for ' + dev.name);
      body.appendChild(head);
      dr.copyNote = hl.el('p', 't2 small');
      dr.copyNote.hidden = true;
      dr.linkBox = hl.el('div', 'stack');
      if (dr.linkError) {
        dr.linkBox.appendChild(hl.el('div', 'note crit', 'The one-time link could not be made: ' + dr.linkError + ' Use New link in the table, or set the device up by hand with the subscription URL below.'));
      } else {
        var top = hl.el('div', 'row');
        var q = qrOf(dr.link);
        if (q) { top.appendChild(q); }
        var words = hl.el('div', 'stack beside');
        words.appendChild(hl.el('p', null, 'Scan with the phone, or send the link to its owner.'));
        words.appendChild(hl.el('p', 't2 small', 'Works once, valid for 24 hours. Opening it here would use it up, so it is not clickable.'));
        top.appendChild(words);
        dr.linkBox.appendChild(top);
        var secret = secretBox(dr.link, 'Enrolment link');
        dr.linkBox.appendChild(secret);
        var btns = hl.el('div', 'rowbtns start');
        btns.appendChild(copyButton('Copy link', dr.link, secret, dr.copyNote, true));
        dr.linkBox.appendChild(btns);
      }
      body.appendChild(dr.linkBox);
      body.appendChild(dr.copyNote);
      if (dr.mode === 'create') {
        var again = hl.el('div', 'rowbtns start');
        again.appendChild(hl.button('Enrol another device', null, 'quiet', function () { dr.enrolled = false; buildDetails(dr); }));
        body.appendChild(again);
      }
      if (!dr.linkError) {
        body.appendChild(hl.el('hr', 'divider'));
        dr.waitBox = hl.el('div', 'stack');
        body.appendChild(dr.waitBox);
        setWaiting(dr);
      }
      if (dr.subscription) {
        var det = hl.el('details');
        if (dr.linkError) { det.setAttribute('open', ''); }
        det.appendChild(hl.el('summary', 't2 small', 'Manual setup: subscription URL'));
        var inner = hl.el('div', 'stack');
        inner.appendChild(hl.el('p', 't3 small', 'For when the link does not open the app. Paste it into the client by hand.'));
        var sub = secretBox(dr.subscription, 'Subscription URL');
        inner.appendChild(sub);
        var row = hl.el('div', 'rowbtns start');
        row.appendChild(copyButton('Copy subscription URL', dr.subscription, sub, dr.copyNote, false));
        inner.appendChild(row);
        det.appendChild(inner);
        body.appendChild(det);
      }
      if (!dr.enrolled && !dr.linkError && !dr.poller) {
        dr.poller = poll(function () { return ctrl.loadDevices ? ctrl.loadDevices() : undefined; }, WAIT_MS);
      }
      checkEnrolled();
    }

    // buildRotated shows the new subscription URL after a rotation (#441). Like a link, it lives only in this panel.
    function buildRotated(dr) {
      var body = dr.body;
      hl.clear(body);
      dr.step = 'rotated';
      body.appendChild(hl.status('ok', 'New subscription URL for ' + dr.device.name));
      body.appendChild(hl.el('p', 't2 small', 'The old URL no longer works. Give this one to the person to paste into the app, or send a new link instead.'));
      dr.copyNote = hl.el('p', 't2 small');
      dr.copyNote.hidden = true;
      var sub = secretBox(dr.subscription, 'Subscription URL');
      body.appendChild(sub);
      var row = hl.el('div', 'rowbtns start');
      row.appendChild(copyButton('Copy subscription URL', dr.subscription, sub, dr.copyNote, true));
      body.appendChild(row);
      body.appendChild(dr.copyNote);
    }

    function daysHint(value) {
      var sec = endOfDay(value);
      if (isNaN(sec)) { return 'Pick a date.'; }
      var days = Math.round((sec * 1000 - now()) / DAY);
      if (sec * 1000 <= now()) { return 'That date has passed.'; }
      return days === 1 ? '1 day from today' : days + ' days from today';
    }

    function buildDetails(dr) {
      var body = dr.body;
      hl.clear(body);
      dr.step = 'details';
      dr.mode = 'create';
      dr.waitBox = null;
      dr.linkBox = null;
      dr.link = '';
      dr.subscription = '';
      if (dr.poller) { dr.poller.stop(); dr.poller = null; }
      var form = hl.el('form', 'stack');
      form.setAttribute('novalidate', '');
      var fields = {};
      function field(label, key, input) {
        var l = hl.el('label', 'field', label);
        l.appendChild(input);
        var e = hl.el('div');
        e.hidden = true;
        l.appendChild(e);
        fields[key] = { input: input, err: e };
        form.appendChild(l);
        return input;
      }
      var name = hl.el('input', 'input');
      name.setAttribute('type', 'text');
      name.setAttribute('maxlength', '100');
      name.setAttribute('autocomplete', 'off');
      name.setAttribute('data-fk', 'name');
      field('Device name', 'name', name);
      var owner = hl.el('input', 'input');
      owner.setAttribute('type', 'text');
      owner.setAttribute('maxlength', '100');
      owner.setAttribute('autocomplete', 'off');
      field('Owner', 'owner', owner);
      var pair = hl.el('div', 'row');
      var client = hl.el('select', 'input');
      CLIENTS.forEach(function (c) {
        var o = hl.el('option', null, c);
        o.setAttribute('value', c);
        client.appendChild(o);
      });
      client.value = lastClient();
      var expires = hl.el('input', 'input');
      expires.setAttribute('type', 'date');
      expires.value = defaultExpiry(now());
      var cl = hl.el('label', 'field grow', 'Client');
      cl.appendChild(client);
      var el2 = hl.el('label', 'field grow', 'Expires');
      el2.appendChild(expires);
      var hint = hl.el('div', 't3 small', daysHint(expires.value));
      el2.appendChild(hint);
      var ee = hl.el('div');
      ee.hidden = true;
      el2.appendChild(ee);
      fields.expires = { input: expires, err: ee };
      pair.appendChild(cl);
      pair.appendChild(el2);
      form.appendChild(pair);
      var refreshHint = function () { hint.textContent = daysHint(expires.value); };
      expires.addEventListener('input', refreshHint);
      expires.addEventListener('change', refreshHint);
      dr.formError = hl.el('div', 'note crit');
      dr.formError.setAttribute('role', 'alert');
      dr.formError.hidden = true;
      form.appendChild(dr.formError);
      var submit = hl.el('button', 'btn primary', 'Create and get link');
      submit.setAttribute('type', 'submit');
      form.appendChild(submit);
      body.appendChild(form);
      dr.fields = fields;

      function invalid(key, text) {
        fields[key].err.textContent = '';
        fields[key].err.appendChild(hl.status('crit', text));
        fields[key].err.hidden = false;
        fields[key].input.setAttribute('aria-invalid', 'true');
      }
      function valid(key) {
        hl.clear(fields[key].err);
        fields[key].err.hidden = true;
        fields[key].input.removeAttribute('aria-invalid');
      }
      form.addEventListener('submit', function (ev) {
        if (ev && ev.preventDefault) { ev.preventDefault(); }
        if (dr.creating) { return; }
        var n = String(name.value || '').trim();
        var o = String(owner.value || '').trim();
        var sec = endOfDay(expires.value);
        var first = null;
        ['name', 'owner', 'expires'].forEach(valid);
        if (!n) { invalid('name', 'Name is required.'); first = first || name; }
        if (!o) { invalid('owner', 'Owner is required.'); first = first || owner; }
        if (isNaN(sec) || sec * 1000 <= now()) { invalid('expires', 'Pick an expiry date in the future.'); first = first || expires; }
        if (first) { first.focus(); return; }
        var c = CLIENTS.indexOf(client.value) >= 0 ? client.value : CLIENTS[0];
        rememberClient(c);
        dr.creating = true;
        submit.disabled = true;
        submit.textContent = 'Creating…';
        dr.formError.hidden = true;
        var made = null;
        hl.api('POST', '/enroll/devices', { name: n, owner: o, client: c, expires_at: sec }).then(function (j) {
          made = hl.obj(j);
          dr.device = { id: made.id, name: hl.plain(n, 100), owner: hl.plain(o, 100), expires_at: sec };
          dr.subscription = typeof made.subscription_url === 'string' ? made.subscription_url : '';
          dr.link = '';
          dr.linkError = '';
          dr.enrolled = false;
          if (made.sync !== undefined) { st.sync = made.sync || null; st.ingresses = hl.arr(made.ingresses); renderSync(); }
          if (ctrl.loadDevices) { ctrl.loadDevices(); }
          return hl.api('POST', '/enroll/devices/' + encodeURIComponent(made.id) + '/links', { ttl: LINK_TTL, max_uses: 1 }).then(function (l) {
            dr.link = String(hl.obj(l).url || '');
          }, function (e) {
            dr.linkError = hl.plain(e && e.message, 200);
          });
        }).then(function () {
          dr.creating = false;
          if (st.drawer === dr) {
            buildLink(dr);
            // The form (and the button that was focused) is gone: land on the first button of the result, the copy button.
            var first = dr.body.querySelector ? dr.body.querySelector('button') : null;
            if (first) { first.focus(); }
          }
        }).catch(function (e) {
          dr.creating = false;
          var msg = hl.plain(e && e.message, 200);
          if (made) {
            // The device exists: the form stays shut so a second one is not made by mistake.
            var text = 'The device was created, but this panel could not show its link' + (msg ? ': ' + msg : '') + '. Find it in the table and use New link.';
            if (dr.step === 'link') {
              dr.body.appendChild(hl.el('div', 'note crit', text));
            } else {
              dr.formError.textContent = text;
              dr.formError.hidden = false;
              submit.textContent = 'Created';
            }
            try { if (ctrl.loadDevices) { ctrl.loadDevices(); } } catch (e2) { /* the next poll reads it */ }
            return;
          } else {
            submit.disabled = false;
            submit.textContent = 'Create and get link';
            dr.formError.textContent = msg;
          }
          dr.formError.hidden = false;
        });
      });
      name.focus();
    }

    function openDrawer(opts) {
      opts = opts || {};
      if (st.drawer) { return st.drawer; }
      var dr = { step: '', mode: opts.mode || 'create', device: opts.device || null, link: opts.link || '', linkError: '', subscription: opts.subscription || '',
        enrolled: false, enrolledAt: 0, poller: null, creating: false, opener: typeof document !== 'undefined' ? document.activeElement : null };
      dr.root = hl.el('div');
      var scrim = hl.el('div', 'scrim');
      scrim.addEventListener('click', closeDrawer);
      dr.aside = hl.el('aside', 'drawer');
      dr.aside.setAttribute('role', 'dialog');
      dr.aside.setAttribute('aria-modal', 'true');
      var title = dr.mode === 'device' ? 'Device ' + hl.plain(dr.device && dr.device.name, 100) : 'Enrol device';
      dr.aside.setAttribute('aria-label', title);
      var head = hl.el('div', 'panelhead');
      head.appendChild(hl.el('h2', null, title));
      var close = hl.el('button', 'btn quiet sm');
      close.setAttribute('type', 'button');
      close.setAttribute('aria-label', 'Close');
      close.setAttribute('title', 'Close');
      var ic = hl.icon('close');
      if (ic) { close.appendChild(ic); }
      close.addEventListener('click', closeDrawer);
      head.appendChild(close);
      dr.aside.appendChild(head);
      dr.body = hl.el('div', 'stack');
      dr.aside.appendChild(dr.body);
      dr.aside.addEventListener('keydown', function (ev) {
        if (!ev) { return; }
        if (ev.key === 'Escape') { closeDrawer(); return; }
        if (ev.key === 'Tab') {
          var list = drawerFocusables(dr);
          if (!list.length) { return; }
          var ae = document.activeElement;
          var last = list[list.length - 1], first = list[0];
          if (!ev.shiftKey && ae === last) { if (ev.preventDefault) { ev.preventDefault(); } first.focus(); }
          else if (ev.shiftKey && ae === first) { if (ev.preventDefault) { ev.preventDefault(); } last.focus(); }
        }
      });
      dr.root.appendChild(scrim);
      dr.root.appendChild(dr.aside);
      els.drawer.appendChild(dr.root);
      st.drawer = dr;
      if (dr.mode === 'device') { buildDevice(dr); close.focus(); return dr; }
      hl.setQueryParam('enrol', '1');
      if (dr.mode === 'link') { buildLink(dr); close.focus(); } else if (dr.mode === 'rotate') { buildRotated(dr); close.focus(); } else { buildDetails(dr); }
      return dr;
    }

    // ----- the device drawer (#437): profile, traffic and credential per bridge, tailnet devices, timeline

    var CRED_WORD = { current: ['ok', 'Has the current credential'], pending: ['warn', 'Update pending'],
      unknown: ['off', 'Not known yet'] };
    var EVENT_WORD = { created: 'Created', invited: 'Link issued', link_used: 'Link used',
      profile_downloaded: 'Profile first downloaded', last_traffic: 'Last traffic', extended: 'Extended',
      revoked: 'Revoked' };

    function bytesText(n) {
      n = Number(n) || 0;
      var u = ['B', 'KB', 'MB', 'GB', 'TB'], i = 0;
      while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
      return (i ? n.toFixed(1) : String(n)) + ' ' + u[i];
    }
    function whenText(sec) {
      return sec ? fmtDate(sec) + ' (' + hl.relTime(new Date(sec * 1000).toISOString(), now()) + ')' : 'never';
    }
    function section(body, text) { body.appendChild(hl.el('h3', 'small', text)); }
    function line(body, label, value) {
      var p = hl.el('p', 'small');
      p.appendChild(hl.el('span', 't2', label + ': '));
      p.appendChild(hl.el('span', null, value));
      body.appendChild(p);
    }

    function fillDevice(dr, j) {
      var body = dr.body;
      hl.clear(body);
      var prof = hl.obj(j.profile);
      section(body, 'Profile');
      line(body, 'First download', whenText(prof.first));
      line(body, 'Last download', whenText(prof.last));
      line(body, 'Format the app asked for', prof.format ? hl.plain(prof.format, 40) : 'not known');
      section(body, 'Bridges');
      var bridges = hl.arr(j.bridges);
      if (!bridges.length) { body.appendChild(hl.el('p', 't3 small', 'No bridge is configured.')); }
      bridges.forEach(function (b) {
        var box = hl.el('div', 'stack');
        box.setAttribute('data-bridge', hl.plain(b.name, 63));
        var w = CRED_WORD[b.credential] || CRED_WORD.unknown;
        var headRow = hl.el('div', 'row');
        headRow.appendChild(hl.el('strong', null, hl.plain(b.name, 63)));
        headRow.appendChild(hl.status(w[0], w[1]));
        if (b.online === false) { headRow.appendChild(hl.status('crit', 'Offline')); }
        box.appendChild(headRow);
        var tr = b.traffic;
        line(box, 'Traffic', !b.answered ? 'the bridge did not answer' :
          tr ? '↑ ' + bytesText(tr.up) + ' ↓ ' + bytesText(tr.down) : 'none');
        body.appendChild(box);
      });
      section(body, 'Tailnet devices of the person');
      if (j.tailnet === null || j.tailnet === undefined) {
        body.appendChild(hl.el('p', 't3 small', 'Not known: the owner is not a self-enrolled person, or headscale did not answer.'));
      } else if (!hl.arr(j.tailnet).length) {
        body.appendChild(hl.el('p', 't3 small', 'None.'));
      } else {
        hl.arr(j.tailnet).forEach(function (n) {
          var p = hl.el('div', 'row');
          p.appendChild(hl.el('span', null, hl.plain(n.name, 63)));
          p.appendChild(n.online ? hl.status('ok', 'Online') : hl.status('off', 'Last seen ' + whenText(n.last_seen)));
          body.appendChild(p);
        });
      }
      section(body, 'Timeline');
      var events = hl.arr(j.timeline);
      if (!events.length) { body.appendChild(hl.el('p', 't3 small', 'No event is recorded.')); }
      events.forEach(function (e) {
        line(body, EVENT_WORD[e.event] || hl.plain(e.event, 30), whenText(e.at));
      });
    }

    function buildDevice(dr) {
      hl.clear(dr.body);
      dr.step = 'device';
      dr.body.appendChild(hl.el('p', 't3 small', 'Loading…'));
      hl.api('GET', '/enroll/devices/' + encodeURIComponent(dr.device.id)).then(function (j) {
        if (st.drawer === dr) { fillDevice(dr, hl.obj(j)); }
      }, function (err) {
        if (st.drawer !== dr) { return; }
        hl.clear(dr.body);
        dr.body.appendChild(hl.el('div', 'note crit', 'The device could not be read: ' + hl.plain(err && err.message, 200)));
      });
    }

    // ----- control surface

    ctrl.setFilter = function (f) {
      st.filter = plainFilter(f);
      hl.setQueryParam('filter', st.filter === DEFAULT_FILTER ? null : st.filter);
      st.open = null;
      render(true);
    };
    ctrl.update = function (d) {
      d = hl.obj(d);
      st.loaded = true;
      st.devices = hl.arr(d.devices);
      st.sync = d.sync || null;
      st.ingresses = hl.arr(d.ingresses);
      st.partial = !!d.stats_partial;
      st.bridges = hl.arr(d.bridges);
      els.traffic.textContent = st.partial ? 'Traffic (partial)' : 'Traffic';
      els.traffic.setAttribute('title', st.partial ? 'Partial: a bridge did not answer, so its traffic is missing. See Bridges above.' :
        'Traffic since each bridge last restarted its counters (on a credential change). Not a history.');
      renderSync();
      renderBridges();
      checkEnrolled();
      render();
    };
    ctrl.setFailed = function (f) { st.failed = !!f; render(); };
    // updatePeople / updateInvites take the lists of /enroll/people and /enroll/invites; null: self-enrolment is off.
    ctrl.updatePeople = function (list) { st.people = list === null ? null : hl.arr(list); st.selfOn = list !== null; render(); };
    ctrl.updateInvites = function (list) { st.invites = list === null ? null : hl.arr(list); render(); };
    ctrl.setQuery = function (q) {
      st.query = String(q || '').trim().toLowerCase().slice(0, 100);
      st.open = null;
      render(true);
    };
    ctrl.loadPeople = function () { return Promise.resolve(); };
    ctrl.loadInvites = function () { return Promise.resolve(); };
    ctrl.openDrawer = openDrawer;
    ctrl.closeDrawer = closeDrawer;
    ctrl.state = st;
    ctrl.onAction = null;

    if (els.empty) {
      els.empty.appendChild(hl.el('div', 'rowbtns start')).appendChild(hl.button('Enrol device', 'plus', 'primary', function () { openDrawer(); }));
    }
    render();
    // The top-bar action links to /enroll?enrol=1: the panel is open on arrival.
    if (hl.queryParam('enrol') === '1') { openDrawer(); }
    return ctrl;
  }

  function init() {
    var root = document.getElementById('devices');
    if (!root) { return; }
    var $ = function (id) { return document.getElementById(id); };
    var ctrl = mount({
      sub: $('sub'), chips: $('chips'), error: $('error'), notice: $('notice'), syncnote: $('syncnote'), bridges: $('bridges'), empty: $('empty'),
      tablewrap: $('tablewrap'), rows: $('rows'), foot: $('foot'), traffic: $('trafficth'), drawer: $('drawer'),
      search: $('search'), inviteSection: $('invites'), inviteEmail: $('inviteemail'), inviteDays: $('invitedays')
    });
    var search = $('searchinput');
    if (search) { search.addEventListener('input', function () { ctrl.setQuery(search.value); }); }

    // People and invitations (#287): a 404 means self-enrolment is off, and the tree shows only devices.
    function loadPeople() {
      return hl.api('GET', '/enroll/people').then(function (j) { ctrl.updatePeople(hl.arr(hl.obj(j).people)); },
        function (e) { if (e && e.status === 404) { ctrl.updatePeople(null); } });
    }
    function loadInvites() {
      return hl.api('GET', '/enroll/invites').then(function (j) { ctrl.updateInvites(hl.arr(hl.obj(j).invites)); },
        function (e) { if (e && e.status === 404) { ctrl.updateInvites(null); } });
    }
    ctrl.loadPeople = loadPeople;
    ctrl.loadInvites = loadInvites;
    function load() {
      return Promise.all([hl.api('GET', '/enroll/devices').then(function (j) {
        ctrl.update(j);
        ctrl.setFailed(false);
      }, function () { ctrl.setFailed(true); }), loadPeople(), loadInvites()]);
    }
    ctrl.loadDevices = load;
    hl.devicesCtrl = ctrl; // the invitation form reloads the tree after it makes one
    var poller = hl.poll(load, POLL_MS);
    if (document.hidden) { load(); }
    document.addEventListener('visibilitychange', function () { if (!document.hidden) { poller.now(); } });
    ctrl.onAction = function () { poller.now(); };
  }

  hl.devices = {
    stateOf: stateOf, actionsFor: actionsFor, supportInfo: supportInfo, mount: mount, init: init, endOfDay: endOfDay, defaultExpiry: defaultExpiry,
    extendTo: extendTo, lastClient: lastClient, setUp: setUp, groupTree: groupTree, groupCounts: groupCounts,
    relExpiry: relExpiry, sameNames: sameNames
  };
  init();
})();
