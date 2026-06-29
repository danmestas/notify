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

/*
 * Slide-over drawer focus manager — the reusable a11y mechanism every
 * .console-slideover opts into with markup alone (no per-drawer JS).
 *
 * Contract a drawer must satisfy to be managed (see the templates):
 *   - element carries class "console-slideover" and aria-modal="true"
 *   - it is shown/hidden via Datastar data-show, which toggles
 *     inline style.display between "" (open) and "none" (closed)
 *   - Escape-to-close is wired declaratively in the template
 *     (data-on:keydown__window flips the open signal or navigates);
 *     this manager only handles focus move/restore, not closing.
 *
 * On open (display flips away from "none") it remembers the element that
 * had focus (the trigger), then moves focus to the drawer's first
 * focusable field (or its heading as a fallback). On close it restores
 * focus to the remembered trigger. A MutationObserver on the style/
 * hidden attributes is framework-agnostic: it works no matter how
 * Datastar toggles visibility, and needs zero template wiring beyond the
 * standard attribute set.
 */
(function () {
  var FOCUSABLE =
    'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]),' +
    ' select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

  function isOpen(el) {
    if (el.hidden) return false;
    // Datastar data-show sets style.display = "none" when hidden.
    return getComputedStyle(el).display !== "none";
  }

  function firstField(drawer) {
    var fields = drawer.querySelectorAll(FOCUSABLE);
    for (var i = 0; i < fields.length; i++) {
      var f = fields[i];
      // Skip the close button so focus lands on a real field, not the X.
      if (f.classList && f.classList.contains("console-slideover-close")) continue;
      return f;
    }
    // Fall back to the heading (made focusable via tabindex="-1" in markup).
    return drawer.querySelector("h1, h2, h3, [role='heading']") || drawer;
  }

  function manage(drawer) {
    // Idempotency guard: a second setup pass (e.g. from a future dynamic
    // drawer insertion) must not attach a second observer to the same element.
    if (drawer.__a11yManaged) return;
    drawer.__a11yManaged = true;

    var wasOpen = isOpen(drawer);
    if (wasOpen) focusInto(drawer);

    // The observer intentionally lives for the page lifetime: console
    // navigation is full-page (no SPA body-swap), so observers are
    // naturally torn down with the document. As cheap insurance we also
    // disconnect on pagehide (bfcache eviction, mobile Safari unload).
    // The pagehide listener is intentionally never removed: it is bounded
    // O(drawers) by the __a11yManaged guard and dies with the document.
    var obs = new MutationObserver(function () {
      var nowOpen = isOpen(drawer);
      if (nowOpen === wasOpen) return;
      wasOpen = nowOpen;
      if (nowOpen) {
        focusInto(drawer);
      } else if (drawer.__restoreFocus) {
        try {
          drawer.__restoreFocus.focus();
        } catch (_) {}
        drawer.__restoreFocus = null;
      }
    });
    obs.observe(drawer, {
      attributes: true,
      attributeFilter: ["style", "hidden", "class"],
    });
    window.addEventListener("pagehide", function () {
      obs.disconnect();
    });
  }

  function focusInto(drawer) {
    var active = document.activeElement;
    if (active && !drawer.contains(active)) {
      drawer.__restoreFocus = active;
    }
    var target = firstField(drawer);
    // Defer so Datastar has finished its DOM apply for this tick.
    // Re-check the drawer is still open before moving focus: if the drawer
    // was closed before the frame fires (e.g. rapid open/close), skip the
    // focus move to avoid stealing focus from the now-visible page.
    requestAnimationFrame(function () {
      if (!isOpen(drawer)) return;
      try {
        target.focus();
      } catch (_) {}
    });
  }

  function initDrawers() {
    var drawers = document.querySelectorAll(
      '.console-slideover[aria-modal="true"]',
    );
    for (var i = 0; i < drawers.length; i++) manage(drawers[i]);
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", initDrawers);
  } else {
    initDrawers();
  }
})();
