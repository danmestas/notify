package steptype_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/secretbox"
	"github.com/danmestas/notify/internal/steptype"
)

func testBox(t *testing.T) *secretbox.Box {
	t.Helper()
	b, err := secretbox.New(bytes.Repeat([]byte{0x2a}, 32))
	if err != nil {
		t.Fatalf("secretbox.New: %v", err)
	}
	return b
}

// httpStep builds an http_request step whose headers hold the given value.
func httpStep(headers string) domain.Step {
	cv, _ := json.Marshal(map[string]any{
		"url": "https://example.com", "method": "POST",
		"headers": headers, "body": "", "timeoutSec": 30, "failurePolicy": "fail_closed",
	})
	return domain.Step{StepID: "s1", Type: "http_request", ControlValues: cv}
}

func headersOf(t *testing.T, step domain.Step) string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(step.ControlValues, &m); err != nil {
		t.Fatalf("unmarshal cv: %v", err)
	}
	var s string
	_ = json.Unmarshal(m["headers"], &s)
	return s
}

// TestEncryptSecrets_SealsHeaderValue is the core: after encryption the stored
// headers must not contain the plaintext secret.
func TestEncryptSecrets_SealsHeaderValue(t *testing.T) {
	r := steptype.Default()
	box := testBox(t)
	const secret = `{"Authorization":"Bearer sk-live-xyz"}`

	out, err := r.EncryptSecrets(box, httpStep(secret), domain.Step{})
	if err != nil {
		t.Fatalf("EncryptSecrets: %v", err)
	}
	stored := headersOf(t, out)
	if strings.Contains(stored, "sk-live-xyz") {
		t.Fatalf("stored headers still contain the plaintext secret: %q", stored)
	}
	if !secretbox.IsSealed(stored) {
		t.Fatalf("stored headers are not sealed: %q", stored)
	}
}

// TestDecryptSecrets_RoundTrip proves the run-time read recovers the plaintext.
func TestDecryptSecrets_RoundTrip(t *testing.T) {
	r := steptype.Default()
	box := testBox(t)
	const secret = `{"Authorization":"Bearer round-trip"}`

	sealed, err := r.EncryptSecrets(box, httpStep(secret), domain.Step{})
	if err != nil {
		t.Fatalf("EncryptSecrets: %v", err)
	}
	opened, err := r.DecryptSecrets(box, sealed)
	if err != nil {
		t.Fatalf("DecryptSecrets: %v", err)
	}
	if got := headersOf(t, opened); got != secret {
		t.Errorf("round trip headers = %q, want %q", got, secret)
	}
}

// TestEncryptSecrets_NoDoubleEncrypt pins idempotency: re-saving an
// already-sealed step must not seal it again (which would make it un-openable
// in one pass and corrupt on the next).
func TestEncryptSecrets_NoDoubleEncrypt(t *testing.T) {
	r := steptype.Default()
	box := testBox(t)

	once, err := r.EncryptSecrets(box, httpStep(`{"k":"v"}`), domain.Step{})
	if err != nil {
		t.Fatalf("first encrypt: %v", err)
	}
	twice, err := r.EncryptSecrets(box, once, once)
	if err != nil {
		t.Fatalf("second encrypt: %v", err)
	}
	if headersOf(t, once) != headersOf(t, twice) {
		t.Fatal("re-encrypting an already-sealed step changed the value: double-encrypted")
	}
	// And it still opens to the original in a single pass.
	opened, _ := r.DecryptSecrets(box, twice)
	if got := headersOf(t, opened); got != `{"k":"v"}` {
		t.Errorf("double-encrypt corrupted the value: opened to %q", got)
	}
}

// TestEncryptSecrets_MaskPreservesExisting covers the editor's unchanged-secret
// path: a save carrying SecretMask must keep the prior stored ciphertext, not
// seal the mask.
func TestEncryptSecrets_MaskPreservesExisting(t *testing.T) {
	r := steptype.Default()
	box := testBox(t)
	const secret = `{"Authorization":"Bearer keep-me"}`

	prior, err := r.EncryptSecrets(box, httpStep(secret), domain.Step{})
	if err != nil {
		t.Fatalf("seed prior: %v", err)
	}

	// Operator re-saves without retyping: headers come back as the mask.
	resaved, err := r.EncryptSecrets(box, httpStep(steptype.SecretMask), prior)
	if err != nil {
		t.Fatalf("EncryptSecrets with mask: %v", err)
	}
	if headersOf(t, resaved) != headersOf(t, prior) {
		t.Fatal("saving with the mask did not preserve the prior stored secret")
	}
	opened, _ := r.DecryptSecrets(box, resaved)
	if got := headersOf(t, opened); got != secret {
		t.Errorf("preserved secret opened to %q, want %q", got, secret)
	}
}

// TestDecryptSecrets_LegacyPlaintext keeps steps stored before encryption
// working: an unsealed headers value passes through unchanged.
func TestDecryptSecrets_LegacyPlaintext(t *testing.T) {
	r := steptype.Default()
	box := testBox(t)
	const legacy = `{"Authorization":"Bearer legacy-plaintext"}`

	out, err := r.DecryptSecrets(box, httpStep(legacy))
	if err != nil {
		t.Fatalf("DecryptSecrets(legacy): %v", err)
	}
	if got := headersOf(t, out); got != legacy {
		t.Errorf("legacy plaintext = %q, want unchanged", got)
	}
}

// TestControlMap_MasksHeaders proves the display path never returns the stored
// secret to the console.
func TestControlMap_MasksHeaders(t *testing.T) {
	r := steptype.Default()
	box := testBox(t)

	sealed, err := r.EncryptSecrets(box, httpStep(`{"Authorization":"Bearer visible?"}`), domain.Step{})
	if err != nil {
		t.Fatalf("EncryptSecrets: %v", err)
	}
	flat := r.Get("http_request").ControlMap(sealed.ControlValues)
	if flat["headers"] != steptype.SecretMask {
		t.Fatalf("ControlMap headers = %q, want the mask", flat["headers"])
	}
	if strings.Contains(flat["headers"], "visible?") || secretbox.IsSealed(flat["headers"]) {
		t.Fatalf("ControlMap leaked the stored value: %q", flat["headers"])
	}
}

// TestSecretFields_OnlyHTTPDeclares confirms the optional interface is opt-in:
// a type with no secrets returns none, so the generic layer is a no-op for it.
func TestSecretFields_OnlyHTTPDeclares(t *testing.T) {
	r := steptype.Default()
	if got := r.SecretFields("http_request"); len(got) != 1 || got[0] != "headers" {
		t.Errorf("http_request SecretFields = %v, want [headers]", got)
	}
	if got := r.SecretFields("in_app"); len(got) != 0 {
		t.Errorf("in_app SecretFields = %v, want none", got)
	}
}
