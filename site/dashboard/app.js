/* Starts the installable app. Kept out of index.html so the page runs under a
   Content-Security-Policy without 'unsafe-inline', as it does when the Helm chart serves it. */
window.addEventListener('DOMContentLoaded', function () {
  var source = window.SubnetDashboard.sourceFromPage();
  var dash = window.SubnetDashboard.mount(document.getElementById('app'), {
    source: source,
    onTheme: function (id) {
      // Keep the OS chrome in step with the dashboard theme.
      var bg = getComputedStyle(document.getElementById('app')).getPropertyValue('--d-bg').trim();
      document.querySelector('meta[name="theme-color"]').setAttribute('content', bg || '#0c1222');
      document.body.classList.toggle('theme-daylight', id === 'daylight');
    }
  });
  dash.start();

  // The link back to the project page only makes sense on the project's own site.
  document.querySelector('.back').hidden = source.mode !== 'demo';

  // Installability: browsers fire this when the app qualifies; Safari installs from the share sheet.
  var deferred = null;
  window.addEventListener('beforeinstallprompt', function (e) {
    e.preventDefault();
    deferred = e;
    dash.installButton.hidden = false;
  });
  dash.installButton.addEventListener('click', function () {
    if (!deferred) return;
    deferred.prompt();
    deferred.userChoice.finally(function () { deferred = null; dash.installButton.hidden = true; });
  });
  window.addEventListener('appinstalled', function () { dash.installButton.hidden = true; });

  // Offline, the demo still works from the cache; live data does not, and says so itself.
  function offline() { document.body.classList.toggle('is-offline', source.mode === 'demo' && !navigator.onLine); }
  window.addEventListener('online', offline);
  window.addEventListener('offline', offline);
  offline();

  // Only the demo is cached for offline use. A live page would have nothing to show offline,
  // and its service worker would sit between the page and the API for no benefit.
  if (source.mode === 'demo' && 'serviceWorker' in navigator) {
    navigator.serviceWorker.register('sw.js', { scope: './' }).catch(function () {});
  }
});
