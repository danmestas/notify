// Package console implements the notify operator console: a server-rendered
// Datastar UI mounted at /console on the same :3000 listener as the public
// API. It is one deep module — the only exported surface is New (construct
// from Deps) and Console.Routes (an http.Handler) — everything else
// (sessions, CSRF, fragments, pages) stays internal.
//
// Conventions every later console slice builds on:
//
//   - Pages are full layout renders under /console/*; each page template
//     defines `content` over the shared `layout` tree (dagnats idiom).
//   - Fragment endpoints live under /console/api/fragments/* and return
//     exactly one Datastar PatchElements SSE event swapping a stable
//     element id (the fragment template's outer element carries the id).
//     Filter state arrives as Datastar signals merged with the URL query
//     via mergeFragmentParams — URL params win.
//   - Mutations are POST/PUT/PATCH/DELETE under /console/api/* (and the
//     auth endpoints), CSRF-checked against the session token, and respond
//     with toast-lift JSON ({"toast": "..."}) or PatchElements.
//   - All data access goes through Scope-carrying services (identity,
//     authoring, analytics, ...) in-process. No loopback HTTP, and no
//     console-side queries that bypass Scope.
//   - Deps.ReadOnly is template-enforcement only (disabled buttons +
//     banner); every future mutation handler must also check
//     Deps.ReadOnly server-side before touching state.
//   - Assets are vendored and embedded; the browser makes zero outbound
//     requests at runtime.
package console

import "embed"

// assetsFS holds the vendored browser assets. datastar.js.gz is the
// upstream Datastar ES-module bundle (see assets/README.md for the pinned
// version and refresh procedure), served with Content-Encoding: gzip.
// app.css is small enough to ship uncompressed.
//
//go:embed assets/datastar.js.gz
//go:embed assets/app.css
//go:embed assets/theme.js
var assetsFS embed.FS

// templatesFS carries every Go html/template file the console renders.
// The layout and shared partials form the base tree; each page file owns
// one `content` overlay (see loadTemplates in layout.go).
//
//go:embed templates/layout.html
//go:embed templates/login.html
//go:embed templates/dashboard.html
//go:embed templates/analytics.html
//go:embed templates/activity.html
//go:embed templates/not_found.html
//go:embed templates/error.html
//go:embed templates/workflows.html
//go:embed templates/workflow_detail.html
//go:embed templates/partials/page_header.html
//go:embed templates/partials/empty_state.html
//go:embed templates/partials/toast.html
//go:embed templates/partials/dashboard_kpis.html
//go:embed templates/partials/workflows_list.html
//go:embed templates/partials/workflow_step_editor.html
//go:embed templates/partials/analytics_kpis.html
//go:embed templates/partials/analytics_charts.html
//go:embed templates/partials/activity_tbody.html
//go:embed templates/partials/activity_detail_panel.html
//go:embed templates/integrations.html
//go:embed templates/partials/integrations_grid.html
//go:embed templates/partials/integration_detail.html
//go:embed templates/partials/integration_provider_form.html
//go:embed templates/subscribers.html
//go:embed templates/subscriber_detail.html
//go:embed templates/topics.html
//go:embed templates/topic_detail.html
//go:embed templates/partials/subscribers_tbody.html
//go:embed templates/partials/topics_tbody.html
//go:embed templates/partials/subscriber_autocomplete.html
//go:embed templates/webhooks.html
//go:embed templates/partials/webhooks_tbody.html
var templatesFS embed.FS
