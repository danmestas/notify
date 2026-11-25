package resend

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/danmestas/notify/internal/providers"
)

// TestClient_Send_Attachments pins Resend's attachments array: filename,
// base64 content and content_type. An email with none carries no key.
func TestClient_Send_Attachments(t *testing.T) {
	srv, got := stubServer(t, http.StatusOK, `{"id":"re_1"}`)
	c := &Client{APIKey: "k", From: "n@example.com", BaseURL: srv.URL}
	_, err := c.Send(context.Background(), providers.EmailMessage{
		To: "u@example.com", Subject: "s", Text: "t",
		Attachments: []providers.Attachment{{Filename: "invoice.pdf", ContentType: "application/pdf", Content: []byte("%PDF")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `"attachments":[{"filename":"invoice.pdf","content":"JVBERg==","content_type":"application/pdf"}]`
	if !strings.Contains(got.body, want) {
		t.Fatalf("body = %s\nwant it to contain %s", got.body, want)
	}

	if _, err := c.Send(context.Background(), providers.EmailMessage{To: "u@example.com", Subject: "s", Text: "t"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.body, "attachments") {
		t.Fatalf("an email with no attachments sent %s", got.body)
	}
}
