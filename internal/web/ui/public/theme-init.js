// Applies the saved theme before first paint so the page never flashes the
// wrong one. React mounts far too late to do this.
//
// It must be an external file, not an inline <script>: the server sends
// Content-Security-Policy: script-src 'self' with no 'unsafe-inline', so an
// inline bootstrap is silently blocked and every theme selector stops matching.
(function () {
  try {
    document.documentElement.dataset.theme =
      localStorage.getItem("omnilog_theme") || "system";
  } catch (e) {
    document.documentElement.dataset.theme = "system";
  }
})();
