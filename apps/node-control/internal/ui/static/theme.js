// Applies the saved theme before first paint. No attribute on the root means System.
(function () {
  try {
    var t = window.localStorage.getItem('hl-theme');
    if (t === 'light' || t === 'dark') { document.documentElement.setAttribute('data-hl-theme', t); }
  } catch (e) { /* no storage: the page follows the OS */ }
})();
