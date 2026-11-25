package webhook

import (
	"context"
	"net/http"
	"testing"

	"github.com/danmestas/notify/internal/providers"
)

// TestEmailClient_Send_AttachmentsAreRefused: the webhook body has no place
// for a file, so the send fails (rejected_message) instead of dropping it, and
// nothing is posted.
func TestEmailClient_Send_AttachmentsAreRefused(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{}`)
	c := &EmailClient{URL: srv.URL}
	_, err := c.Send(context.Background(), providers.EmailMessage{
		To: "u@example.com", Subject: "s", Text: "t",
		Attachments: []providers.Attachment{{Filename: "a.pdf", ContentType: "application/pdf", Content: []byte("x")}},
	})
	if err == nil {
		t.Fatal("send with attachments succeeded")
	}
	if c := providers.ClassifyError(err); c != providers.ClassRejectedMessage {
		t.Fatalf("class = %q, want %q (%v)", c, providers.ClassRejectedMessage, err)
	}
	if providers.IsTransient(err) {
		t.Fatal("a refusal must not be retried")
	}
	if got.body != "" {
		t.Fatalf("posted %s", got.body)
	}
}
