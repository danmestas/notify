package steptype

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
)

// in_app rich-field unpacking (ParseForm) and pre-fill flattening (ControlMap).
// The controlValues JSON mirrors the reference inAppOutputSchema field names so the
// persisted message extras blob — and therefore the inbox API output — is
// wire-compatible at the field level:
//
//	{subject?, body?, avatar?,
//	 primaryAction?:{label, redirect?:{url, target?}},
//	 secondaryAction?:{label, redirect?:{url, target?}},
//	 redirect?:{url, target?},
//	 data?:{...}}

// ParseForm builds the in_app controlValues from the editor form. Subject and
// body are always written; each rich field is added only when present so a step
// with no rich fields keeps a compact blob. A redirect sub-object is dropped
// entirely when its url is blank (the reference platform redirectSchema requires url), and a CTA is
// dropped when its label is blank.
func (inApp) ParseForm(form url.Values) (json.RawMessage, error) {
	m := map[string]any{
		"subject": formField(form, "subject"),
		"body":    formField(form, "body"),
	}
	if v := formField(form, "avatar"); v != "" {
		m["avatar"] = v
	}
	if a := inAppActionFromForm(form, "primary_action_label", "primary_action_redirect_url", "primary_action_redirect_target"); a != nil {
		m["primaryAction"] = a
	}
	if a := inAppActionFromForm(form, "secondary_action_label", "secondary_action_redirect_url", "secondary_action_redirect_target"); a != nil {
		m["secondaryAction"] = a
	}
	if rd := inAppRedirectFromForm(formField(form, "redirect_url"), formField(form, "redirect_target")); rd != nil {
		m["redirect"] = rd
	}
	if dj := formField(form, "data_json"); dj != "" {
		data, err := parseDataObject(dj)
		if err != nil {
			return nil, err
		}
		m["data"] = data
	}
	return marshalControlValues(m)
}

// inAppRedirectFromForm builds a redirect sub-object, returning nil (omit the
// whole object) when url is blank — even if target is set.
func inAppRedirectFromForm(url, target string) map[string]any {
	if url == "" {
		return nil
	}
	r := map[string]any{"url": url}
	if target != "" {
		r["target"] = target
	}
	return r
}

// inAppActionFromForm builds a CTA sub-object, returning nil when the label is
// blank. Its redirect is omitted when the redirect url is blank.
func inAppActionFromForm(form url.Values, labelKey, urlKey, targetKey string) map[string]any {
	label := formField(form, labelKey)
	if label == "" {
		return nil
	}
	a := map[string]any{"label": label}
	if rd := inAppRedirectFromForm(formField(form, urlKey), formField(form, targetKey)); rd != nil {
		a["redirect"] = rd
	}
	return a
}

// parseDataObject validates that s is a JSON object (free-form values allowed,
// unlike push's string-only data_json) and returns it compacted. A non-object or
// invalid JSON is a user-visible error.
func parseDataObject(s string) (json.RawMessage, error) {
	var probe any
	if err := json.Unmarshal([]byte(s), &probe); err != nil {
		return nil, errors.New("data must be valid JSON")
	}
	if _, ok := probe.(map[string]any); !ok {
		return nil, errors.New("data must be a JSON object")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		return nil, errors.New("data must be valid JSON")
	}
	return json.RawMessage(append([]byte(nil), buf.Bytes()...)), nil
}

// inAppRedirectCV / inAppActionCV decode the nested controlValues sub-objects for
// ControlMap pre-fill (local mirrors of runenv's types; steptype stays free of
// the delivery/runenv import).
type inAppRedirectCV struct {
	URL    string `json:"url"`
	Target string `json:"target"`
}

type inAppActionCV struct {
	Label    string           `json:"label"`
	Redirect *inAppRedirectCV `json:"redirect"`
}

// ControlMap flattens the in_app controlValues for editor pre-fill. Scalar keys
// (subject, body, avatar) ride the shared flattener; the nested rich objects are
// expanded with PREFIX-EXPLICIT dot-notation keys (primaryAction.label,
// primaryAction.redirect.url, redirect.target, ...) the editor template reads via
// `index .ControlMap "primaryAction.label"`. Only the known in_app prefixes are
// expanded, so no other key gains dotted sub-keys.
func (inApp) ControlMap(cv json.RawMessage) map[string]string {
	out := flattenControlValues(cv)
	if len(cv) == 0 {
		return out
	}
	var rich struct {
		PrimaryAction   *inAppActionCV   `json:"primaryAction"`
		SecondaryAction *inAppActionCV   `json:"secondaryAction"`
		Redirect        *inAppRedirectCV `json:"redirect"`
		Data            json.RawMessage  `json:"data"`
	}
	if err := json.Unmarshal(cv, &rich); err != nil {
		return out
	}
	putInAppAction(out, "primaryAction", rich.PrimaryAction)
	putInAppAction(out, "secondaryAction", rich.SecondaryAction)
	putInAppRedirect(out, "redirect", rich.Redirect)
	if len(rich.Data) > 0 {
		out["data"] = string(rich.Data)
	}
	return out
}

func putInAppRedirect(out map[string]string, prefix string, r *inAppRedirectCV) {
	if r == nil {
		return
	}
	out[prefix+".url"] = r.URL
	if r.Target != "" {
		out[prefix+".target"] = r.Target
	}
}

func putInAppAction(out map[string]string, prefix string, a *inAppActionCV) {
	if a == nil {
		return
	}
	out[prefix+".label"] = a.Label
	putInAppRedirect(out, prefix+".redirect", a.Redirect)
}
