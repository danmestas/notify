package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/blobstore"
	"github.com/danmestas/notify/internal/domain"
	"github.com/danmestas/notify/internal/engine"
	"github.com/danmestas/notify/internal/providers"
)

func runEmail(t *testing.T, h *EmailHandler) domain.Message {
	t.Helper()
	input, err := json.Marshal(emailEnvelope())
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.HandleEmail(context.Background(), engine.TaskPayload{RunID: "run_1", StepID: "step_1", Input: input})
	if err != nil {
		t.Fatalf("HandleEmail: %v", err)
	}
	var m domain.Message
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func putAttachment(t *testing.T, blobs blobstore.Store, content []byte) blobstore.Ref {
	t.Helper()
	ref, err := blobs.Put(context.Background(), bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// TestHandleEmail_SendsAttachmentsAndRetriesResendThem: the notification's
// attachment rows are read back from the blob store and sent; running the step
// again (a retry, or a second email step of the run) sends the same bytes.
func TestHandleEmail_SendsAttachmentsAndRetriesResendThem(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	blobs, err := blobstore.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pdf := []byte("%PDF-1.4 invoice \x00\xff")
	ref := putAttachment(t, blobs, pdf)
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	if err := s.EmailAttachments().Add(ctx, scope, "ntf_1", "user_1", []domain.EmailAttachment{
		{Filename: "invoice-1042.pdf", ContentType: "application/pdf", SHA256: ref.SHA256, Size: ref.Size},
	}); err != nil {
		t.Fatal(err)
	}
	cap := providers.NewCapture()
	h := NewEmail(s, stubResolver{provider: cap, from: "noreply@acme.io"}).WithBlobStore(blobs)

	for i := 0; i < 2; i++ {
		if m := runEmail(t, h); m.Status != "sent" {
			t.Fatalf("run %d: status = %q (%s)", i, m.Status, m.ErrorText)
		}
	}
	sent := cap.Sent()
	if len(sent) != 2 {
		t.Fatalf("sent %d emails, want 2", len(sent))
	}
	for i, em := range sent {
		if len(em.Attachments) != 1 {
			t.Fatalf("email %d: %d attachments, want 1", i, len(em.Attachments))
		}
		a := em.Attachments[0]
		if a.Filename != "invoice-1042.pdf" || a.ContentType != "application/pdf" || !bytes.Equal(a.Content, pdf) {
			t.Fatalf("email %d: attachment = %q %q %q", i, a.Filename, a.ContentType, a.Content)
		}
	}
}

// TestHandleEmail_AttachmentsUnreadableFailsTheSend: a missing blob, or no blob
// store at all, records an error message and sends nothing, rather than
// sending the email without its file.
func TestHandleEmail_AttachmentsUnreadableFailsTheSend(t *testing.T) {
	ctx := context.Background()
	scope := domain.Scope{OrgID: "org_1", EnvID: "env_1"}
	missing := domain.EmailAttachment{Filename: "a.pdf", ContentType: "application/pdf", SHA256: strings.Repeat("ab", 32), Size: 3}

	for name, mk := range map[string]func(t *testing.T, h *EmailHandler) *EmailHandler{
		"blob gone": func(t *testing.T, h *EmailHandler) *EmailHandler {
			blobs, err := blobstore.NewFS(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			return h.WithBlobStore(blobs)
		},
		"no blob store": func(_ *testing.T, h *EmailHandler) *EmailHandler { return h },
	} {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			if err := s.EmailAttachments().Add(ctx, scope, "ntf_1", "user_1", []domain.EmailAttachment{missing}); err != nil {
				t.Fatal(err)
			}
			cap := providers.NewCapture()
			h := mk(t, NewEmail(s, stubResolver{provider: cap, from: "noreply@acme.io"}))
			m := runEmail(t, h)
			if m.Status != "error" || m.ErrorClass == "" {
				t.Fatalf("message = %+v, want a classified error", m)
			}
			if len(cap.Sent()) != 0 {
				t.Fatal("an email was sent without its attachment")
			}
		})
	}
}

// TestHandleEmail_NoAttachmentsSendsNone: a notification without attachment
// rows sends exactly what it did before attachments existed.
func TestHandleEmail_NoAttachmentsSendsNone(t *testing.T) {
	s := newStore(t)
	cap := providers.NewCapture()
	blobs, err := blobstore.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if m := runEmail(t, NewEmail(s, stubResolver{provider: cap, from: "noreply@acme.io"}).WithBlobStore(blobs)); m.Status != "sent" {
		t.Fatalf("status = %q", m.Status)
	}
	if got := cap.Sent(); len(got) != 1 || got[0].Attachments != nil {
		t.Fatalf("sent = %+v", got)
	}
}
