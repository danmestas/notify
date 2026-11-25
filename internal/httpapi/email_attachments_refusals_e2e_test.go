package httpapi_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/blobstore"
	"github.com/danmestas/notify/internal/domain"
)

func (e *attachmentEnv) seedWorkflow(t *testing.T, identifier string, stepTypes ...string) {
	t.Helper()
	var steps []domain.Step
	for i, ty := range stepTypes {
		steps = append(steps, domain.Step{StepID: string(rune('A' + i)), Type: ty, ControlValues: json.RawMessage(`{"subject":"s","body":"b"}`)})
	}
	raw, _ := json.Marshal(steps)
	if _, err := e.st.Workflows().Create(context.Background(), e.scope, domain.Workflow{
		ID: "wf-" + identifier, Name: identifier, TriggerIdentifier: identifier, Active: true, Steps: raw,
	}); err != nil {
		t.Fatal(err)
	}
}

func (e *attachmentEnv) stored(t *testing.T, content []byte) bool {
	t.Helper()
	sum := sha256.Sum256(content)
	_, err := e.blobs.Head(context.Background(), hex.EncodeToString(sum[:]))
	if err != nil && err != blobstore.ErrNotFound {
		t.Fatal(err)
	}
	return err == nil
}

// TestEmailAttachments_RefusedTriggersStoreNothing: every refusal comes before
// the bytes reach the blob store, so a refused trigger leaves no orphan blob.
func TestEmailAttachments_RefusedTriggersStoreNothing(t *testing.T) {
	e := newAttachmentEnv(t, false, true)
	e.seedWorkflow(t, "plain", "in_app")
	e.seedWorkflow(t, "digested", "digest", "email")
	e.seedWorkflow(t, "mail", "email")
	content := []byte("%PDF refused trigger content")
	atts := "[" + attJSON("a.pdf", "application/pdf", content) + "]"

	for name, c := range map[string]struct {
		body   string
		status int
		want   string
	}{
		"no email step": {`{"name":"plain","to":{"subscriberId":"user_9"},"attachments":` + atts + `}`, http.StatusUnprocessableEntity, "only sent by email steps"},
		"digest first":  {`{"name":"digested","to":{"subscriberId":"user_9"},"attachments":` + atts + `}`, http.StatusUnprocessableEntity, "digest step"},
		"unknown topic": {`{"name":"mail","to":{"topicKey":"nobody"},"attachments":` + atts + `}`, http.StatusNotFound, "topic not found"},
	} {
		t.Run(name, func(t *testing.T) {
			status, body := doPost(t, e.srv.URL+"/v1/events/trigger", e.auth, c.body)
			if status != c.status || !strings.Contains(body, c.want) {
				t.Fatalf("status = %d body = %s, want %d mentioning %q", status, body, c.status, c.want)
			}
			if e.stored(t, content) {
				t.Fatal("a refused trigger left its attachment in the blob store")
			}
		})
	}
}

// TestEmailAttachments_InboundRouteCapsAndRefuses: POST /v1/inbound/{trigger}
// takes the same body cap (413) and answers a refused attachment with 422,
// where its other refusals answer 201.
func TestEmailAttachments_InboundRouteCapsAndRefuses(t *testing.T) {
	e := newAttachmentEnv(t, false, true)
	e.seedWorkflow(t, "plain", "in_app")

	huge := `{"to":{"subscriberId":"user_9"},"payload":{"x":"` + strings.Repeat("a", 16<<20) + `"}}`
	if status, _ := doPost(t, e.srv.URL+"/v1/inbound/plain", e.auth, huge); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: status = %d, want 413", status)
	}
	bad := `{"to":{"subscriberId":"user_9"},"attachments":[` + attJSON("a.exe", "application/x-msdownload", []byte("x")) + `]}`
	if status, body := doPost(t, e.srv.URL+"/v1/inbound/plain", e.auth, bad); status != http.StatusUnprocessableEntity || !strings.Contains(body, "not allowed") {
		t.Fatalf("bad attachment: status = %d body = %s, want 422", status, body)
	}
	// The other refusals keep answering 201 on this route.
	if status, _ := doPost(t, e.srv.URL+"/v1/inbound/missing", e.auth, `{"to":{"subscriberId":"user_9"}}`); status != http.StatusCreated {
		t.Fatalf("unknown workflow: status = %d, want 201", status)
	}
}
