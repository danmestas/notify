package console

import (
	"errors"
	"net/http"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/phoneverify"
)

// smsConsentView binds the "Text customers who opted in at your shop" card on
// the Integrations page: one switch over the environment's operator-attestation
// setting and its key's permission to attest.
type smsConsentView struct {
	// On: the setting is on and the key may attest. A half-set state (the
	// CLI can set the two apart) reads as off, and saving fixes it.
	On        bool
	PolicyURL string
	// Error is the plain-words refusal of the last save, if any.
	Error string
	// Saved confirms the last save.
	Saved bool
}

const smsConsentNeedsURL = "Add a link to your privacy policy that starts with https://"

// smsConsentFor reads the card's state; the error param is the page's
// ?smsConsentError=1 and ?smsConsentSaved=1 round trip.
func (c *Console) smsConsentFor(r *http.Request, scope domain.Scope) (*smsConsentView, error) {
	settings, err := c.deps.Phone.ConsentSettings(r.Context(), scope)
	if err != nil {
		return nil, err
	}
	allowed, err := c.deps.Identity.AttestSMSConsentAllowed(r.Context(), scope)
	if err != nil {
		return nil, err
	}
	v := &smsConsentView{On: settings.Enabled && allowed, PolicyURL: settings.PolicyURL}
	if r.URL.Query().Get("smsConsentError") == "1" {
		v.Error = smsConsentNeedsURL
	}
	v.Saved = r.URL.Query().Get("smsConsentSaved") == "1"
	return v, nil
}

// handleSMSConsentSwitch handles POST /console/integrations/sms-consent: the
// one switch. On sets the environment setting (with the https policy link)
// and the key permission together; off clears both. A bad link redirects back
// with smsConsentError=1 and changes nothing. Whoever can log in to the
// console decides this for the shop; the HTTP API cannot change it.
func (c *Console) handleSMSConsentSwitch(w http.ResponseWriter, r *http.Request) {
	scope, ok := domain.ScopeFromContext(r.Context())
	if !ok {
		panic("handleSMSConsentSwitch: no scope in context")
	}
	if c.deps.ReadOnly {
		http.Error(w, "console is read-only", http.StatusForbidden)
		return
	}
	var err error
	if r.FormValue("enabled") == "on" {
		err = c.deps.Phone.EnableOperatorConsent(r.Context(), scope, r.FormValue("policy_url"))
	} else {
		err = c.deps.Phone.DisableOperatorConsent(r.Context(), scope)
	}
	if errors.Is(err, phoneverify.ErrPolicyURLRequired) {
		c.redirect(w, r, "/console/integrations?smsConsentError=1#sms-consent", http.StatusSeeOther)
		return
	}
	if err != nil {
		c.deps.Logger.Error("console: save sms consent switch", "err", err)
		http.Error(w, "save failed", http.StatusInternalServerError)
		return
	}
	c.redirect(w, r, "/console/integrations?smsConsentSaved=1#sms-consent", http.StatusSeeOther)
}
