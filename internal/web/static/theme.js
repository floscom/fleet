// Applies the theme chosen in Settings before the first paint, so a dark
// choice never flashes light. "system" (the default) follows the device.
// app.js (setTheme) changes it later; CSP keeps this out of index.html.
'use strict';
(() => {
  let t = 'system';
  try {
    t = localStorage.getItem('fleet.theme') || 'system';
  } catch {}
  document.documentElement.dataset.theme = t;
})();
