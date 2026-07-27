package steptype

import (
	"encoding/json"
	"fmt"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/secretbox"
)

// SecretFielder is optionally implemented by step types whose control values
// include secret fields that must be encrypted at rest (e.g. http_request's
// request headers, which routinely carry bearer tokens and API keys).
//
// It is an optional interface, type-asserted by the encrypt/decrypt layer,
// precisely so the other step types implement nothing: only a type with a
// genuine secret declares one.
type SecretFielder interface {
	// SecretFields names the control-value keys whose values are secret.
	SecretFields() []string
}

// SecretMask stands in for a stored secret when a step is rendered for editing,
// so the console never returns the real value to the browser. A save that comes
// back still carrying the mask means "unchanged" -- see EncryptSecrets.
const SecretMask = "••••••"

// SecretFields returns the declared secret control-value keys for the named
// step type, or nil if the type has no secrets.
func (r *Registry) SecretFields(typeName string) []string {
	if sf, ok := r.Get(typeName).(SecretFielder); ok {
		return sf.SecretFields()
	}
	return nil
}

// EncryptSecrets seals the declared secret fields of step's control values.
//
// It is the single write-side chokepoint: authoring calls it before persisting
// a step, so a secret is encrypted regardless of which mutation stored it.
//
//   - Idempotent: a field already sealed is left alone, so re-saving a step does
//     not double-encrypt.
//   - Unchanged secrets: a field whose submitted value is SecretMask means the
//     operator edited the step without retyping the secret; the value stored on
//     prior is preserved instead of sealing the mask. prior may be a zero Step
//     (e.g. on create), in which case a masked field resolves to empty.
//
// Steps of a type with no secret fields are returned unchanged.
func (r *Registry) EncryptSecrets(box *secretbox.Box, step, prior domain.Step) (domain.Step, error) {
	fields := r.SecretFields(step.Type)
	if len(fields) == 0 || box == nil {
		return step, nil
	}

	cur, err := decodeControlMap(step.ControlValues)
	if err != nil {
		return step, fmt.Errorf("steptype: encrypt secrets: %w", err)
	}
	priorMap, _ := decodeControlMap(prior.ControlValues) // best-effort; nil on zero prior

	for _, f := range fields {
		val, ok := stringField(cur, f)
		if !ok {
			continue
		}
		switch {
		case val == SecretMask:
			// Unchanged in the editor: keep whatever was stored before.
			if pv, ok := stringField(priorMap, f); ok {
				cur[f] = jsonString(pv)
			} else {
				cur[f] = jsonString("")
			}
		case secretbox.IsSealed(val):
			// Already encrypted; leave it.
		default:
			sealed, err := box.Seal(val)
			if err != nil {
				return step, fmt.Errorf("steptype: seal %q: %w", f, err)
			}
			cur[f] = jsonString(sealed)
		}
	}

	out, err := json.Marshal(cur)
	if err != nil {
		return step, fmt.Errorf("steptype: encrypt secrets marshal: %w", err)
	}
	step.ControlValues = out
	return step, nil
}

// DecryptSecrets opens the declared secret fields of step's control values. It
// is the read-side chokepoint the run-time handler calls before executing a
// step. A legacy plaintext value passes through unchanged (see secretbox.Open).
func (r *Registry) DecryptSecrets(box *secretbox.Box, step domain.Step) (domain.Step, error) {
	fields := r.SecretFields(step.Type)
	if len(fields) == 0 || box == nil {
		return step, nil
	}

	cur, err := decodeControlMap(step.ControlValues)
	if err != nil {
		return step, fmt.Errorf("steptype: decrypt secrets: %w", err)
	}
	for _, f := range fields {
		val, ok := stringField(cur, f)
		if !ok {
			continue
		}
		plain, err := box.Open(val)
		if err != nil {
			return step, fmt.Errorf("steptype: open %q: %w", f, err)
		}
		cur[f] = jsonString(plain)
	}

	out, err := json.Marshal(cur)
	if err != nil {
		return step, fmt.Errorf("steptype: decrypt secrets marshal: %w", err)
	}
	step.ControlValues = out
	return step, nil
}

// maskControlValues replaces the named secret fields with SecretMask for
// display. A field that is empty or absent is left as-is, so the editor shows a
// blank rather than masking nothing. Cipher-free: masking is display logic the
// step type owns; only encryption needs the key.
func maskControlValues(cv json.RawMessage, fields []string) map[string]string {
	flat := flattenControlValues(cv)
	for _, f := range fields {
		if flat[f] != "" {
			flat[f] = SecretMask
		}
	}
	return flat
}

// decodeControlMap unmarshals control values into a field->raw map, preserving
// every non-secret field's bytes exactly.
func decodeControlMap(cv json.RawMessage) (map[string]json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if len(cv) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(cv, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// stringField reads a control field that holds a JSON string.
func stringField(m map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := m[key]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// jsonString marshals s as a JSON string value; the input is developer-supplied
// and never fails to marshal.
func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
