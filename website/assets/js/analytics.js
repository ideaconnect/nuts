/* Custom GA4 event tracking for the NUTS site.

   Every event here fires through track(), which is a HARD no-op until Google
   Analytics has actually loaded. Under our prior-consent model that happens
   only after the visitor presses Accept (see loadGoogleAnalytics in
   _layouts/default.html, which sets window.__gaLoaded). So no interaction is
   ever recorded — not even queued in dataLayer — without consent.

   Events (custom GA4 events; counts appear under Reports -> Engagement ->
   Events automatically, param values need custom dimensions to show in reports):

     download        - visitor copied an install/run command (the acquisition
                       action on a module site). param: method = xcaddy | docker
     github_click    - visitor opened the NUTS GitHub repo (any link to it).
                       param: link_url
     contact_submit  - contact form submitted; fired on the thank-you page,
                       which is only reachable after a successful send.
     features_scroll - visitor scrolled ~90% down the /features/ page (once).
                       param: percent = 90

   Wiring: install commands are marked with data-download-cmd in index.html;
   GitHub repo links are matched by href; the last two are matched by URL path,
   so no per-page markup is required. */
(function () {
  "use strict";

  // Only emit once GA is loaded (i.e. consent was granted). window.gtag exists
  // pre-consent as a dataLayer stub, so gating on __gaLoaded — not on gtag —
  // is what keeps pre-consent interactions from being stashed and later sent.
  function track(name, params) {
    if (window.__gaLoaded && typeof window.gtag === "function") {
      window.gtag("event", name, params || {});
    }
  }

  var path = location.pathname.replace(/\/+$/, ""); // normalise trailing slash

  // --- github_click --------------------------------------------------------
  // Delegated so it covers every repo link (header desktop + mobile, hero,
  // bottom CTA, footer). Capture phase so it still counts even if something
  // stops propagation. Sponsor (github.com/sponsors/...) and Buy-Me-a-Coffee
  // links don't match this selector, so they're naturally excluded.
  document.addEventListener("click", function (e) {
    var t = e.target;
    var a = t && t.closest ? t.closest('a[href*="github.com/ideaconnect/nuts"]') : null;
    if (a) track("github_click", { link_url: a.href });
  }, true);

  // --- download ------------------------------------------------------------
  // An install/run command is marked with data-download-cmd="xcaddy|docker".
  // Two acquisition paths, no double count: the copy button uses the async
  // Clipboard API (no native 'copy' event), while a manual text selection +
  // Ctrl/Cmd-C fires 'copy' but no button click.
  document.addEventListener("click", function (e) {
    var t = e.target;
    var btn = t && t.closest ? t.closest(".copy-btn") : null;
    if (!btn) return;
    var block = btn.closest("[data-download-cmd]");
    if (block) track("download", { method: block.getAttribute("data-download-cmd") });
  }, true);

  Array.prototype.forEach.call(document.querySelectorAll("[data-download-cmd]"), function (block) {
    block.addEventListener("copy", function () {
      track("download", { method: block.getAttribute("data-download-cmd") });
    });
  });

  // --- contact_submit ------------------------------------------------------
  // The thank-you page is the redirect target after a successful form POST,
  // so reaching it is a reliable "message sent" signal (and a normal page
  // load, unlike firing during the form's own navigation).
  if (/\/contact\/thanks$/.test(path)) {
    track("contact_submit", {});
  }

  // --- features_scroll -----------------------------------------------------
  // One event the first time the visitor reaches ~90% depth on /features/.
  if (/\/features$/.test(path)) {
    var fired = false;
    var onScroll = function () {
      if (fired) return;
      var de = document.documentElement;
      var max = de.scrollHeight - window.innerHeight;
      if (max > 0 && (window.scrollY || window.pageYOffset) / max >= 0.9) {
        fired = true;
        window.removeEventListener("scroll", onScroll);
        track("features_scroll", { percent: 90 });
      }
    };
    window.addEventListener("scroll", onScroll, { passive: true });
  }
})();
