package console

// inbound_email.go implements the read-only /console/inbound-email page: a
// configuration banner (webhook URL, endpoint secret status, verified-domain
// summary) plus an activity log of received inbound email threads.
//
// All data access is in-process and scope-filtered via Deps.EmailThreads and
// Deps.InboundDomains. The page is read-only — no mutation routes, no CSRF
// surface beyond the shared logout form. The endpoint secret value is never
// rendered; only its configured/not-configured status.

import (
	"net/http"
	"strings"

	"github.com/danmestas/notify/internal/domain"
)

type inboundEmailRow struct {
	SubscriberEmail string
	AgentIdentifier string
	InboundDomain   string
	LastActivityAt  string
	CreatedAt       string
}

type inboundEmailListView struct {
	Rows []inboundEmailRow
}

type inboundEmailPageView struct {
	Header           pageHeader
	WebhookURL       string
	PublicBaseURLSet bool
	SecretConfigured bool
	VerifiedDomains  int
	TotalDomains     int
	Fragment         inboundEmailListView
}

func (c *Console) buildInboundEmailListView(r *http.Request) (inboundEmailListView, error) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("buildInboundEmailListView: no scope in context")
	}
	items, _, _, err := c.deps.EmailThreads.List(r.Context(), scope, "", 50)
	if err != nil {
		return inboundEmailListView{}, err
	}
	rows := make([]inboundEmailRow, 0, len(items))
	for _, it := range items {
		rows = append(rows, inboundEmailRow{
			SubscriberEmail: it.SubscriberEmail,
			AgentIdentifier: it.AgentIdentifier,
			InboundDomain:   it.InboundDomain,
			LastActivityAt:  it.LastActivityAt,
			CreatedAt:       it.CreatedAt,
		})
	}
	return inboundEmailListView{Rows: rows}, nil
}

func (c *Console) serveInboundEmailPage(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveInboundEmailPage: w is nil")
	}
	if r == nil {
		panic("serveInboundEmailPage: r is nil")
	}
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("serveInboundEmailPage: no scope in context")
	}
	frag, err := c.buildInboundEmailListView(r)
	if err != nil {
		c.deps.Logger.Error("console: inbound email page list", "err", err)
		c.serveError(w, r)
		return
	}

	webhookURL := "{base_url}/v1/inbound/email"
	publicBaseURLSet := false
	if base := strings.TrimRight(c.deps.PublicBaseURL, "/"); base != "" {
		webhookURL = base + "/v1/inbound/email"
		publicBaseURLSet = true
	}

	verified := 0
	total := 0
	if doms, _, t, err := c.deps.InboundDomains.List(r.Context(), scope, "", "", 200); err == nil {
		total = t
		for _, d := range doms {
			if d.Status == "verified" {
				verified++
			}
		}
	} else {
		c.deps.Logger.Error("console: inbound email domain summary", "err", err)
	}

	header := pageHeader{
		Title:    "Inbound Email",
		Subtitle: "Received inbound email threads and the endpoint that accepts them.",
		Tiles: []tile{
			{Label: "threads", Count: len(frag.Rows), Tone: toneDefault},
			{Label: "verified domains", Count: verified, Tone: toneSuccess},
		},
	}
	if err := header.validate(); err != nil {
		header = pageHeader{Title: "Inbound Email"}
	}

	view := inboundEmailPageView{
		Header:           header,
		WebhookURL:       webhookURL,
		PublicBaseURLSet: publicBaseURLSet,
		SecretConfigured: c.deps.InboundSecretConfigured,
		VerifiedDomains:  verified,
		TotalDomains:     total,
		Fragment:         frag,
	}
	c.renderPage(w, r, http.StatusOK, "inbound-email", "Inbound Email", view)
}

func (c *Console) serveFragmentInboundEmailList(w http.ResponseWriter, r *http.Request) {
	if w == nil {
		panic("serveFragmentInboundEmailList: w is nil")
	}
	if r == nil {
		panic("serveFragmentInboundEmailList: r is nil")
	}
	if _, ok := domain.ScopeFromContext(r.Context()); !ok {
		panic("serveFragmentInboundEmailList: no scope in context")
	}
	view, err := c.buildInboundEmailListView(r)
	if err != nil {
		c.deps.Logger.Error("console: inbound email fragment", "err", err)
		http.Error(w, "list failed", http.StatusInternalServerError)
		return
	}
	html, err := renderFragment(c.ts.base, "inbound-email-tbody", view)
	if err != nil {
		c.deps.Logger.Error("console: render inbound email fragment", "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	c.patchHTML(w, r, html)
}
