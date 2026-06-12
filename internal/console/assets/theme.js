/*
 * notify console — theme toggle (dagnats app.js idiom).
 *
 * Three-state cycle: System (prefers-color-scheme) → Light → Dark →
 * System. State persists in localStorage; absence of the key means
 * "follow system". The theme attribute lives on <html> so the html
 * element itself paints the chosen palette (setting it on <body> causes
 * a two-tone scroll-bounce when the OS prefers the other scheme). The
 * layout's inline head stub applies the persisted theme before first
 * paint; this file owns the toggle UI only.
 */
(function () {
  const STORAGE_KEY = "notify-console-theme";
  const MODES = ["system", "light", "dark"];
  const LABELS = { system: "System", light: "Light", dark: "Dark" };

  function current() {
    try {
      const v = localStorage.getItem(STORAGE_KEY);
      return MODES.indexOf(v) >= 0 ? v : "system";
    } catch (_) {
      return "system";
    }
  }

  function apply(mode) {
    if (mode === "system") {
      document.documentElement.removeAttribute("data-theme");
    } else {
      document.documentElement.setAttribute("data-theme", mode);
    }
  }

  function updateButton(btn, mode) {
    if (!btn) return;
    btn.setAttribute("data-theme-mode", mode);
    btn.setAttribute("aria-label", "Theme: " + LABELS[mode] + " (click to cycle)");
    const label = btn.querySelector(".theme-toggle-label");
    if (label) label.textContent = LABELS[mode];
  }

  function cycle() {
    const next = MODES[(MODES.indexOf(current()) + 1) % MODES.length];
    try {
      localStorage.setItem(STORAGE_KEY, next);
    } catch (_) {}
    apply(next);
    updateButton(document.getElementById("theme-toggle"), next);
  }

  function init() {
    const mode = current();
    apply(mode);
    const btn = document.getElementById("theme-toggle");
    updateButton(btn, mode);
    if (btn) btn.addEventListener("click", cycle);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
